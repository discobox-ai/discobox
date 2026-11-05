package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"

	"github.com/discobox-ai/discobox/pkg/requestprogress"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The triage regression first reproduced the 409 unchanged. The accepted
// design requires explicit provenance, rather than treating every generation
// gap as an upgrade. Start must now wait before dispatching to the runtime.
func TestExplicitStartWaitsForPendingUpgradeRuntimeAfterPoolReturns(t *testing.T) {
	svc, p, db := useFixture(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Updates(map[string]any{
		"state": model.SandboxStateReady, "generation": 4, "observed_generation": 3,
		"upgrade_generation": 4, "last_successful_create": 3, "error_message": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	p.container.Store(true)
	p.startCall = func(_ context.Context, ref sandbox.SandboxRef) error {
		if ref.StartEpoch == nil {
			t.Error("start lost its admission fence")
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := svc.StartSandbox(useContext(t), "project-1", "sb-1", services.StartSandboxBody{})
		done <- err
	}()
	// Admission precedes waiting. Once admitted no start or repair may be sent
	// until the original upgrade converges, even with a reachable pool.
	deadline := time.After(3 * time.Second)
	for storedSandbox(t, svc).WakeEpoch == 0 {
		select {
		case err := <-done:
			t.Fatalf("returned before convergence: %v", err)
		case <-deadline:
			t.Fatal("start never admitted")
		case <-time.After(time.Millisecond):
		}
	}
	if got := p.starts.Load(); got != 0 {
		t.Fatalf("starts while upgrade pending = %d", got)
	}
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Update("observed_generation", 4).Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("start did not finish")
	}
	sb := storedSandbox(t, svc)
	if p.starts.Load() != 1 || p.creates.Load() != 0 || sb.Generation != 4 || sb.WakeEpoch != 1 {
		t.Fatalf("starts=%d repairs=%d generation=%d epoch=%d", p.starts.Load(), p.creates.Load(), sb.Generation, sb.WakeEpoch)
	}
}

func TestPendingUpgradeStartRequiresDurableProvenance(t *testing.T) {
	baseline := model.Sandbox{PoolID: "pool-1", UpgradeGeneration: 4, LastSuccessfulCreate: 3, ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.DesiredStatePresent, State: model.SandboxStateReady, Generation: 4, ObservedGeneration: 3}}
	if !pendingUpgradeStart(&baseline) {
		t.Fatal("existing pending upgrade must qualify")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*model.Sandbox)
	}{
		{"initial create", func(sb *model.Sandbox) { sb.LastSuccessfulCreate = 0 }},
		{"unrelated intent", func(sb *model.Sandbox) { sb.UpgradeGeneration = 3 }},
		{"repair", func(sb *model.Sandbox) { sb.RepairGeneration = 4 }},
		{"settled", func(sb *model.Sandbox) { sb.ObservedGeneration = 4 }},
		{"archived", func(sb *model.Sandbox) { sb.DesiredState = model.DesiredStateArchived }},
		{"delivery", func(sb *model.Sandbox) { sb.State = model.SandboxStateAwaitingSource }},
		{"unplaced", func(sb *model.Sandbox) { sb.PoolID = "" }},
		{"harness change", func(sb *model.Sandbox) { id := "other"; sb.PendingHarnessConfigID = &id }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := baseline
			tc.mutate(&sb)
			if pendingUpgradeStart(&sb) {
				t.Fatal("unexpected upgrade entitlement")
			}
		})
	}
	baseline.State = model.SandboxStateFailed
	if !pendingUpgradeStart(&baseline) {
		t.Fatal("previously created failed sandbox with pending upgrade must qualify")
	}
}

