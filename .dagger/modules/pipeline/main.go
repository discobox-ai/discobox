// Discobox's CI and release graph. GitHub Actions is an optional caller.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"dagger/pipeline/internal/dagger"
)

type Pipeline struct {
	// +private
	Source *dagger.Directory
	// +private
	Revision string
	// +private
	Dirty bool
	// Release destination, also used to construct download URLs.
	Repository string
}

func New(ctx context.Context, ws *dagger.Workspace,
	// +default="discobox-ai/discobox"
	repository string,
) (*Pipeline, error) {
	revision, err := ws.Git().Head().CommitSHA(ctx)
	if err != nil {
		return nil, err
	}
	clean, err := ws.Git().Uncommitted().IsEmpty(ctx)
	if err != nil {
		return nil, err
	}
	source := ws.Directory("/", dagger.WorkspaceDirectoryOpts{Exclude: []string{
		".git", ".dagger", "build", ".tmp", ".direnv", "**/node_modules", ".pnpm-store", ".env", ".env.*", ".claude/worktrees", ".idea", ".devenv*",
		".discobox/cache", ".discobox/config", ".discobox/data", ".discobox/state", "**/*.pem", "**/*.key",
	}})
	source, err = source.Sync(ctx)
	if err != nil {
		return nil, err
	}
	return &Pipeline{Source: source, Revision: revision, Dirty: !clean, Repository: repository}, nil
}

// Toolchain for every Linux check and release helper. No host toolchain is used.
func (m *Pipeline) Toolchain() *dagger.Container {
	node := dag.Container().From("node:24.18.0-bookworm-slim")
	pwsh := dag.Container().From("mcr.microsoft.com/powershell:7.5-debian-12")
	return dag.Container().From("golang:1.27.1").
		WithExec([]string{"sh", "-ec", "apt-get update && apt-get install -y --no-install-recommends git gh curl ca-certificates jq shellcheck bats zip unzip binutils e2fsprogs libicu-dev && rm -rf /var/lib/apt/lists/*"}).
		WithDirectory("/usr/local", node.Directory("/usr/local")).
		WithDirectory("/opt/microsoft/powershell/7", pwsh.Directory("/opt/microsoft/powershell/7")).
		WithSymlink("/opt/microsoft/powershell/7/pwsh", "/usr/local/bin/pwsh").
		WithEnvVariable("NODE_USE_SYSTEM_CA", "1").
		WithExec([]string{"npm", "install", "--global", "pnpm@11.9.0"}).
		WithFile("/pipeline/install-dagger.sh", dag.CurrentModule().Source().File("install-dagger.sh")).
		WithFile("/pipeline/dagger-cli-checksums.txt", dag.CurrentModule().Source().File("dagger-cli-checksums.txt")).
		WithExec([]string{"bash", "/pipeline/install-dagger.sh"}).
		WithEnvVariable("GOFLAGS", "-buildvcs=false -p=4").
		WithEnvVariable("GOMAXPROCS", "4").
		WithEnvVariable("TERM", "xterm-256color")
}

