package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// recordingProvider is a no-op sandbox.Provider that records the sentinel set
// passed to Update.
type recordingProvider struct {
	updated [][]string
}

func (p *recordingProvider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	return nil
}
func (p *recordingProvider) Close() error { return nil }
func (p *recordingProvider) Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{}
}
func (p *recordingProvider) Status() sandbox.ProviderStatus  { return sandbox.ProviderStatus{} }
func (p *recordingProvider) Reconcile(context.Context) error { return nil }
func (p *recordingProvider) RemoveProject(context.Context, string) error {
	return nil
}
func (p *recordingProvider) List(context.Context) ([]*sandbox.Sandbox, error) { return nil, nil }
func (p *recordingProvider) Create(context.Context, sandbox.SandboxRef, []byte, sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	return nil, nil, nil
}
func (p *recordingProvider) Update(_ context.Context, _ sandbox.SandboxRef, state []byte, opts sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	p.updated = append(p.updated, append([]string{}, opts.Sentinels...))
	return nil, state, nil
}
func (p *recordingProvider) Start(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}
func (p *recordingProvider) Stop(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (p *recordingProvider) Restart(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}
func (p *recordingProvider) Archive(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}

func (p *recordingProvider) Remove(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}
func (p *recordingProvider) Get(context.Context, sandbox.SandboxRef, []byte) (*sandbox.Sandbox, error) {
	return nil, nil
}
func (p *recordingProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string) (*transport.HTTPClientLease, error) {
	return nil, nil
}

func newAssignFixture(t *testing.T) (*Service, *recordingProvider) {
	t.Helper()
	svc, _ := newBindingFixture(t)
	rec := &recordingProvider{}
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("test", rec)
	manager.SetDefault("test")
	svc.sandboxProviders = manager
	// Provider resolution is sandbox → pool → provider instance, so the fixture
	// pool's backing instance carries the registered "test" provider type.
	if err := svc.store.CreateSandboxProviderInstance(context.Background(), &model.SandboxProviderInstance{
		ID: "prov-1", ProjectID: "project-1", Type: "test", Name: "test",
	}); err != nil {
		t.Fatalf("create provider instance: %v", err)
	}
	if err := svc.store.CreatePool(context.Background(), &model.Pool{
		ID:        "pool-1",
		ProjectID: "project-1",
		PoolManifest: model.PoolManifest{
			Name:               "pool-1",
			ProviderInstanceID: "prov-1",
		},
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if err := svc.store.CreateSandbox(context.Background(), &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "sb-1",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	return svc, rec
}

func TestAssignSandboxHarnessSecretsMintsPushesAndReuses(t *testing.T) {
	ctx := context.Background()
	svc, rec := newAssignFixture(t)
	config := codexConfig(t, svc.store) // declares OPENAI_API_KEY required
	sec := bearerSecret(t, svc.store, "openai", "")
	if err := svc.store.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: config.ID, EnvName: "OPENAI_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	got, err := svc.AssignSandboxHarnessSecrets(ctx, "project-1", "sb-1", config.ID)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	sentinel := got["OPENAI_API_KEY"]
	if sentinel == "" || sentinel == "sk-abc" {
		t.Fatalf("env = %q, want a sentinel that is not the raw value", sentinel)
	}
	if len(rec.updated) != 1 || len(rec.updated[0]) != 1 || rec.updated[0][0] != sentinel {
		t.Fatalf("provider.Update sentinels = %#v, want [%q]", rec.updated, sentinel)
	}

	// A second assignment for the same env reuses the sentinel and does not push.
	got2, err := svc.AssignSandboxHarnessSecrets(ctx, "project-1", "sb-1", config.ID)
	if err != nil {
		t.Fatalf("assign again: %v", err)
	}
	if got2["OPENAI_API_KEY"] != sentinel {
		t.Fatalf("reuse sentinel = %q, want %q", got2["OPENAI_API_KEY"], sentinel)
	}
	if len(rec.updated) != 1 {
		t.Fatalf("second assign pushed again: %#v", rec.updated)
	}
}

func TestAssignSandboxHarnessSecretsBlocksUnboundRequired(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAssignFixture(t)
	config := codexConfig(t, svc.store) // OPENAI_API_KEY required, no binding

	_, err := svc.AssignSandboxHarnessSecrets(ctx, "project-1", "sb-1", config.ID)
	if err == nil {
		t.Fatal("expected 400 for unbound required secret")
	}
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want 400", err)
	}
}