func TestUpgradeAdmissionCapturesLegacySuccessAndSurvivesMetadataSave(t *testing.T) {
	svc, _, db := useFixture(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Updates(map[string]any{"state": model.SandboxStateReady, "generation": 3, "observed_generation": 3, "error_message": nil}).Error; err != nil {
		t.Fatal(err)
	}
	stale := storedSandbox(t, svc)
	repin := sandboxRepin{Available: true}
	sb, err := svc.recordSandboxIntent(t.Context(), "project-1", "sb-1", model.DesiredStatePresent, false, repin.applyUpgrade)
	if err != nil {
		t.Fatal(err)
	}
	if sb.UpgradeGeneration != 4 || sb.LastSuccessfulCreate != 3 {
		t.Fatalf("upgrade=%d created=%d", sb.UpgradeGeneration, sb.LastSuccessfulCreate)
	}
	// Whole-row bookkeeping must not erase independently admitted provenance.
	if err := svc.store.UpdateSandbox(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	current, err := svc.store.GetSandbox(t.Context(), "project-1", "sb-1", store.SandboxFromPrimary())
	if err != nil {
		t.Fatal(err)
	}
	if current.UpgradeGeneration != 4 || current.LastSuccessfulCreate != 3 {
		t.Fatal("metadata save erased provenance")
	}
}

func TestExplicitStartDoesNotWaitForArbitraryMissingRuntime(t *testing.T) {
	svc, p := readyStartFixture(t)
	p.startCall = func(context.Context, sandbox.SandboxRef) error { return sandbox.ErrNotFound }
	_, err := svc.StartSandbox(useContext(t), "project-1", "sb-1", services.StartSandboxBody{})
	if err == nil || !strings.Contains(err.Error(), "no runtime") || p.starts.Load() != 1 {
		t.Fatalf("err=%v starts=%d", err, p.starts.Load())
	}
}

func TestUpgradeProvenanceSurvivesAdditiveMigration(t *testing.T) {
	svc, _, db := useFixture(t)
	before := storedSandbox(t, svc)
	for _, column := range []string{"upgrade_generation", "last_successful_create"} {
		if err := db.Migrator().DropColumn(&model.Sandbox{}, column); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&model.Sandbox{}); err != nil {
		t.Fatal(err)
	}
	sb := storedSandbox(t, svc)
	if sb.ID != before.ID || sb.Generation != before.Generation || sb.UpgradeGeneration != 0 || sb.LastSuccessfulCreate != 0 {
		t.Fatalf("migration changed old row: %+v", sb)
	}
}

func TestGuardedMetadataSaveCannotEraseSuccessfulCreate(t *testing.T) {
	svc, _, _ := useFixture(t)
	stale := storedSandbox(t, svc)
	if err := svc.store.RecordSuccessfulSandboxCreate(t.Context(), "project-1", "sb-1", stale.Generation); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpdateSandbox(t.Context(), stale, store.WithGeneration(stale.Generation)); err != nil {
		t.Fatal(err)
	}
	if got := storedSandbox(t, svc).LastSuccessfulCreate; got != stale.Generation {
		t.Fatalf("prior success erased: %d", got)
	}
}

type upgradeProgressRecorder struct {
	*httptest.ResponseRecorder
	phases  []string
	onPhase func(string)
}

func (w *upgradeProgressRecorder) WriteHeader(code int) {
	if code == http.StatusProcessing {
		phase := w.Header().Get(requestprogress.Header)
		w.phases = append(w.phases, phase)
		if w.onPhase != nil {
			w.onPhase(phase)
		}
		return
	}
	w.ResponseRecorder.WriteHeader(code)
}

func TestExplicitUpgradeStartNarratesPoolThenUpgradeAndClearsProgress(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "superseded", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			svc, p, db := useFixture(t)
			if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Updates(map[string]any{"state": model.SandboxStateReady, "generation": 4, "observed_generation": 3, "upgrade_generation": 4, "last_successful_create": 3, "error_message": nil}).Error; err != nil {
				t.Fatal(err)
			}
			p.container.Store(true)
			probes := 0
			p.compatibilityCall = func(context.Context, sandbox.SandboxRef) error {
				probes++
				if probes == 1 {
					return sandbox.ErrPoolProtocolTransition
				}
				return nil
			}
			ctx, cancel := context.WithCancel(useContext(t))
			defer cancel()
			recorder := &upgradeProgressRecorder{ResponseRecorder: httptest.NewRecorder()}
			recorder.onPhase = func(phase string) {
				if !strings.Contains(phase, "waiting for the sandbox upgrade") {
					return
				}
				updates := map[string]any{"observed_generation": 4}
				switch outcome {
				case "failure":
					updates["state"] = model.SandboxStateFailed
					updates["error_message"] = "upgrade image unavailable"
				case "superseded":
					updates["generation"] = 5
				case "cancel":
					cancel()
					return
				}
				if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Updates(updates).Error; err != nil {
					t.Fatal(err)
				}
			}
			var result error
			requestprogress.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, result = svc.StartSandbox(r.Context(), "project-1", "sb-1", services.StartSandboxBody{})
			})).ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodPost, "/start", nil))
			if (result == nil) != (outcome == "success") {
				t.Fatalf("outcome=%s err=%v", outcome, result)
			}
			if len(recorder.phases) != 3 || !strings.Contains(recorder.phases[0], "pool to come online") || !strings.Contains(recorder.phases[1], "sandbox upgrade") || recorder.phases[2] != "" {
				t.Fatalf("phases=%q", recorder.phases)
			}
			if p.creates.Load() != 0 || storedSandbox(t, svc).WakeEpoch != 1 {
				t.Fatal("wait admitted a second operation")
			}
			if outcome != "success" && p.starts.Load() != 0 {
				t.Fatal("start sent after failed wait")
			}
		})
	}
}

