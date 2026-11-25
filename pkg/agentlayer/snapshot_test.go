package agentlayer

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// A snapshot plan is anonymous: one naming a credentials endpoint, secrets
// or sources, which only a sandbox has, is refused (ADR 26-10-16-423 §8).
func TestASnapshotPlanIsRefusedAnythingOfASandbox(t *testing.T) {
	good := func() SnapshotPlan {
		return SnapshotPlan{
			ID: "snapshot_1", ProjectID: "proj_1", PoolID: "pool_1",
			Pool:          sandboxconfig.PoolEndpoints{Proxy: "discobox-pool-proxy:17080"},
			RuntimeConfig: sandboxconfig.RuntimeConfig{Revision: 1, Proxy: &sandboxconfig.RuntimeProxy{}},
		}
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("an anonymous plan: %v", err)
	}
	for name, change := range map[string]func(*SnapshotPlan){
		"a credentials endpoint": func(p *SnapshotPlan) { p.Pool.Credentials = "discobox-pool-proxy:17090" },
		"a builder endpoint":     func(p *SnapshotPlan) { p.Pool.BuildKit = "https://discobox-pool-proxy:17081" },
		"secrets":                func(p *SnapshotPlan) { p.RuntimeConfig.SecretEnv = map[string]string{"TOKEN": "sentinel"} },
		"sources":                func(p *SnapshotPlan) { p.RuntimeConfig.Sources = []sandboxconfig.RuntimeSource{{Slug: "x"}} },
		"no proxy material":      func(p *SnapshotPlan) { p.RuntimeConfig.Proxy = nil },
		"no identity":            func(p *SnapshotPlan) { p.ID = "" },
	} {
		plan := good()
		change(&plan)
		if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "snapshot plan") {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
}

// The agent's start unit is pulled in by a link a snapshot removes, so a box
// made from one starts its agent when its maker says (ADR 26-11-03-986 §3).
func TestTheAgentStartUnitHasALinkToDisarm(t *testing.T) {
	for _, unit := range Units {
		if unit.Name != AgentStartUnit {
			continue
		}
		link, ok := unit.Link()
		if !ok || link != "/etc/systemd/system/paths.target.wants/"+AgentStartUnit {
			t.Fatalf("the agent start unit's link = %q, %v", link, ok)
		}
		return
	}
	t.Fatalf("no unit %s among the agent's units", AgentStartUnit)
}

// A progress line reads back as the status it was written from, and a line
// that is not one, or is torn, is refused (ADR 26-11-23-004 §1).
func TestASnapshotProgressLineReadsBackAndStrayOutputDoesNot(t *testing.T) {
	status := provisionspec.Status{
		State: provisionspec.StateProvisioning,
		Features: map[string]provisionspec.FeatureStatus{
			"docker": {State: provisionspec.StateProvisioning, Step: "install"},
			"go":     {State: provisionspec.StateFailed, Error: "download: 404"},
		},
	}
	line, err := SnapshotProgressLine(status)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(line), "\n") || strings.Count(string(line), "\n") != 1 {
		t.Fatalf("line = %q, want one line", line)
	}
	got, ok := ParseSnapshotProgress(line[:len(line)-1])
	if !ok || got.State != status.State || got.Features["docker"].Step != "install" || got.Features["go"].Error != "download: 404" {
		t.Fatalf("ParseSnapshotProgress = %+v, %v", got, ok)
	}
	for _, stray := range []string{"", "hello", SnapshotProgressPrefix + "{", SnapshotProgressPrefix + "{}", `{"state":"ready"}`} {
		if _, ok := ParseSnapshotProgress([]byte(stray)); ok {
			t.Errorf("ParseSnapshotProgress(%q) read a status", stray)
		}
	}
}
