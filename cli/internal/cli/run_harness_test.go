package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func sandboxWith(harnessConfigID string, source apiclientgen.OptGitSource) *apimodel.Sandbox {
	return &apimodel.Sandbox{
		Config: apimodel.SandboxConfig{
			HarnessConfigId: apiclientgen.NewOptString(harnessConfigID),
			Source:          source,
		},
	}
}

func TestEnsureRunHarnessWillLaunchResolvedHarnessConfig(t *testing.T) {
	// Server pinned a harness config: the sandbox-agent will launch it.
	sandbox := sandboxWith("agc_123", apiclientgen.OptGitSource{})
	if err := ensureRunHarnessWillLaunch(sandbox); err != nil {
		t.Fatalf("ensureRunHarnessWillLaunch = %v, want nil", err)
	}
}

func TestEnsureRunHarnessWillLaunchLocalSourceWithoutConfigFailsFast(t *testing.T) {
	dir := t.TempDir()
	source := apiclientgen.NewOptGitSource(apiclientgen.GitSource{
		Kind:           apiclientgen.GitSourceKindGit,
		LocalDirectory: apiclientgen.NewOptString(dir),
	})
	sandbox := sandboxWith("", source)
	if err := ensureRunHarnessWillLaunch(sandbox); !errors.Is(err, errNoRunHarness) {
		t.Fatalf("ensureRunHarnessWillLaunch = %v, want errNoRunHarness", err)
	}
}

func TestEnsureRunHarnessWillLaunchLocalSourceWithConfigProceeds(t *testing.T) {
	dir := t.TempDir()
	discoDir := filepath.Join(dir, ".discobox")
	if err := os.MkdirAll(discoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discoDir, "harness.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := apiclientgen.NewOptGitSource(apiclientgen.GitSource{
		Kind:           apiclientgen.GitSourceKindGit,
		LocalDirectory: apiclientgen.NewOptString(dir),
	})
	sandbox := sandboxWith("", source)
	if err := ensureRunHarnessWillLaunch(sandbox); err != nil {
		t.Fatalf("ensureRunHarnessWillLaunch = %v, want nil", err)
	}
}

func TestEnsureRunHarnessWillLaunchRemoteSourceDefersToWait(t *testing.T) {
	// A remote source has no local directory to inspect, so run cannot prove the
	// absence of a local harness config and must defer to the bounded wait.
	source := apiclientgen.NewOptGitSource(apiclientgen.GitSource{
		Kind: apiclientgen.GitSourceKindGit,
	})
	sandbox := sandboxWith("", source)
	if err := ensureRunHarnessWillLaunch(sandbox); err != nil {
		t.Fatalf("ensureRunHarnessWillLaunch = %v, want nil", err)
	}
}
