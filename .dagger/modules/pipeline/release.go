package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"dagger/pipeline/internal/dagger"
)

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)
var dotVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

func validateVersion(version string) error {
	if !releaseVersion.MatchString(version) {
		return fmt.Errorf("expected vMAJOR.MINOR.PATCH[-prerelease], got %q", version)
	}
	return nil
}
func (m *Pipeline) registry() string {
	return "ghcr.io/" + strings.ToLower(strings.Split(m.Repository, "/")[0])
}
func (m *Pipeline) serverFlags(version string) string {
	if version == "" {
		return ""
	}
	registry := m.registry()
	return "-X github.com/discobox-ai/discobox/server/providers/dockerworker.DefaultSandboxBaseImage=" + registry + "/discobox-sandbox-base:" + version +
		" -X github.com/discobox-ai/discobox/server/providers/dockerworker.DefaultPoolAgentBundle=" + registry + "/discobox-pool-agent-bundle:" + version +
		" -X github.com/discobox-ai/discobox/harness.BundleRegistry=" + registry +
		" -X github.com/discobox-ai/discobox/harness.BundleTag=" + version
}
func linkFlags(version, os, arch string) string {
	flags := "-s -w"
	if os == "linux" {
		loader := "/lib64/ld-linux-x86-64.so.2"
		if arch == "arm64" {
			loader = "/lib/ld-linux-aarch64.so.1"
		}
		flags += " -I " + loader
	}
	if version != "" {
		flags += " -X github.com/discobox-ai/discobox/pkg/version.Version=" + version
	}
	return flags
}

func (m *Pipeline) withHelpers() *dagger.Container {
	ctr := m.work()
	for _, arch := range []string{"amd64", "arm64"} {
		relay := "/tmp/discobox-cp-relay.linux-" + arch
		clip := "/tmp/discobox-clipwatch.windows-" + arch + ".exe"
		ctr = ctr.WithEnvVariable("CGO_ENABLED", "0").WithEnvVariable("GOARCH", arch).
			WithEnvVariable("GOOS", "linux").WithWorkdir("/src/discobox/pool-agent").
			WithExec([]string{"go", "build", "-trimpath", "-ldflags=-s -w", "-o", relay, "./cmd/discobox-cp-relay"}).
			WithEnvVariable("GOOS", "windows").WithWorkdir("/src/discobox/cli").
			WithExec([]string{"go", "build", "-trimpath", "-ldflags=-s -w", "-o", clip, "./cmd/discobox-clipwatch"}).
			WithoutEnvVariable("GOOS").WithoutEnvVariable("GOARCH").WithoutEnvVariable("CGO_ENABLED").WithWorkdir("/src/discobox").
			WithExec([]string{"go", "run", "./internal/cmd/discobox-gzip", relay, "server/providers/wslc/relay/artifacts/discobox-cp-relay.linux-" + arch + ".gz"}).
			WithExec([]string{"go", "run", "./internal/cmd/discobox-gzip", clip, "cli/internal/clipwatch/artifacts/discobox-clipwatch.windows-" + arch + ".exe.gz"}).
			WithExec([]string{"bash", "-ec", `for helper in "$@"; do test -f "$helper" && test "$(wc -c < "$helper")" -gt 65536 || { echo "missing or invalid embedded helper: $helper" >&2; exit 1; }; done`, "verify-helpers",
				"server/providers/wslc/relay/artifacts/discobox-cp-relay.linux-" + arch + ".gz", "cli/internal/clipwatch/artifacts/discobox-clipwatch.windows-" + arch + ".exe.gz"})
	}
	return ctr
}

type buildReceipt struct {
	Version    string            `json:"version"`
	Repository string            `json:"repository"`
	Dirty      bool              `json:"dirty,omitempty"`
	Checks     map[string]bool   `json:"checks,omitempty"`
	Revision   string            `json:"revision"`
	Platform   string            `json:"platform"`
	Status     string            `json:"status"`
	Digests    map[string]string `json:"digests"`
}

