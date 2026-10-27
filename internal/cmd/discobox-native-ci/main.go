// Native-only CI and release operations. Portable builds and publication belong
// to Dagger; this command runs unchanged on a developer machine and in Actions.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type receipt struct {
	Version    string            `json:"version"`
	Revision   string            `json:"revision"`
	Repository string            `json:"repository"`
	Dirty      bool              `json:"dirty,omitempty"`
	Platform   string            `json:"platform"`
	Status     string            `json:"status"`
	Digests    map[string]string `json:"digests"`
	Checks     map[string]bool   `json:"checks,omitempty"`
}

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$`)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func command(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	//nolint:gosec // Commands are repository build tools or explicitly selected local smoke artifacts; no shell is invoked.
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	c.Stderr = os.Stderr
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
func execute(ctx context.Context, args ...string) error {
	//nolint:gosec // Repository-owned native build/test commands; arguments are passed directly without a shell.
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return nil
}
func packages(ctx context.Context) ([]string, error) {
	out, err := command(ctx, "", nil, "go", "work", "edit", "-json")
	if err != nil {
		return nil, err
	}
	var work struct{ Use []struct{ DiskPath string } }
	if err = json.Unmarshal([]byte(out), &work); err != nil {
		return nil, err
	}
	var result []string
	for _, m := range work.Use {
		result = append(result, strings.TrimSuffix(m.DiskPath, "/")+"/...")
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty workspace")
	}
	return result, nil
}
func run(ctx context.Context) error {
	mode := flag.String("mode", "check", "check, build, or smoke")
	version := flag.String("version", "", "release version; empty for CI")
	repository := flag.String("repository", "discobox-ai/discobox", "release repository")
	output := flag.String("output", "build/native", "artifact directory")
	inputs := flag.String("inputs", "build/native-inputs", "portable helper artifacts exported by Dagger")
	flag.Parse()
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return fmt.Errorf("native CI requires macOS or Windows; got %s", runtime.GOOS)
	}
	if *version != "" && !versionPattern.MatchString(*version) {
		return fmt.Errorf("invalid release version %q", *version)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(*repository) {
		return fmt.Errorf("invalid repository")
	}
	if runtime.GOOS == "darwin" {
		if err := os.Setenv("CGO_ENABLED", "1"); err != nil {
			return err
		}
	}
	if *mode == "smoke" {
		return smoke(ctx, *output, *version)
	}
	if *mode != "check" && *mode != "build" {
		return fmt.Errorf("unknown mode %q", *mode)
	}
	if *mode == "check" {
		pkgs, err := packages(ctx)
		if err != nil {
			return err
		}
		if err = execute(ctx, append([]string{"go", "test"}, pkgs...)...); err != nil {
			return err
		}
		if runtime.GOOS == "windows" {
			return nil
		}
		if err = execute(ctx, append([]string{"go", "vet"}, pkgs...)...); err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "discobox-native-lint-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		lint := filepath.Join(dir, "golangci-lint")
		if err = execute(ctx, "go", "build", "-o", lint, "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"); err != nil {
			return err
		}
		if err = execute(ctx, "bash", "scripts/lint-darwin.sh", lint); err != nil {
			return err
		}
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return fmt.Errorf("native release builds require darwin/arm64")
	}
	return build(ctx, *version, *repository, *output, *inputs)
}
func build(ctx context.Context, version, repository, output, inputs string) error {
	revision, err := command(ctx, "", nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	revision = strings.TrimSpace(revision)
	dirty, err := command(ctx, "", nil, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if version != "" {
		if dirty != "" {
			return fmt.Errorf("tagged native build requires a clean checkout")
		}
		tag, err := command(ctx, "", nil, "git", "rev-parse", version+"^{commit}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(tag) != revision {
			return fmt.Errorf("release tag does not point to checkout")
		}
	}
	// These are Linux/Windows cross-builds; validate their original source and
	// bytes before modifying the native checkout's ignored embed directories.
	inputReceipt, payloads, err := loadPortableInputs(inputs, revision, repository, version != "")
	if err != nil {
		return err
	}
	for name, body := range payloads {
		if err = os.WriteFile(filepath.FromSlash(name), body, 0600); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(filepath.Join(output, "receipts"), 0750); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(output, "receipts/darwin-arm64.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if version == "" {
		if err = os.Remove(filepath.Join(output, "discobox-manifest-darwin-arm64.json")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	server := filepath.Join(abs, "discobox-server-darwin-arm64")
	cli := filepath.Join(abs, "discobox-darwin-arm64")
	flags := "-s -w"
	sf := ""
	if version != "" {
		flags += " -X github.com/discobox-ai/discobox/pkg/version.Version=" + version
		registry := "ghcr.io/" + strings.ToLower(strings.Split(repository, "/")[0])
		sf = " -X github.com/discobox-ai/discobox/server/providers/dockerworker.DefaultSandboxBaseImage=" + registry + "/discobox-sandbox-base:" + version + " -X github.com/discobox-ai/discobox/server/providers/dockerworker.DefaultPoolAgentBundle=" + registry + "/discobox-pool-agent-bundle:" + version + " -X github.com/discobox-ai/discobox/harness.BundleRegistry=" + registry + " -X github.com/discobox-ai/discobox/harness.BundleTag=" + version
	}
	env := []string{"CGO_ENABLED=1", "GOOS=darwin", "GOARCH=arm64", "GOFLAGS=-buildvcs=false"}
	if _, err = command(ctx, "server", env, "go", "build", "-trimpath", "-ldflags", flags+sf, "-o", server, "./cmd/discobox-server"); err != nil {
		return err
	}
	if err = execute(ctx, "codesign", "--force", "--sign", "-", "--entitlements", "server/providers/vz/vz.entitlements", server); err != nil {
		return err
	}
	if err = execute(ctx, "codesign", "--verify", "--strict", server); err != nil {
		return err
	}
	entitlements, err := command(ctx, "", nil, "codesign", "--display", "--entitlements", ":-", server)
	if err != nil {
		return err
	}
	if !virtualizationEntitlement([]byte(entitlements)) {
		return fmt.Errorf("signed server lacks Virtualization entitlement")
	}
	args := []string{"go", "run", "./internal/cmd/discobox-server-manifest", "-version", version, "-os", "darwin", "-arch", "arm64", "-command", "discobox-server", "-asset", "discobox-server=" + server}
	manifest := ""
	images := filepath.Join(abs, "images.json")
	if version != "" {
		if repository == "discobox-ai/discobox" {
			args = append(args, "-base-url", "https://assets.discobox.ai/discobox/"+version)
		}
		args = append(args, "-base-url", "https://github.com/"+repository+"/releases/download/"+version)
		manifest, err = command(ctx, "", nil, args...)
		if err != nil {
			return err
		}
		imageManifest, err := command(ctx, "", nil, "go", "run", "-ldflags", strings.TrimSpace(sf)+" -X github.com/discobox-ai/discobox/pkg/version.Version="+version, "./server/cmd/discobox-server", "manifest")
		if err != nil {
			return err
		}
		if err = os.WriteFile(images, []byte(imageManifest), 0600); err != nil {
			return err
		}
	}
	cliFlags := flags + " -X github.com/discobox-ai/discobox/cli/internal/cli.serverAutoLaunch=true"
	if version != "" {
		cliFlags += " -X github.com/discobox-ai/discobox/pkg/serverstage.DefaultManifest=" + strings.TrimSpace(manifest)
	}
	if _, err = command(ctx, "cli", env, "go", "build", "-trimpath", "-ldflags", cliFlags, "-o", cli, "./cmd/discobox"); err != nil {
		return err
	}
	if err = execute(ctx, "codesign", "--force", "--sign", "-", cli); err != nil {
		return err
	}
	if err = execute(ctx, "codesign", "--verify", "--strict", cli); err != nil {
		return err
	}
	names := []string{"discobox-server-darwin-arm64", "discobox-darwin-arm64"}
	if version != "" {
		name := "discobox-manifest-darwin-arm64.json"
		args = append(args, "-revision", revision, "-client-asset", "discobox="+cli, "-release-images", images, "-release-output", filepath.Join(abs, name))
		if _, err = command(ctx, "", nil, args...); err != nil {
			return err
		}
		names = append(names, name)
		if err = os.Remove(images); err != nil {
			return err
		}
	}
	if err = smoke(ctx, abs, version); err != nil {
		return err
	}
	if version != "" {
		finalDirty, err := command(ctx, "", nil, "git", "status", "--porcelain", "--untracked-files=normal")
		if err != nil {
			return err
		}
		finalRevision, err := command(ctx, "", nil, "git", "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if finalDirty != "" || strings.TrimSpace(finalRevision) != revision {
			return fmt.Errorf("source changed during native release build")
		}
	}
	r := receipt{Version: version, Dirty: dirty != "" || inputReceipt.Dirty, Revision: revision, Repository: repository, Platform: "darwin/arm64", Status: "built", Digests: map[string]string{}, Checks: map[string]bool{"portable-inputs": true, "codesign-server": true, "codesign-cli": true, "virtualization-entitlement": true, "smoke": true}}
	for _, name := range names {
		sum, err := digest(filepath.Join(abs, name))
		if err != nil {
			return err
		}
		r.Digests[name] = sum
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(abs, "receipts/darwin-arm64.json"), append(data, '\n'), 0600)
}
func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func virtualizationEntitlement(data []byte) bool {
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	wanted := false
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		switch v := token.(type) {
		case xml.StartElement:
			if v.Name.Local == "key" {
				var key string
				if decoder.DecodeElement(&key, &v) != nil {
					return false
				}
				wanted = key == "com.apple.security.virtualization"
			} else if wanted {
				if v.Name.Local != "true" {
					return false
				}
				var value struct{}
				return decoder.DecodeElement(&value, &v) == nil
			}
		}
	}
}
func smoke(ctx context.Context, dir, version string) error {
	suffix := "-" + runtime.GOOS + "-" + runtime.GOARCH
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for _, prefix := range []string{"discobox", "discobox-server"} {
		path, err := filepath.Abs(filepath.Join(dir, prefix+suffix+ext))
		if err != nil {
			return err
		}
		out, err := command(ctx, "", nil, path, "--version")
		if err != nil {
			return err
		}
		if version != "" && !strings.Contains(out, version) {
			return fmt.Errorf("%s reports incorrect version %s", path, out)
		}
		fmt.Print(out)
		if prefix == "discobox" && version != "" {
			out, err = command(ctx, "", nil, path, "admin", "server", "manifest")
			if err != nil {
				return err
			}
			if !strings.Contains(out, "discobox-server"+suffix+ext) {
				return fmt.Errorf("CLI manifest names wrong server")
			}
		}
	}
	return nil
}

func helperPaths() []string {
	return []string{"server/providers/wslc/relay/artifacts/discobox-cp-relay.linux-amd64.gz", "server/providers/wslc/relay/artifacts/discobox-cp-relay.linux-arm64.gz", "cli/internal/clipwatch/artifacts/discobox-clipwatch.windows-amd64.exe.gz", "cli/internal/clipwatch/artifacts/discobox-clipwatch.windows-arm64.exe.gz"}
}
func validateInputs(r receipt, revision, repository string, requireClean bool) error {
	if r.Revision != revision || r.Repository != repository || r.Platform != "native-inputs" || r.Status != "built" || (requireClean && r.Dirty) {
		return fmt.Errorf("portable inputs do not match this source/repository")
	}
	for _, name := range helperPaths() {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(r.Digests[name]) {
			return fmt.Errorf("portable inputs omit a valid digest for %s", name)
		}
	}
	return nil
}

// Load and verify the full transfer before any embed input is overwritten.
func loadPortableInputs(dir, revision, repository string, requireClean bool) (receipt, map[string][]byte, error) {
	inputData, err := os.ReadFile(filepath.Join(dir, "inputs.json"))
	if err != nil {
		return receipt{}, nil, err
	}
	var inputReceipt receipt
	if err = json.Unmarshal(inputData, &inputReceipt); err != nil {
		return receipt{}, nil, err
	}
	if err = validateInputs(inputReceipt, revision, repository, requireClean); err != nil {
		return receipt{}, nil, err
	}
	payloads := map[string][]byte{}
	for _, name := range helperPaths() {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return receipt{}, nil, err
		}
		sum := sha256.Sum256(body)
		if len(body) <= 65536 || hex.EncodeToString(sum[:]) != inputReceipt.Digests[name] {
			return receipt{}, nil, fmt.Errorf("invalid portable helper %s", name)
		}
		payloads[name] = body
	}
	return inputReceipt, payloads, nil
}
