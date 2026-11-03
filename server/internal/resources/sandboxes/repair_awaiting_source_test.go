package sandboxes

import (
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// A discobox still owed its client's delivery that a repair rebuilds is parked
// again at awaiting_source, and repair must leave it parked: starting it would
// run the harness on a workspace the server says is not there, read as
// starting forever, and be failed by the source-await timeout while it runs.
func TestRepairLeavesASandboxAwaitingItsSourceParked(t *testing.T) {
	service, provider, _ := useFixture(t)
	ctx := useContext(t)
	sb := storedSandbox(t, service)
	sb.Source = &model.GitSource{Kind: "git", Delivery: model.GitSourceDeliveryPush}
	if err := service.store.UpdateSandbox(ctx, sb); err != nil {
		t.Fatalf("make the source push-delivered: %v", err)
	}
	if !awaitingSourcePush(storedSandbox(t, service)) {
		t.Fatal("fixture: the sandbox should be owed its delivery")
	}

	if _, err := service.RepairSandbox(ctx, "project-1", "sb-1"); err != nil {
		t.Fatalf("repair: %v", err)
	}

	if got := storedSandbox(t, service).State; got != model.SandboxStateAwaitingSource {
		t.Fatalf("state = %q, want awaiting_source: the delivery is still owed", got)
	}
	if n := provider.starts.Load(); n != 0 {
		t.Fatalf("starts = %d, want 0: repair started a sandbox parked for its source", n)
	}
}
