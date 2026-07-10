package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func sandboxWith(agentConfigID string, source apiclientgen.OptGitSource) *apimodel.Sandbox {
	return &apimodel.Sandbox{
		Config: apimodel.SandboxConfig{
			AgentConfigId: apiclientgen.NewOptString(agentConfigID),
			Source:        source,
		},
	}
}

func TestEnsureRunAgentWillLaunchResolvedAgentConfig(t *testing.T) {
	// Server pinned an agent config: the sandbox-agent will launch it.
	sandbox := sandboxWith("agc_123", apiclientgen.OptGitSource{})
	if err := ensureRunAgentWillLaunch(sandbox); err != nil {
		t.Fatalf("ensureRunAgentWillLaunch = %v, want nil", err)
	}
}

func TestEnsureRunAgentWillLaunchLocalSourceWithoutConfigFailsFast(t *testing.T) {
	dir := t.TempDir()
	source := apiclientgen.NewOptGitSource(apiclientgen.GitSource{
		Kind:           apiclientgen.GitSourceKindGit,
		LocalDirectory: apiclientgen.NewOptString(dir),
	})
	sandbox := sandboxWith("", source)
	if err := ensureRunAgentWillLaunch(sandbox); !errors.Is(err, errNoRunAgent) {
		t.Fatalf("ensureRunAgentWillLaunch = %v, want errNoRunAgent", err)
	}
}

func TestEnsureRunAgentWillLaunchLocalSourceWithConfigProceeds(t *testing.T) {
	dir := t.TempDir()
	discoDir := filepath.Join(dir, ".discobox")
	if err := os.MkdirAll(discoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(discoDir, "agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := apiclientgen.NewOptGitSource(apiclientgen.GitSource{
		Kind:           apiclientgen.GitSourceKindGit,
		LocalDirectory: apiclientgen.NewOptString(dir),
	})
	sandbox := sandboxWith("", source)
	if err := ensureRunAgentWillLaunch(sandbox); err != nil {
		t.Fatalf("ensureRunAgentWillLaunch = %v, want nil", err)
	}
}

func TestEnsureRunAgentWillLaunchRemoteSourceDefersToWait(t *testing.T) {
	// A remote source has no local directory to inspect, so run cannot prove the
	// absence of a local agent config and must defer to the bounded wait.
	source := apiclientgen.NewOptGitSource(apiclientgen.GitSource{
		Kind: apiclientgen.GitSourceKindGit,
	})
	sandbox := sandboxWith("", source)
	if err := ensureRunAgentWillLaunch(sandbox); err != nil {
		t.Fatalf("ensureRunAgentWillLaunch = %v, want nil", err)
	}
}
