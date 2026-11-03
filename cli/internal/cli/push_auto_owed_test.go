package cli

import (
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// Until a discobox's delivery is reported, only that delivery writes into its
// origin. A create whose delivery never landed is failed by the source-await
// timeout, not parked, and an automatic push into it moves the branch the
// delivery still has to land on — whatever state the runtime is in.
func TestPushableRefusesADiscoboxStillOwedItsDelivery(t *testing.T) {
	owed := pushDeliveredSandbox()
	owed.Runtime.State = apiclientgen.SandboxRuntimeStateFailed
	owed.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState("failed")
	if pushable(owed, thisHost) {
		t.Error("failed with no sourceDeliveredAt: pushable = true, want false")
	}

	delivered := owed
	delivered.Runtime.SourceDeliveredAt = apiclientgen.NewOptDateTime(time.Now())
	if !pushable(delivered, thisHost) {
		t.Error("failed after its delivery: pushable = false, want true")
	}
}
