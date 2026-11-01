package sandboxes

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sourcecache"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// recordingProvider is a no-op sandbox.Provider that records the sentinel set
// passed to Update.
type recordingProvider struct {
	updates []sandbox.UpdateOptions
	updated [][]string
	routes  []map[string][][]secretroute.Route
	// updateErrs fail the next updates, one each, in order.
	updateErrs []error
	// egress records each egress policy pushed, and updateErr is what Update
	// answers with.
	egress []egress.Policy
	// gateways records each set of gateways pushed.
	gateways  [][]gateway.Route
	updateErr error
	createErr error
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
	return nil, nil, p.createErr
}
func (p *recordingProvider) Update(_ context.Context, _ sandbox.SandboxRef, state []byte, opts sandbox.UpdateOptions) (*sandbox.Sandbox, []byte, error) {
	p.updates = append(p.updates, opts)
	if len(p.updateErrs) > 0 {
		err := p.updateErrs[0]
		p.updateErrs = p.updateErrs[1:]
		if err != nil {
			return nil, state, err
		}
	}
	if p.updateErr != nil {
		return nil, state, p.updateErr
	}
	if opts.Sentinels != nil {
		p.updated = append(p.updated, append([]string{}, opts.Sentinels...))
		p.routes = append(p.routes, opts.SentinelRoutes)
	}
	if opts.Egress != nil {
		p.egress = append(p.egress, *opts.Egress)
	}
	if opts.Gateways != nil {
		p.gateways = append(p.gateways, opts.Gateways)
	}
	return nil, state, nil
}
func (p *recordingProvider) Start(context.Context, sandbox.SandboxRef, []byte) ([]byte, error) {
	return nil, nil
}
func (p *recordingProvider) Stop(context.Context, sandbox.SandboxRef, []byte, time.Duration) ([]byte, error) {
	return nil, nil
}

func (p *recordingProvider) PowerDown(_ context.Context, _ sandbox.SandboxRef, state []byte, _ sandbox.PowerDownReason) (sandbox.Wake, []byte, error) {
	return sandbox.Wake{}, state, nil
}

func (p *recordingProvider) Suspend(_ context.Context, _ sandbox.SandboxRef, state []byte) ([]byte, error) {
	return state, nil
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
func (p *recordingProvider) EnsureRunning(context.Context, sandbox.SandboxRef, []byte) error {
	return nil
}

func (p *recordingProvider) AcquireHTTPClient(context.Context, sandbox.SandboxRef, []byte, []string, sandbox.Use) (*transport.HTTPClientLease, error) {
	return nil, nil
}

func (*recordingProvider) ExportTree(context.Context, sandbox.SandboxRef, string, sandbox.Delivery, []byte) (io.ReadCloser, error) {
	return nil, nil
}

func (*recordingProvider) ImportTree(_ context.Context, _ sandbox.SandboxRef, poolID string, _ platform.Platform, _ io.Reader) (string, error) {
	return poolID, nil
}

func newAssignFixture(t *testing.T) (*Service, *recordingProvider) {
	t.Helper()
	svc, _ := newBindingFixture(t)
	rec := &recordingProvider{}
	manager := sandbox.NewProviderManager(svc.store)
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
	bindUse(t, svc.store, sec.ID, model.SecretBindingTargetHarnessConfig, config.ID, "OPENAI_API_KEY")

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

func (p *recordingProvider) PullSource(context.Context, sandbox.SandboxRef, sourcecache.PullRequest) (sourcecache.Delivery, error) {
	panic("unexpected source pull")
}

// A harness config given to a sandbox for a second harness is attached, not
// copied: removing its binding later reaches the sandbox as it reaches every
// sandbox running the config, and a discobox does not attach one.
func TestAnAttachedHarnessConfigsBindingsStayItsOwn(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAssignFixture(t)
	st := svc.store
	config := codexConfig(t, st)
	sec := bearerSecret(t, st, "openai", "")
	binding := bindUse(t, st, sec.ID, model.SecretBindingTargetHarnessConfig, config.ID, "OPENAI_API_KEY")

	asBox := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sb-1", ProjectID: "project-1"})
	if _, err := svc.AssignSandboxHarnessSecrets(asBox, "project-1", "sb-1", config.ID); err == nil {
		t.Fatal("a discobox attached a harness config")
	}
	if _, err := svc.AssignSandboxHarnessSecrets(ctx, "project-1", "sb-1", config.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	own, err := st.ListSecretBindings(ctx, "project-1", store.SecretBindingFilter{TargetKind: model.SecretBindingTargetSandbox, TargetID: "sb-1"})
	if err != nil || len(own) != 0 {
		t.Fatalf("sandbox bindings = %+v, %v; want none copied", own, err)
	}
	covered, err := st.ListSandboxIDsCoveredBy(ctx, "project-1", model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil || len(covered) != 1 || covered[0] != "sb-1" {
		t.Fatalf("covered = %v, %v; want the attached sandbox", covered, err)
	}
	if _, err := st.DeleteSecretBinding(ctx, "project-1", binding.ID, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	if rows := materialized(t, st, "sb-1"); len(rows) != 0 {
		t.Fatalf("rows = %+v, want the harness config's removal to reach the sandbox", rows)
	}
}

// A raw binding's row is materialized with its delivery, and its sentinel is
// only the pool's handle (ADR 26-10-27-840): pushed as a raw secret, never
// among the sentinels, and never handed to a harness run in the sandbox.
func TestARawBindingIsPushedRawAndNeverAsASentinel(t *testing.T) {
	ctx := context.Background()
	svc, rec := newAssignFixture(t)
	config := codexConfig(t, svc.store)
	sec := bearerSecret(t, svc.store, "openai", "")
	sec.AllowRaw = true
	if err := svc.store.UpdateSecret(ctx, sec); err != nil {
		t.Fatal(err)
	}
	binding := &model.SecretBinding{ProjectID: "project-1", SecretID: sec.ID, TargetKind: model.SecretBindingTargetHarnessConfig, TargetID: config.ID, Use: true, EnvName: "OPENAI_API_KEY", Raw: "file"}
	if err := svc.store.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.AssignSandboxHarnessSecrets(ctx, "project-1", "sb-1", config.ID)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, ok := got["OPENAI_API_KEY"]; ok {
		t.Fatalf("env = %#v, want no sentinel for a raw binding", got)
	}
	rows, err := svc.store.ListSandboxSecrets(ctx, "project-1", "sb-1")
	if err != nil || len(rows) != 1 || rows[0].Raw != "file" || rows[0].BindingID != binding.ID {
		t.Fatalf("rows = %+v, %v, want one raw file row", rows, err)
	}
	if len(rec.updates) != 1 {
		t.Fatalf("updates = %d, want one push", len(rec.updates))
	}
	pushed := rec.updates[0]
	if len(pushed.Sentinels) != 0 || len(pushed.SecretEnv) != 0 {
		t.Fatalf("pushed sentinels %v and env %v, want none for a raw binding", pushed.Sentinels, pushed.SecretEnv)
	}
	if raw := pushed.RawSecrets[rows[0].Sentinel]; raw != (RawSecret{EnvName: "OPENAI_API_KEY", Delivery: "file"}) {
		t.Fatalf("pushed raw = %#v, want the row's delivery by its sentinel", pushed.RawSecrets)
	}
	if len(pushed.SentinelRoutes[rows[0].Sentinel]) == 0 {
		t.Fatalf("pushed routes = %#v, want the raw sentinel's route lists", pushed.SentinelRoutes)
	}
}
