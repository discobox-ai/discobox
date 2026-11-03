package agentlayer

import (
	"strings"
	"testing"

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
