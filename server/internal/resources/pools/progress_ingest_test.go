package pools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/auth"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// capturingReporter records what the ingest handed on, which is the blob a
// client will later read off the sandbox.
type capturingReporter struct {
	progress []store.SandboxProgressReport
}

func (c *capturingReporter) ReportSandboxStates(context.Context, store.SandboxStateReportBatch) error {
	return nil
}

func (c *capturingReporter) ReportSandboxProgress(_ context.Context, _ string, _ time.Time, reports []store.SandboxProgressReport) error {
	c.progress = append(c.progress, reports...)
	return nil
}

// poolPrincipalContext authorizes the report as the pool that is making it.
func poolPrincipalContext(t *testing.T, poolID string) context.Context {
	t.Helper()
	return auth.WithPrincipal(t.Context(), auth.Principal{Type: auth.PrincipalTypePool, PoolID: poolID})
}

// A phase with nothing to measure must survive the ingest.
//
// It did not: the stored blob was built with encoding/json, and an unset ogen
// optional marshals to zero bytes, which encoding/json rejects out of a
// json.Marshaler ("unexpected end of JSON input"). Every progress report
// carried an image pull until phases were added, so the bug arrived with the
// first report that had no pull — every phase but one — and surfaced as a 500
// the pool agent logged and dropped, leaving the record permanently empty.
func TestProgressIngestAcceptsAPhaseWithNoPull(t *testing.T) {
	reporter := &capturingReporter{}
	service := &Service{sandboxReporter: reporter}

	err := service.ReportPoolSandboxStates(poolPrincipalContext(t, "pool-1"), "pool-1", services.ReportPoolSandboxStatesBody{
		ReportedAt: time.Now().UTC(),
		Progress: []serverapi.PoolSandboxProgress{{
			SandboxId: "sbx_1",
			Phase:     serverapi.PoolSandboxProvisionPhaseCreatingContainer,
		}},
	})
	if err != nil {
		t.Fatalf("report progress: %v", err)
	}
	if len(reporter.progress) != 1 {
		t.Fatalf("recorded %d progress reports, want 1", len(reporter.progress))
	}

	// The blob is what a client decodes, so it has to be the client-facing
	// shape: the phase, no pull, and none of the agent-facing sandboxId that
	// SandboxProvisionProgress forbids.
	var stored map[string]any
	if err := json.Unmarshal(reporter.progress[0].Progress, &stored); err != nil {
		t.Fatalf("stored progress is not valid JSON (%s): %v", reporter.progress[0].Progress, err)
	}
	if stored["phase"] != string(serverapi.SandboxProvisionPhaseCreatingContainer) {
		t.Fatalf("stored phase = %v, want the reported one", stored["phase"])
	}
	if _, ok := stored["pull"]; ok {
		t.Fatalf("stored progress carries a pull it was never told about: %s", reporter.progress[0].Progress)
	}
	if _, ok := stored["sandboxId"]; ok {
		t.Fatalf("stored progress leaked the agent-facing sandboxId: %s", reporter.progress[0].Progress)
	}
}

// The pull still crosses intact, with its phase alongside it.
func TestProgressIngestCarriesThePullAndItsPhase(t *testing.T) {
	reporter := &capturingReporter{}
	service := &Service{sandboxReporter: reporter}

	err := service.ReportPoolSandboxStates(poolPrincipalContext(t, "pool-1"), "pool-1", services.ReportPoolSandboxStatesBody{
		ReportedAt: time.Now().UTC(),
		Progress: []serverapi.PoolSandboxProgress{{
			SandboxId: "sbx_1",
			Phase:     serverapi.PoolSandboxProvisionPhasePullingImage,
			Pull: serverapi.NewOptPoolSandboxPullProgress(serverapi.PoolSandboxPullProgress{
				Image:  "ghcr.io/example/sandbox:latest",
				Layers: serverapi.NewOptInt(4),
			}),
		}},
	})
	if err != nil {
		t.Fatalf("report progress: %v", err)
	}
	if len(reporter.progress) != 1 {
		t.Fatalf("recorded %d progress reports, want 1", len(reporter.progress))
	}

	var stored serverapi.SandboxProvisionProgress
	if err := stored.UnmarshalJSON(reporter.progress[0].Progress); err != nil {
		t.Fatalf("stored progress does not decode as the client-facing shape (%s): %v", reporter.progress[0].Progress, err)
	}
	if stored.Phase != serverapi.SandboxProvisionPhasePullingImage {
		t.Fatalf("stored phase = %q, want pulling_image", stored.Phase)
	}
	pull, ok := stored.Pull.Get()
	if !ok || pull.Image != "ghcr.io/example/sandbox:latest" || pull.Layers.Or(0) != 4 {
		t.Fatalf("stored pull = %+v, want the reported one", stored.Pull)
	}
}
