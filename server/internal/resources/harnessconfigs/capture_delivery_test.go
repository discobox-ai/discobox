package harnessconfigs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

type captureDeliveryRuntime struct {
	*sandboxes.Service
	store    *store.Store
	endpoint *httptest.Server
	internal atomic.Int32
}

func (r *captureDeliveryRuntime) AcquireSandboxHTTPClient(ctx context.Context, p, b string, _ []string, _ services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	box, err := r.store.GetSandbox(ctx, p, b)
	if err != nil {
		return nil, nil, err
	}
	return transport.NewHTTPClientLeaseWithBaseURL(r.endpoint.Client(), r.endpoint.URL, nil), box, nil
}
func (r *captureDeliveryRuntime) AcquireSandboxHTTPClientForServer(ctx context.Context, p, b string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	r.internal.Add(1)
	return r.AcquireSandboxHTTPClient(ctx, p, b, scopes, use)
}

func TestSavedCaptureAcknowledgmentConvergesAfterLostFirstDelivery(t *testing.T) {
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatal(err)
	}
	st := store.New(db.Write, db.Read)
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true, PollInterval: 10 * time.Millisecond, BackoffBase: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	svc, cfg, candidate := captureFixtureStore(t, st)
	svc.dirtier = engine
	runtime := &captureDeliveryRuntime{Service: sandboxes.NewService(st, nil, "user-1", engine), store: st}
	svc.sandboxes = runtime
	var captureCalls, ackCalls atomic.Int32
	received := make(chan harnesscapture.Acknowledgment, 4)
	runtime.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/harness-config-capture") {
			captureCalls.Add(1)
			if err := json.NewEncoder(w).Encode(candidate); err != nil {
				t.Error(err)
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/harness-config-ack") {
			if ackCalls.Add(1) == 1 {
				w.WriteHeader(503)
				return
			} // Never delivered to the agent.
			var ack harnesscapture.Acknowledgment
			if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
				t.Error(err)
				return
			}
			received <- ack
			if err := json.NewEncoder(w).Encode(harnesscapture.Preview{State: "clean", HarnessConfigID: cfg.ID}); err != nil {
				t.Error(err)
			}
			return
		}
		t.Errorf("unexpected endpoint %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	defer runtime.endpoint.Close()
	userCtx := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeUser, UserID: "user-1"})
	result, err := svc.SaveSandboxHarnessConfigChanges(userCtx, cfg.ProjectID, "sbx-capture", harnesscapture.CandidateRequest{CandidateID: candidate.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "saved-handoff-pending" {
		t.Fatalf("state=%s", result.State)
	}
	dirty, err := engine.ListDirty(ctx, HarnessConfigResourceType)
	if err != nil || len(dirty) != 1 {
		t.Fatalf("missing durable delivery intent: %v %+v", err, dirty)
	}
	// Even a replica that has not observed the committed schema/data cannot
	// hide the receipt from replay or the durable work scan.
	replica, replicaErr := database.New(database.Config{DSN: ":memory:"})
	if replicaErr != nil {
		t.Fatal(replicaErr)
	}
	defer func() { _ = replica.Close() }()
	lagging := store.New(db.Write, replica.Read)
	if _, err := lagging.GetHarnessCaptureReceipt(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, candidate.ID); err != nil {
		t.Fatalf("replay read lagging replica: %v", err)
	}
	if pending, err := lagging.ListPendingHarnessCaptureReceipts(ctx, cfg.ProjectID, cfg.ID); err != nil || len(pending) != 1 {
		t.Fatalf("delivery scan missed primary receipt: %v", err)
	}
	// Reconstruct the service before starting its worker: no user context or
	// second save survives into this attempt, only the durable receipt and mark.
	restarted := &Service{store: st, sandboxes: runtime, dirtier: engine}
	if err := engine.Register(HarnessConfigResourceType, restarted); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Stop(context.Background()) }()
	select {
	case ack := <-received:
		assignments, err := st.ListSandboxSecrets(ctx, cfg.ProjectID, "sbx-capture")
		if err != nil {
			t.Fatal(err)
		}
		if len(assignments) != 1 || ack.Sentinels["API_TOKEN"] == "" || ack.Sentinels["API_TOKEN"] != assignments[0].Sentinel {
			t.Fatal("first-login acknowledgment lacks origin sentinel")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending acknowledgment was not automatically delivered")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		receipt, err := st.GetHarnessCaptureReceipt(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, candidate.ID)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.AcknowledgedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("acknowledgment was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if captureCalls.Load() != 1 {
		t.Fatal("reconciliation resubmitted credential capture")
	}
	if runtime.internal.Load() < 2 {
		t.Fatal("delivery did not use explicit internal authority")
	}
}

func TestCaptureAddsOriginAccountWithoutReplacingBoxOverridesOrConsumerAssignments(t *testing.T) {
	svc, cfg, candidate := captureFixture(t)
	st := svc.store
	ctx := context.Background()
	cfg.SaveBack.Providers[0].MultipleAccounts = true
	if err := st.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	override := &model.Secret{ProjectID: cfg.ProjectID, Name: "Box-only", Type: "token", EncryptedValue: []byte(`{"token":"box-value"}`)}
	if err := st.CreateSecret(ctx, override); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSandboxSecret(ctx, &model.SandboxSecret{ProjectID: cfg.ProjectID, SandboxID: "sbx-capture", SecretID: override.ID, EnvName: "API_TOKEN", Sentinel: "box-override-sentinel"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-consumer", ProjectID: cfg.ProjectID, PoolID: "pool-capture", Name: "Consumer", SandboxManifest: model.SandboxManifest{HarnessConfigID: &cfg.ID}}); err != nil {
		t.Fatal(err)
	}
	runtime := &captureDeliveryRuntime{Service: sandboxes.NewService(st, nil, "user-1", nil), store: st}
	svc.sandboxes = runtime
	received := make(chan harnesscapture.Acknowledgment, 2)
	runtime.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/harness-config-capture") {
			if err := json.NewEncoder(w).Encode(candidate); err != nil {
				t.Error(err)
			}
			return
		}
		var ack harnesscapture.Acknowledgment
		if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
			t.Error(err)
			return
		}
		received <- ack
		if err := json.NewEncoder(w).Encode(harnesscapture.Preview{State: "clean", HarnessConfigID: cfg.ID}); err != nil {
			t.Error(err)
		}
	}))
	defer runtime.endpoint.Close()
	userCtx := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeUser, UserID: "user-1"})
	for i, env := range []string{"API_TOKEN", "API_TOKEN_2"} {
		baseline, err := st.CaptureBaseline(ctx, cfg.ProjectID, cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		candidate.Baseline = baseline
		candidate.WriteRevision = baseline.ConfigRevision
		candidate.SecretRevisions = baseline.SecretRevisions
		candidate.ID = "capture-" + env
		candidate.Credentials[0].EnvName = env
		result, err := svc.SaveSandboxHarnessConfigChanges(userCtx, cfg.ProjectID, "sbx-capture", harnesscapture.CandidateRequest{CandidateID: candidate.ID})
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "clean" {
			t.Fatalf("save state=%s", result.State)
		}
		ack := <-received
		if ack.Sentinels["API_TOKEN"] != "box-override-sentinel" {
			t.Fatal("per-box override replaced")
		}
		if i == 1 && ack.Sentinels["API_TOKEN_2"] == "" {
			t.Fatal("additional account has no sentinel")
		}
	}
	assignments, err := st.ListSandboxSecrets(ctx, cfg.ProjectID, "sbx-consumer")
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 0 {
		t.Fatal("new credential unexpectedly injected into existing consumer")
	}
	origin, err := st.ListSandboxSecrets(ctx, cfg.ProjectID, "sbx-capture")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range origin {
		if a.EnvName == "API_TOKEN" && a.SecretID != override.ID {
			t.Fatal("origin override secret changed")
		}
	}
}