func receiptFile(platform string) string {
	return "receipts/" + strings.ReplaceAll(platform, "/", "-") + ".json"
}

// Build portable release binaries. macOS artifacts are produced externally.
// Empty version builds CI artifacts; tagged builds embed the release manifests.
func (m *Pipeline) ReleaseBuild(ctx context.Context,
	// +default=""
	version string,
	// +default="all"
	targetOs string,
	// +default="all"
	targetArch string,
) (*dagger.Directory, error) {
	if version != "" {
		if err := validateVersion(version); err != nil {
			return nil, err
		}
	}
	if targetOs == "darwin" {
		return nil, fmt.Errorf("darwin builds run natively: go tool task native:macos:build; supply their artifacts to release assembly")
	}
	if targetOs != "all" && targetOs != "linux" && targetOs != "windows" {
		return nil, fmt.Errorf("unsupported build OS %q", targetOs)
	}
	if targetArch != "all" && targetArch != "amd64" && targetArch != "arm64" {
		return nil, fmt.Errorf("unsupported build architecture %q", targetArch)
	}
	targets := [][2]string{}
	for _, os := range []string{"linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			if (targetOs == "all" || targetOs == os) && (targetArch == "all" || targetArch == arch) {
				targets = append(targets, [2]string{os, arch})
			}
		}
	}
	base := m.withHelpers()
	results := make([]*dagger.Directory, len(targets))
	jobs := make([]func() error, len(targets))
	for i, target := range targets {
		jobs[i] = func() error {
			var err error
			results[i], err = m.binary(ctx, base, version, target[0], target[1])
			return err
		}
	}
	if err := parallel(jobs...); err != nil {
		return nil, err
	}
	out := dag.Directory()
	for _, result := range results {
		out = out.WithDirectory(".", result)
	}
	if targetOs == "all" {
		out = out.WithFile("native/macos.json", m.Macos()).WithFile("native/windows.json", m.Windows())
	}
	return out, nil
}