func TestPendingUpgradeCeilingIncludesFirstCompatibilityProbe(t *testing.T) {
	old := sandboxPoolHostWaitCeiling
	sandboxPoolHostWaitCeiling = 30 * time.Millisecond
	t.Cleanup(func() { sandboxPoolHostWaitCeiling = old })
	svc, p, db := useFixture(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Updates(map[string]any{"state": model.SandboxStateReady, "generation": 4, "observed_generation": 3, "upgrade_generation": 4, "last_successful_create": 3, "error_message": nil}).Error; err != nil {
		t.Fatal(err)
	}
	p.compatibilityCall = func(ctx context.Context, _ sandbox.SandboxRef) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 100*time.Millisecond {
			t.Error("first probe exceeds upgrade ceiling")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	started := time.Now()
	_, err := svc.StartSandbox(useContext(t), "project-1", "sb-1", services.StartSandboxBody{})
	if err == nil || time.Since(started) > time.Second || !strings.Contains(err.Error(), "waiting ended") || !strings.Contains(err.Error(), "pool-1") {
		t.Fatalf("err=%v duration=%v", err, time.Since(started))
	}
	if p.starts.Load() != 0 {
		t.Fatal("expired probe sent start")
	}
}

func TestFirstUpgradeProbeCallerDeadlinePreservesRefusal(t *testing.T) {
	svc, p, db := useFixture(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Updates(map[string]any{"state": model.SandboxStateReady, "generation": 4, "observed_generation": 3, "upgrade_generation": 4, "last_successful_create": 3, "error_message": nil}).Error; err != nil {
		t.Fatal(err)
	}
	p.compatibilityCall = func(ctx context.Context, _ sandbox.SandboxRef) error { <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithTimeout(useContext(t), 30*time.Millisecond)
	defer cancel()
	_, err := svc.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{})
	var status apperrors.StatusError
	if !errors.As(err, &status) || status.Status != http.StatusConflict || !strings.Contains(err.Error(), "waiting ended") || !strings.Contains(err.Error(), "pool-1") {
		t.Fatalf("first probe deadline: %v", err)
	}
	if p.starts.Load() != 0 {
		t.Fatal("expired request sent start")
	}
}
