package sandboxcreate

import (
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func poolWith(progress apimodel.PoolProvisionProgress, observed time.Time) *apimodel.Pool {
	return &apimodel.Pool{
		ProvisionProgress:   apiclientgen.NewOptPoolProvisionProgress(progress),
		ProvisionProgressAt: apiclientgen.NewOptDateTime(observed),
	}
}

// The whole point: "waiting for a pool to take it" is the longest part of a
// cold start and the least informative thing to say about it.
func TestPoolProvisionStatusNamesTheDriversWork(t *testing.T) {
	for _, tc := range []struct {
		phase apiclientgen.PoolProvisionPhase
		want  Step
	}{
		{apiclientgen.PoolProvisionPhaseFetchingVMImage, "fetching the VM image"},
		{apiclientgen.PoolProvisionPhaseStartingVM, "starting the VM"},
		{apiclientgen.PoolProvisionPhaseWaitingForDocker, "waiting for Docker in the VM"},
		{apiclientgen.PoolProvisionPhasePullingPoolImage, "pulling the pool agent image"},
		{apiclientgen.PoolProvisionPhaseStartingPoolAgent, "starting the pool agent"},
		{apiclientgen.PoolProvisionPhaseWaitingForPoolAgent, "waiting for the pool agent to come up"},
	} {
		pool := poolWith(apimodel.PoolProvisionProgress{Phase: tc.phase}, time.Now())
		if got := PoolProvisionStatus(pool); got != tc.want {
			t.Errorf("PoolProvisionStatus(%s) = %q, want %q", tc.phase, got, tc.want)
		}
	}
}

// The two long phases carry byte counts, rendered by the same code that renders
// a sandbox's pull — one shape, one renderer.
func TestPoolProvisionStatusRendersAPullWithItsBytes(t *testing.T) {
	pool := poolWith(apimodel.PoolProvisionProgress{
		Phase: apiclientgen.PoolProvisionPhasePullingPoolImage,
		Pull: apiclientgen.NewOptSandboxPullProgress(apimodel.SandboxPullProgress{
			Image:          "ghcr.io/discobox-ai/discobox-pool-agent:v1",
			Current:        apiclientgen.NewOptInt64(500 << 20),
			Total:          apiclientgen.NewOptInt64(1000 << 20),
			Layers:         apiclientgen.NewOptInt(40),
			LayersComplete: apiclientgen.NewOptInt(3),
		}),
	}, time.Now())
	got := string(PoolProvisionStatus(pool))
	for _, want := range []string{"discobox-pool-agent:v1", "500.0 MiB", "1000.0 MiB", "3/40 layers"} {
		if !strings.Contains(got, want) {
			t.Fatalf("PoolProvisionStatus() = %q, missing %q", got, want)
		}
	}
}

// A pool that came up long ago still carries the phase it finished in; the
// record is never cleared, so age is what makes it history.
func TestPoolProvisionStatusIgnoresAStalePhase(t *testing.T) {
	pool := poolWith(
		apimodel.PoolProvisionProgress{Phase: apiclientgen.PoolProvisionPhaseStartingVM},
		time.Now().Add(-2*ProvisionProgressFresh),
	)
	if got := PoolProvisionStatus(pool); got != "" {
		t.Fatalf("PoolProvisionStatus() = %q, want nothing for a stale phase", got)
	}
}

func TestPoolProvisionStatusSaysNothingWithoutARecord(t *testing.T) {
	if got := PoolProvisionStatus(&apimodel.Pool{}); got != "" {
		t.Fatalf("PoolProvisionStatus() = %q, want nothing", got)
	}
	if got := PoolProvisionStatus(nil); got != "" {
		t.Fatalf("PoolProvisionStatus(nil) = %q, want nothing", got)
	}
}