func (m *Pipeline) binary(ctx context.Context, base *dagger.Container, version, os, arch string) (*dagger.Directory, error) {
	ext := ""
	if os == "windows" {
		ext = ".exe"
	}
	cli := "discobox-" + os + "-" + arch + ext
	server := "discobox-server-" + os + "-" + arch + ext
	flags := linkFlags(version, os, arch)
	sf := m.serverFlags(version)
	ctr := base
	if os == "windows" {
		for _, binary := range []struct{ module, name, description string }{{"server", "discobox-server.exe", "Discobox server"}, {"cli", "discobox.exe", "Discobox CLI"}} {
			ctr = ctr.WithExec([]string{"go", "run", "./internal/cmd/discobox-winres", "write", "-version", version, "-arch", arch, "-name", binary.name, "-description", binary.description, "-out", binary.module + "/cmd/" + strings.TrimSuffix(binary.name, ".exe") + "/zz_version_windows_" + arch + ".syso"})
		}
	}
	target := ctr.WithEnvVariable("GOOS", os).WithEnvVariable("GOARCH", arch).WithEnvVariable("CGO_ENABLED", "0")
	target = target.WithWorkdir("/src/discobox/server").WithExec([]string{"go", "build", "-trimpath", "-ldflags", flags + " " + sf, "-o", "/out/" + server, "./cmd/discobox-server"})
	// Helpers run on the engine's native platform, even for foreign targets.
	ctr = target.WithoutEnvVariable("GOOS").WithoutEnvVariable("GOARCH").WithoutEnvVariable("CGO_ENABLED").WithWorkdir("/src/discobox")
	manifest := ""
	if version != "" {
		args := []string{"go", "run", "./internal/cmd/discobox-server-manifest", "-version", version, "-os", os, "-arch", arch, "-command", "discobox-server" + ext, "-asset", "discobox-server" + ext + "=/out/" + server}
		if m.Repository == "discobox-ai/discobox" {
			args = append(args, "-base-url", "https://assets.discobox.ai/discobox/"+version)
		}
		args = append(args, "-base-url", "https://github.com/"+m.Repository+"/releases/download/"+version)
		var err error
		manifest, err = ctr.WithExec(args).Stdout(ctx)
		if err != nil {
			return nil, err
		}
		manifest = strings.TrimSpace(manifest)
		ctr = ctr.WithExec([]string{"go", "run", "-ldflags", sf + " -X github.com/discobox-ai/discobox/pkg/version.Version=" + version, "./server/cmd/discobox-server", "manifest"}, dagger.ContainerWithExecOpts{RedirectStdout: "/tmp/images.json"})
		ctr = ctr.WithEnvVariable("GOOS", os).WithEnvVariable("GOARCH", arch).WithEnvVariable("CGO_ENABLED", "0").WithWorkdir("/src/discobox/cli").
			WithExec([]string{"go", "build", "-trimpath", "-ldflags", flags + " -X github.com/discobox-ai/discobox/cli/internal/cli.serverAutoLaunch=true -X github.com/discobox-ai/discobox/pkg/serverstage.DefaultManifest=" + manifest, "-o", "/out/" + cli, "./cmd/discobox"}).
			WithoutEnvVariable("GOOS").WithoutEnvVariable("GOARCH").WithoutEnvVariable("CGO_ENABLED").WithWorkdir("/src/discobox")
		args = append(args, "-revision", m.Revision, "-client-asset", "discobox"+ext+"=/out/"+cli, "-release-images", "/tmp/images.json", "-release-output", "/out/discobox-manifest-"+os+"-"+arch+".json")
		ctr = ctr.WithExec(args)
	} else {
		ctr = ctr.WithEnvVariable("GOOS", os).WithEnvVariable("GOARCH", arch).WithEnvVariable("CGO_ENABLED", "0").WithWorkdir("/src/discobox/cli").
			WithExec([]string{"go", "build", "-trimpath", "-ldflags", flags + " -X github.com/discobox-ai/discobox/cli/internal/cli.serverAutoLaunch=true", "-o", "/out/" + cli, "./cmd/discobox"}).
			WithoutEnvVariable("GOOS").WithoutEnvVariable("GOARCH").WithoutEnvVariable("CGO_ENABLED").WithWorkdir("/src/discobox")
	}
	if os == "windows" {
		for _, name := range []string{cli, server} {
			ctr = ctr.WithExec([]string{"go", "run", "./internal/cmd/discobox-winres", "verify", "-version", version, "/out/" + name})
		}
		ctr = ctr.WithFile("/zip/discobox.exe", ctr.File("/out/"+cli)).WithWorkdir("/zip").WithExec([]string{"zip", "-q", "/out/discobox-windows-" + arch + ".zip", "discobox.exe"}).WithWorkdir("/src/discobox")
	} else {
		for _, name := range []string{cli, server} {
			ctr = ctr.WithExec([]string{"bash", "-ec", `interp=$(readelf -p .interp "$1" 2>/dev/null | sed -n 's/.*\[ *0\] *//p' | tr -d ' '); case "$interp" in ''|/lib/*|/lib64/*) ;; *) echo "invalid ELF interpreter: $interp" >&2; exit 1;; esac`, "verify-interpreter", "/out/" + name})
		}
	}
	hostArch, err := m.Toolchain().WithExec([]string{"go", "env", "GOARCH"}).Stdout(ctx)
	if err != nil {
		return nil, err
	}
	if os == "linux" && arch == strings.TrimSpace(hostArch) {
		for _, name := range []string{cli, server} {
			if version == "" {
				ctr = ctr.WithExec([]string{"/out/" + name, "--version"})
			} else {
				ctr = ctr.WithExec([]string{"bash", "-ec", `got=$("$1" --version); printf '%s\n' "$got"; [[ "$got" == *"$2"* ]]`, "verify-version", "/out/" + name, version})
			}
		}
		if version != "" {
			ctr = ctr.WithExec([]string{"/out/" + cli, "admin", "server", "manifest"})
		}
	}
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	checksums, err := ctr.WithExec([]string{"bash", "-ec", "cd /out && sha256sum *"}).Stdout(ctx)
	if err != nil {
		return nil, err
	}
	digests := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid artifact checksum output")
		}
		digests[fields[1]] = fields[0]
	}
	data, _ := json.MarshalIndent(buildReceipt{Version: version, Dirty: m.Dirty, Repository: m.Repository, Revision: m.Revision, Platform: os + "/" + arch, Status: "built", Digests: digests}, "", "  ")
	return ctr.Directory("/out").WithNewFile(receiptFile(os+"/"+arch), string(data)+"\n"), nil
}