func (m *Pipeline) work() *dagger.Container {
	return m.Toolchain().WithMountedCache("/go/pkg/mod", dag.CacheVolume("discobox-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("discobox-go-build")).
		WithMountedCache("/root/.cache/golangci-lint", dag.CacheVolume("discobox-lint")).
		WithDirectory("/src/discobox", m.Source).WithWorkdir("/src/discobox").
		WithExec([]string{"sh", "-ec", `git init -q; git add .; git -c user.name='Discobox pipeline' -c user.email=pipeline@discobox.ai -c commit.gpgsign=false commit -qm snapshot`})
}

func (m *Pipeline) modules(ctx context.Context) ([]string, error) {
	raw, err := m.work().WithExec([]string{"go", "work", "edit", "-json"}).Stdout(ctx)
	if err != nil {
		return nil, err
	}
	var work struct{ Use []struct{ DiskPath string } }
	if err := json.Unmarshal([]byte(raw), &work); err != nil {
		return nil, err
	}
	var modules []string
	for _, mod := range work.Use {
		modules = append(modules, mod.DiskPath)
	}
	return modules, nil
}
func packages(modules []string) []string {
	result := make([]string, len(modules))
	for i, mod := range modules {
		result[i] = strings.TrimSuffix(mod, "/") + "/..."
	}
	return result
}
func run(ctx context.Context, ctr *dagger.Container, args ...string) error {
	_, err := ctr.WithExec(args).Sync(ctx)
	return err
}
func parallel(jobs ...func() error) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(jobs))
	// Bound each fan-out so local CI does not launch every cross-build/linter
	// at once. Nested groups use their own budget and cannot deadlock.
	slots := make(chan struct{}, 2)
	for _, job := range jobs {
		wg.Add(1)
		go func(job func() error) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if err := job(); err != nil {
				errs <- err
			}
		}(job)
	}
	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// Run every application's Go tests on Linux, using the modules in go.work.
// +check
func (m *Pipeline) Tests(ctx context.Context) error {
	mods, err := m.modules(ctx)
	if err != nil {
		return err
	}
	return run(ctx, m.work(), append([]string{"go", "test"}, packages(mods)...)...)
}