type captureJanitorRuntime struct {
	stubSandboxRuntime
	deleted []string
}

func (r *captureJanitorRuntime) DeleteSandbox(_ context.Context, _, id string) error {
	r.deleted = append(r.deleted, id)
	return nil
}
func (*captureJanitorRuntime) AcquireSandboxHTTPClientForServer(context.Context, string, string, []string, services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	return nil, nil, errors.New("origin is unreachable")
}

func TestCaptureDeliveryFailureDoesNotBlockConfigureJanitor(t *testing.T) {
	svc, cfg, candidate := captureFixture(t)
	ctx := context.Background()
	if _, err := svc.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, candidate); err != nil {
		t.Fatal(err)
	}
	runtime := &captureJanitorRuntime{}
	svc.sandboxes = runtime
	if err := svc.store.Transaction(ctx, func(_ *store.Store, db *gorm.DB) error {
		return db.Model(&model.HarnessConfig{}).Where("id = ?", cfg.ID).UpdateColumns(map[string]any{"configure_sandbox_id": "configure-expired", "updated_at": time.Now().Add(-2 * configureTTL)}).Error
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reconcile(ctx, cfg.ID); err == nil {
		t.Fatal("delivery failure was not reported")
	}
	if len(runtime.deleted) != 1 || runtime.deleted[0] != "configure-expired" {
		t.Fatal("unreachable origin blocked configure cleanup")
	}
	current, err := svc.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ConfigureSandboxID != "" {
		t.Fatal("configure lifecycle was not cleared")
	}
	pending, err := svc.store.ListPendingHarnessCaptureReceipts(ctx, cfg.ProjectID, cfg.ID)
	if err != nil || len(pending) != 1 {
		t.Fatal("failed delivery intent lost")
	}
}

func TestCaptureOutcomeReachesOriginAfterRemoteEdit(t *testing.T) {
	for _, change := range []string{"settings", "binding", "manifest"} {
		t.Run(change, func(t *testing.T) {
			svc, cfg, candidate := captureFixture(t)
			st := svc.store
			ctx := context.Background()
			runtime := &captureDeliveryRuntime{Service: sandboxes.NewService(st, nil, "user-1", nil), store: st}
			svc.sandboxes = runtime
			receipt, err := svc.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, candidate)
			if err != nil {
				t.Fatal(err)
			}
			current, err := st.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			current.Name = "Renamed"
			current.ConfiguredFiles = []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"model":"remote"}`}}
			if change == "manifest" {
				current.ManifestDigest = "sha256:new-definition"
			}
			if err := st.UpdateHarnessConfig(ctx, current); err != nil {
				t.Fatal(err)
			}
			if change == "binding" {
				if err := st.DeleteHarnessConfigSecretBinding(ctx, cfg.ProjectID, cfg.ID, "API_TOKEN"); err != nil {
					t.Fatal(err)
				}
			}
			received := make(chan harnesscapture.Acknowledgment, 1)
			var attempts atomic.Int32
			runtime.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if attempts.Add(1) == 1 {
					w.WriteHeader(503)
					return
				}
				var ack harnesscapture.Acknowledgment
				if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
					t.Error(err)
					return
				}
				received <- ack
				state := "clean"
				if ack.Superseded {
					state = "superseded"
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(harnesscapture.Preview{State: state, HarnessConfigID: cfg.ID}); err != nil {
					t.Error(err)
				}
			}))
			defer runtime.endpoint.Close()
			if _, err := svc.deliverCaptureAcknowledgment(ctx, receipt); err == nil {
				t.Fatal("initial delivery should fail")
			}
			pending, err := st.ListPendingHarnessCaptureReceipts(ctx, cfg.ProjectID, cfg.ID)
			if err != nil || len(pending) != 1 {
				t.Fatal("server finished before origin received outcome")
			}
			if _, err := svc.deliverCaptureAcknowledgment(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			ack := <-received
			if change == "settings" {
				if ack.Superseded || ack.Baseline.ConfigRevision != current.Revision || ack.Files[0].Content != `{"model":"local"}` || ack.Sentinels["API_TOKEN"] == "" {
					t.Fatal("unrelated edit blocked accepted handoff or adopted unrelated settings")
				}
			} else if !ack.Superseded || len(ack.Sentinels) != 0 || len(ack.Files) != 0 || ack.Baseline.ConfigRevision != receipt.Baseline.ConfigRevision {
				t.Fatal("unsafe change did not deliver terminal supersession")
			}
			pending, err = st.ListPendingHarnessCaptureReceipts(ctx, cfg.ProjectID, cfg.ID)
			if err != nil || len(pending) != 0 {
				t.Fatal("acknowledged outcome left delivery pending")
			}
		})
	}
}

func TestCaptureDefinitionConflictIsActionableWithoutEchoingAgentBody(t *testing.T) {
	svc, cfg, candidate := captureFixture(t)
	ctx := context.Background()
	cfg.ManifestDigest = "sha256:new-manifest"
	if err := svc.store.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	_, err := svc.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, candidate)
	if err == nil || !strings.Contains(err.Error(), "upgrade or recreate") {
		t.Fatalf("manifest change lacked recovery instruction: %v", err)
	}
	runtime := &captureDeliveryRuntime{Service: sandboxes.NewService(svc.store, nil, "user-1", nil), store: svc.store}
	svc.sandboxes = runtime
	runtime.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte("private-agent-error-secret"))
	}))
	defer runtime.endpoint.Close()
	userCtx := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeUser, UserID: "user-1"})
	_, err = svc.ReconsiderSandboxHarnessConfigChanges(userCtx, cfg.ProjectID, "sbx-capture", harnesscapture.Selection{CandidateID: candidate.ID})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 409 || !strings.Contains(err.Error(), "upgrade or recreate") || strings.Contains(err.Error(), "private-agent-error-secret") {
		t.Fatalf("unsafe or unhelpful remote mismatch error: %v", err)
	}
}

func TestCaptureTransportUsesResolvedSandboxIdentity(t *testing.T) {
	svc, cfg, _ := captureFixture(t)
	box, err := svc.store.GetSandbox(context.Background(), cfg.ProjectID, "sbx-capture")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/project/"+box.ProjectID+"/pool/"+box.PoolID+"/sandboxes/"+box.ID+"/harness-config-capture" {
			t.Errorf("unresolved identity sent downstream: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer endpoint.Close()
	lease := transport.NewHTTPClientLeaseWithBaseURL(endpoint.Client(), endpoint.URL, nil)
	defer lease.Release()
	var candidate harnesscapture.Candidate
	if err := svc.sendCaptureRequest(context.Background(), "default", "friendly-box-name", http.MethodPost, "/harness-config-capture", nil, &candidate, lease, box); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredCaptureHandoffKeepsServerReceiptPending(t *testing.T) {
	svc, cfg, candidate := captureFixture(t)
	ctx := t.Context()
	receipt, err := svc.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, candidate)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &captureDeliveryRuntime{Service: sandboxes.NewService(svc.store, nil, "user-1", nil), store: svc.store}
	svc.sandboxes = runtime
	var complete atomic.Bool
	runtime.endpoint = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		state := "saved-handoff-pending"
		if complete.Load() {
			state = "saved"
		}
		if err := json.NewEncoder(w).Encode(harnesscapture.Preview{State: state, HarnessConfigID: cfg.ID}); err != nil {
			t.Error(err)
		}
	}))
	defer runtime.endpoint.Close()
	state, err := svc.deliverCaptureAcknowledgment(ctx, receipt)
	if err != nil || state != "saved-handoff-pending" {
		t.Fatalf("delivery = %q, %v", state, err)
	}
	pending, err := svc.store.ListPendingHarnessCaptureReceipts(ctx, cfg.ProjectID, cfg.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("deferred receipt lost: %v %v", pending, err)
	}
	if err := svc.reconcileCaptureAcknowledgments(ctx, cfg); err == nil {
		t.Fatal("deferred handoff did not schedule another reconciliation")
	}
	complete.Store(true)
	if err := svc.reconcileCaptureAcknowledgments(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	pending, err = svc.store.ListPendingHarnessCaptureReceipts(ctx, cfg.ProjectID, cfg.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed receipt still pending: %v %v", pending, err)
	}
}