// Assemble a complete, verified release. Missing native artifacts fail closed.
func (m *Pipeline) ReleaseAssemble(ctx context.Context, version string, artifacts *dagger.Directory) (*dagger.Directory, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	platforms := []string{"linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64", "darwin/arm64"}
	receipts, err := artifacts.Glob(ctx, "receipts/*.json")
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, name := range receipts {
		present[name] = true
	}
	for _, platform := range platforms {
		if !present[receiptFile(platform)] {
			return nil, fmt.Errorf("release incomplete: %s receipt is missing; supply native macOS artifacts", platform)
		}
	}
	for _, platform := range platforms {
		raw, err := artifacts.File(receiptFile(platform)).Contents(ctx)
		if err != nil {
			return nil, fmt.Errorf("release incomplete: %s receipt is missing; supply native macOS artifacts: %w", platform, err)
		}
		var receipt buildReceipt
		if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
			return nil, err
		}
		if err := receipt.validate(version, m.Revision, m.Repository, platform); err != nil {
			return nil, err
		}
		if len(receipt.Digests) < 3 {
			return nil, fmt.Errorf("receipt for %s has no complete artifact digest inventory", platform)
		}
		parts := strings.Split(platform, "/")
		suffix := parts[0] + "-" + parts[1]
		ext := ""
		if parts[0] == "windows" {
			ext = ".exe"
		}
		requiredAssets := []string{"discobox-" + suffix + ext, "discobox-server-" + suffix + ext, "discobox-manifest-" + suffix + ".json"}
		if parts[0] == "windows" {
			requiredAssets = append(requiredAssets, "discobox-"+suffix+".zip")
		}
		for _, required := range requiredAssets {
			if receipt.Digests[required] == "" {
				return nil, fmt.Errorf("receipt for %s omits %s", platform, required)
			}
		}
		var sums strings.Builder
		for name, digest := range receipt.Digests {
			if strings.ContainsAny(name, "/\\\n") || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
				return nil, fmt.Errorf("invalid receipt asset %q", name)
			}
			fmt.Fprintf(&sums, "%s  %s\n", digest, name)
		}
		if err := run(ctx, m.Toolchain().WithDirectory("/artifacts", artifacts).WithNewFile("/receipt.sha256", sums.String()).WithWorkdir("/artifacts"), "sha256sum", "-c", "/receipt.sha256"); err != nil {
			return nil, fmt.Errorf("%s: %w", platform, err)
		}
	}
	return m.assembleAvailable(ctx, version, artifacts)
}