// Lint, cross-typecheck, deadcode, shellcheck, and validate Dockerfile inputs.
// +check
func (m *Pipeline) Static(ctx context.Context) error {
	mods, err := m.modules(ctx)
	if err != nil {
		return err
	}
	lint := m.work().WithExec([]string{"go", "build", "-o", "/usr/local/bin/golangci-lint", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"})
	jobs := []func() error{
		func() error {
			return run(ctx, m.work().WithEnvVariable("GOOS", "windows").WithEnvVariable("GOARCH", "amd64").WithEnvVariable("CGO_ENABLED", "0"), append([]string{"go", "vet"}, packages(mods)...)...)
		},
		func() error {
			return run(ctx, m.work().WithExec([]string{"go", "build", "-o", "/usr/local/bin/deadcode", "golang.org/x/tools/cmd/deadcode"}), "scripts/deadcode.sh", "/usr/local/bin/deadcode")
		},
		func() error { return run(ctx, m.work(), "scripts/shellcheck.sh") },
		func() error { return m.Dockerfiles(ctx) },
	}
	for _, mod := range mods {
		jobs = append(jobs, func() error { return run(ctx, lint.WithWorkdir("/src/discobox/"+mod), "golangci-lint", "run") })
	}
	return parallel(jobs...)
}

// Check Dockerfile COPY paths and imported Go packages without building images.
func (m *Pipeline) Dockerfiles(ctx context.Context) error {
	helper := dag.CurrentModule().Source().File("dockerfiles.sh")
	return run(ctx, m.work().WithFile("/pipeline/dockerfiles.sh", helper), "bash", "/pipeline/dockerfiles.sh")
}

// Verify formatting, tidy, generated files, and Mermaid without mutating the host.
// +check
func (m *Pipeline) Verify(ctx context.Context) error {
	ctr := m.work()
	return parallel(
		func() error {
			return run(ctx, ctr, "bash", "-ec", `files=$(git ls-files -z '*.go' | xargs -0 gofmt -l); test -z "$files" || { printf '%s\n' "$files"; exit 1; }`)
		},
		func() error {
			return run(ctx, ctr, "bash", "-ec", `scripts/go-mod-tidy.sh; git diff --exit-code; test -z "$(git ls-files --others --exclude-standard)"`)
		},
		func() error {
			return run(ctx, m.harnessSettingsContainer(), "bash", "-ec", `mapfile -t modules < <(go work edit -json | jq -r '.Use[].DiskPath'); for mod in "${modules[@]}"; do (cd "$mod" && go generate ./...); done; git diff --exit-code; test -z "$(git ls-files --others --exclude-standard)"`)
		},
		func() error {
			return run(ctx, ctr.WithMountedCache("/root/.local/share/pnpm/store", dag.CacheVolume("discobox-pnpm")), "bash", "-ec", "pnpm install --frozen-lockfile && pnpm run check:mermaid")
		},
	)
}

// Compile and test the pipeline itself, outside the application's go.work.
// +check
func (m *Pipeline) ModuleTests(ctx context.Context) error {
	ctr := m.Toolchain().WithEnvVariable("GOWORK", "off").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("discobox-pipeline-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("discobox-pipeline-build")).
		WithDirectory("/pipeline", dag.CurrentModule().Source()).WithWorkdir("/pipeline")
	if err := run(ctx, ctr, "bash", "-ec", "shellcheck *.sh"); err != nil {
		return err
	}
	if err := run(ctx, ctr, "dagger", "api", "with-session", "go", "test", "./..."); err != nil {
		return err
	}
	// CI must enforce SDK staleness too, not just compile whatever bindings
	// happen to exist. Only the SDK's check runs in this nested workspace.
	bindings := m.work().WithDirectory("/src/discobox/.dagger/modules/pipeline", dag.CurrentModule().Source())
	return run(ctx, bindings, "dagger", "check", "--module=go-sdk", "--progress=plain")
}

type nativeReport struct {
	Platform   string   `json:"platform"`
	Status     string   `json:"status"`
	Operations []string `json:"operations"`
}

func native(platform string) nativeReport {
	ops := []string{"go tool task native:windows", "go tool task native:smoke -- -output <Dagger artifacts>"}
	if platform == "darwin/arm64" {
		ops = []string{"go tool task native:macos", "native tests and Darwin lint/vet", "build with macOS SDK; sign final server before CLI manifest", "verify signatures, Virtualization entitlement, version and server manifest"}
	}
	return nativeReport{platform, "external: execute the native repository command on its operating system", ops}
}

// Native macOS command plan. Execution occurs outside the Linux engine.
func (m *Pipeline) Macos() *dagger.File {
	data, _ := json.MarshalIndent(native("darwin/arm64"), "", "  ")
	return dag.Directory().WithNewFile("macos.json", string(data)+"\n").File("macos.json")
}

// Native Windows command plan. Execution occurs outside the Linux engine.
func (m *Pipeline) Windows() *dagger.File {
	data, _ := json.MarshalIndent(native("windows/amd64"), "", "  ")
	return dag.Directory().WithNewFile("windows.json", string(data)+"\n").File("windows.json")
}

// Run Linux CI and release builds, and export native command plans and portable helpers.
func (m *Pipeline) Ci(ctx context.Context) (*dagger.Directory, error) {
	if err := parallel(func() error { return m.Tests(ctx) }, func() error { return m.Static(ctx) }, func() error { return m.Verify(ctx) }, func() error { return m.ModuleTests(ctx) }); err != nil {
		return nil, err
	}
	out, err := m.ReleaseBuild(ctx, "", "all", "all")
	if err != nil {
		return nil, err
	}
	trust, err := m.TrustCheck(ctx)
	if err != nil {
		return nil, err
	}
	inputs, err := m.NativeInputs(ctx)
	if err != nil {
		return nil, err
	}
	return out.WithFile("native/macos.json", m.Macos()).WithFile("native/windows.json", m.Windows()).WithFile("trust.json", trust).WithDirectory("native-inputs", inputs), nil
}

// Check repository shell scripts using the same toolchain as CI.
func (m *Pipeline) Shell(ctx context.Context) error {
	return run(ctx, m.work(), "scripts/shellcheck.sh")
}

// Validate all repository Mermaid diagrams.
func (m *Pipeline) Mermaid(ctx context.Context) error {
	return run(ctx, m.work().WithMountedCache("/root/.local/share/pnpm/store", dag.CacheVolume("discobox-pnpm")), "bash", "-ec", "pnpm install --frozen-lockfile && pnpm run check:mermaid")
}