// Build actual installers/manifests from the supplied platforms. Production
// calls this only after completeness validation; preview explicitly stays partial.
func (m *Pipeline) assembleAvailable(ctx context.Context, version string, artifacts *dagger.Directory) (*dagger.Directory, error) {
	entries, err := artifacts.Entries(ctx)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, name := range entries {
		present[name] = true
	}
	ctr := m.work().WithDirectory("/artifacts", artifacts).WithExec([]string{"bash", "-ec", "go run ./internal/cmd/discobox-release-manifest -out /artifacts/release.json /artifacts/discobox-manifest-*.json"})
	args := []string{"go", "run", "./internal/cmd/discobox-installers", "-version", version, "-out", "/artifacts"}
	for _, platform := range []string{"linux-amd64", "linux-arm64", "windows-amd64.exe", "windows-arm64.exe", "darwin-arm64"} {
		name := "discobox-" + platform
		if present[name] {
			args = append(args, "/artifacts/"+name)
		}
	}
	ctr = ctr.WithExec(args).WithExec([]string{"bash", "-ec", `cd /artifacts; find . -maxdepth 1 -type f ! -name SHA256SUMS -printf '%f\n' | sort | xargs sha256sum > SHA256SUMS; sha256sum -c SHA256SUMS`})
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return ctr.Directory("/artifacts").WithoutDirectory("native").WithoutDirectory("receipts"), nil
}

// Portable helpers consumed by the native Mac build, exported in source layout.
func (m *Pipeline) NativeInputs(ctx context.Context) (*dagger.Directory, error) {
	ctr := m.withHelpers()
	paths := []string{"server/providers/wslc/relay/artifacts/discobox-cp-relay.linux-amd64.gz", "server/providers/wslc/relay/artifacts/discobox-cp-relay.linux-arm64.gz", "cli/internal/clipwatch/artifacts/discobox-clipwatch.windows-amd64.exe.gz", "cli/internal/clipwatch/artifacts/discobox-clipwatch.windows-arm64.exe.gz"}
	sums, err := ctr.WithExec(append([]string{"sha256sum"}, paths...)).Stdout(ctx)
	if err != nil {
		return nil, err
	}
	digests := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid helper checksum output")
		}
		digests[fields[1]] = fields[0]
	}
	data, err := json.MarshalIndent(buildReceipt{Revision: m.Revision, Dirty: m.Dirty, Repository: m.Repository, Platform: "native-inputs", Status: "built", Digests: digests}, "", "  ")
	if err != nil {
		return nil, err
	}
	out := dag.Directory().WithNewFile("inputs.json", string(data)+"\n")
	for _, name := range paths {
		out = out.WithFile(name, ctr.File("/src/discobox/"+name))
	}
	return out, nil
}

func (r buildReceipt) validate(version, revision, repository, platform string) error {
	if r.Version != version || r.Revision != revision || r.Repository != repository || r.Platform != platform || r.Status != "built" || r.Dirty {
		return fmt.Errorf("mismatched or unverified receipt for %s", platform)
	}
	if platform == "darwin/arm64" {
		for _, check := range []string{"portable-inputs", "codesign-server", "codesign-cli", "virtualization-entitlement", "smoke"} {
			if !r.Checks[check] {
				return fmt.Errorf("macOS receipt lacks successful %s verification", check)
			}
		}
	}
	return nil
}

// Only the named Mac files cross this boundary; native input cannot overwrite
// portable executables, installers, or another platform's receipts.
func (m *Pipeline) mergeNative(ctx context.Context, version string, portable, nativeArtifacts *dagger.Directory) (*dagger.Directory, error) {
	raw, err := nativeArtifacts.File(receiptFile("darwin/arm64")).Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("missing native macOS receipt: %w", err)
	}
	var receipt buildReceipt
	if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
		return nil, err
	}
	if err = receipt.validate(version, m.Revision, m.Repository, "darwin/arm64"); err != nil {
		return nil, err
	}
	for _, name := range []string{"discobox-darwin-arm64", "discobox-server-darwin-arm64", "discobox-manifest-darwin-arm64.json", receiptFile("darwin/arm64")} {
		if strings.HasPrefix(name, "discobox-") && !strings.HasSuffix(name, ".json") {
			portable = portable.WithFile(name, nativeArtifacts.File(name), dagger.DirectoryWithFileOpts{Permissions: 0755})
		} else {
			portable = portable.WithFile(name, nativeArtifacts.File(name))
		}
	}
	return portable, nil
}
