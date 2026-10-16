package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// putGateway stores a gateway on project-1.
func putGateway(t *testing.T, st *store.Store, name string) {
	t.Helper()
	gw := &model.ProjectGateway{ProjectID: "project-1", Name: name, Match: "https://api.anthropic.com/v1", Target: "https://gw.example.com/anthropic/v1"}
	if err := st.PutProjectGateway(context.Background(), gw); err != nil {
		t.Fatal(err)
	}
}

// egressFixture stores one level of each kind and returns a sandbox on them.
func egressFixture(t *testing.T, st *store.Store, project, pool, harnessLevel, own egress.Policy, imageHosts ...string) *model.Sandbox {
	t.Helper()
	ctx := context.Background()
	ensurePool(t, st)
	if err := st.SetProjectEgress(ctx, "project-1", project); err != nil {
		t.Fatal(err)
	}
	pl, err := st.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pl.Egress = pool
	if err := st.UpdatePool(ctx, pl); err != nil {
		t.Fatal(err)
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "claude-code", Name: "Claude Code", Configured: true,
		Egress: harnessLevel, ImageEgressHosts: imageHosts,
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	return eligibleSandbox(t, st, config.ID, "image", "sha256:x", func(sb *model.Sandbox) { sb.Egress = own })
}

func TestEffectiveEgressMergesEveryLevel(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                        string
		project, pool, harness, own egress.Policy
		imageHosts                  []string
		want                        egress.Policy
	}{
		{
			name:       "no level restricts: allow lists have no effect",
			project:    egress.Policy{Allow: []string{"github.com"}},
			imageHosts: []string{"api.anthropic.com"},
			want:       egress.Policy{Allow: []string{"api.anthropic.com", "github.com"}},
		},
		{
			name:       "the pool restricts, and every level's hosts are reachable",
			project:    egress.Policy{Allow: []string{"github.com"}},
			pool:       egress.Policy{DenyByDefault: true},
			harness:    egress.Policy{Allow: []string{"registry.npmjs.org"}},
			own:        egress.Policy{Allow: []string{"10.0.0.0/8"}},
			imageHosts: []string{"api.anthropic.com"},
			want:       egress.Policy{Allow: []string{"10.0.0.0/8", "api.anthropic.com", "github.com", "registry.npmjs.org"}, DenyByDefault: true},
		},
		{
			name: "the sandbox's own level restricts it to nothing",
			own:  egress.Policy{DenyByDefault: true},
			want: egress.Policy{DenyByDefault: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, st := newUpgradeEngineFixture(t)
			sb := egressFixture(t, st, tc.project, tc.pool, tc.harness, tc.own, tc.imageHosts...)
			got, err := st.EffectiveSandboxEgress(ctx, sb)
			if err != nil {
				t.Fatal(err)
			}
			if got.DenyByDefault != tc.want.DenyByDefault || !slices.Equal(got.Allow, tc.want.Allow) {
				t.Fatalf("effective = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The policy rides every create the reconciler sends, so a level change
// reaches a sandbox that is up through the ensure it marks.
func TestCreateOptionsCarryTheEffectiveEgress(t *testing.T) {
	_, st := newUpgradeEngineFixture(t)
	sb := egressFixture(t, st, egress.Policy{DenyByDefault: true}, egress.Policy{}, egress.Policy{}, egress.Policy{}, "api.anthropic.com")
	reconciler := &SandboxReconciler{store: st}
	opts, err := reconciler.createOptionsFromSandbox(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Egress.DenyByDefault || !slices.Equal(opts.Egress.Allow, []string{"api.anthropic.com"}) {
		t.Fatalf("create options egress = %+v", opts.Egress)
	}
	if opts.Gateways == nil || len(opts.Gateways) != 0 {
		t.Fatalf("create options gateways = %#v, want none", opts.Gateways)
	}
	putGateway(t, st, "anthropic")
	if opts, err = reconciler.createOptionsFromSandbox(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	if len(opts.Gateways) != 1 || opts.Gateways[0].Name != "anthropic" {
		t.Fatalf("create options gateways = %+v, want the project's", opts.Gateways)
	}
}

func TestADiscoboxMayNotCreateOneWiderThanItself(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	creator := egressFixture(t, st, egress.Policy{}, egress.Policy{}, egress.Policy{}, egress.Policy{DenyByDefault: true, Allow: []string{"*.github.com"}}, "api.anthropic.com")
	child := func(own egress.Policy) *model.Sandbox {
		return &model.Sandbox{ProjectID: "project-1", PoolID: "pool-1", SandboxManifest: creator.SandboxManifest, Egress: own}
	}
	for _, tc := range []struct {
		name string
		own  egress.Policy
		ok   bool
	}{
		{name: "same policy", own: egress.Policy{DenyByDefault: true, Allow: []string{"*.github.com"}}, ok: true},
		{name: "narrower", own: egress.Policy{DenyByDefault: true, Allow: []string{"api.github.com"}}, ok: true},
		{name: "unrestricted", own: egress.Policy{}, ok: false},
		{name: "an extra host", own: egress.Policy{DenyByDefault: true, Allow: []string{"evil.example"}}, ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.refuseWiderThanCreator(ctx, creator.ID, child(tc.own))
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var statusErr interface{ StatusCode() int }
			if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
				t.Fatalf("err = %v, want forbidden", err)
			}
		})
	}
}

func TestMarkEgressChangedMarksOnlyPresentSandboxesInScope(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	ensurePool(t, st)
	if err := st.CreatePool(ctx, &model.Pool{ID: "pool-2", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-2", ProviderInstanceID: "provider-1"}}); err != nil {
		t.Fatal(err)
	}
	inPool := eligibleSandbox(t, st, "", "image", "sha256:x")
	otherPool := eligibleSandbox(t, st, "", "image", "sha256:x", func(sb *model.Sandbox) { sb.PoolID = "pool-2" })
	archived := eligibleSandbox(t, st, "", "image", "sha256:x", func(sb *model.Sandbox) { sb.DesiredState = model.DesiredStateArchived })

	if err := st.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		return svc.MarkProxyPolicyChangedTx(ctx, txStore, txDB, "project-1", "pool-1", "")
	}); err != nil {
		t.Fatal(err)
	}
	marked := dirtySandboxIDs(t, svc)
	if !slices.Contains(marked, SandboxDirtyID("project-1", inPool.ID)) {
		t.Fatalf("the pool's sandbox was not marked: %v", marked)
	}
	for _, sb := range []*model.Sandbox{otherPool, archived} {
		if slices.Contains(marked, SandboxDirtyID("project-1", sb.ID)) {
			t.Fatalf("%s was marked: %v", sb.Name, marked)
		}
	}
}

func TestUpdateSandboxEgressMarksIt(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	sb := eligibleSandbox(t, st, "", "image", "sha256:x")

	update := func(allow ...string) services.UpdateSandboxBody {
		return services.UpdateSandboxBody{Config: serverapi.NewOptSandboxUpdateConfig(serverapi.SandboxUpdateConfig{
			Egress: serverapi.NewOptEgressPolicy(serverapi.EgressPolicy{
				Allow:         serverapi.NewOptNilStringArray(allow),
				DenyByDefault: serverapi.NewOptBool(true),
			}),
		})}
	}
	body := update("GitHub.com")
	updated, err := svc.UpdateSandbox(ctx, "project-1", sb.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Egress.DenyByDefault || !slices.Equal(updated.Egress.Allow, []string{"github.com"}) {
		t.Fatalf("stored egress = %+v", updated.Egress)
	}
	if !slices.Contains(dirtySandboxIDs(t, svc), SandboxDirtyID("project-1", sb.ID)) {
		t.Fatal("the sandbox's egress delivery was not marked")
	}

	_, err = svc.UpdateSandbox(ctx, "project-1", sb.ID, update("github.com:443"))
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("an entry with a port: err = %v, want bad request", err)
	}
}

func dirtySandboxIDs(t *testing.T, svc *Service) []string {
	t.Helper()
	rows, err := svc.engine.ListDirty(context.Background(), SandboxProxyPolicyResourceType)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ResourceID)
	}
	return ids
}

// A reconcile that loaded the row before a person changed the sandbox's level
// writes the row back under its generation guard, which the change did not
// move. The level must survive that write.
func TestAStaleRowWriteDoesNotRevertTheEgressLevel(t *testing.T) {
	ctx := context.Background()
	_, st := newUpgradeEngineFixture(t)
	sb := eligibleSandbox(t, st, "", "image", "sha256:x")
	stale, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	tightened := egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}}
	if err := st.SetSandboxEgress(ctx, "project-1", sb.ID, tightened); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateSandbox(ctx, stale, store.WithGeneration(stale.Generation)); err != nil {
		t.Fatalf("reconciler write: %v", err)
	}
	got, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Egress.DenyByDefault || !slices.Equal(got.Egress.Allow, tightened.Allow) {
		t.Fatalf("egress after a stale write = %+v, want %+v", got.Egress, tightened)
	}
}

// The egress reconcile pushes the merged policy alone, to a sandbox whatever
// its lifecycle is doing — a settled failure included — and leaves the
// sandbox's own state alone when the pool cannot take it, so the engine retries.
func TestEgressReconcilePushesTheMergedPolicy(t *testing.T) {
	ctx := context.Background()
	svc, rec := newAssignFixture(t)
	st := svc.store
	if err := st.SetProjectEgress(ctx, "project-1", egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}}); err != nil {
		t.Fatal(err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	sb.DesiredState = model.DesiredStatePresent
	sb.RecordFailure(model.SandboxStateFailed, "pool went away", "")
	sb.ObservedGeneration = sb.Generation
	if err := st.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	reconciler := &sandboxProxyPolicyReconciler{store: st, providers: svc.sandboxProviders}
	id := SandboxDirtyID("project-1", "sb-1")

	if _, err := reconciler.Reconcile(ctx, id); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.egress) != 1 || !rec.egress[0].DenyByDefault || !slices.Equal(rec.egress[0].Allow, []string{"github.com"}) {
		t.Fatalf("pushed %+v, want the project's restriction", rec.egress)
	}
	// No gateways is pushed as none, which takes away any the pool holds.
	if len(rec.gateways) != 1 || rec.gateways[0] == nil || len(rec.gateways[0]) != 0 {
		t.Fatalf("pushed gateways %#v, want an empty list", rec.gateways)
	}
	putGateway(t, st, "anthropic")
	if _, err := reconciler.Reconcile(ctx, id); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(rec.gateways) != 2 || len(rec.gateways[1]) != 1 || rec.gateways[1][0].Match != "https://api.anthropic.com/v1" {
		t.Fatalf("pushed gateways %+v, want the project's", rec.gateways)
	}
	rec.egress = rec.egress[:1]
	if len(rec.updated) != 0 {
		t.Fatalf("an egress push touched the sentinels: %v", rec.updated)
	}

	rec.updateErr = errors.New("pool unreachable")
	if _, err := reconciler.Reconcile(ctx, id); err == nil {
		t.Fatal("an unreachable pool settled the push; want an error the engine retries")
	}
	after, err := st.GetSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if after.ErrorMessage == nil || *after.ErrorMessage != "pool went away" || after.Generation != sb.Generation {
		t.Fatalf("the push wrote to the sandbox: error %v generation %d", after.ErrorMessage, after.Generation)
	}

	rec.updateErr = ErrNotFound
	if _, err := reconciler.Reconcile(ctx, id); err != nil {
		t.Fatalf("a sandbox never built: %v, want settled", err)
	}

	rec.updateErr = nil
	after.DesiredState = model.DesiredStateArchived
	if err := st.UpdateSandbox(ctx, after); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, id); err != nil {
		t.Fatalf("archived: %v", err)
	}
	if len(rec.egress) != 1 {
		t.Fatalf("an archived sandbox was pushed a policy: %+v", rec.egress)
	}
}

// A create that fails late may still have put its policy, merged before a
// level change, over the push that change made. Its push is queued anyway,
// with the write that records the failure — the write a push needs first.
func TestAFailedCreateStillQueuesTheEgressPush(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	rec := &recordingProvider{createErr: errors.New("container failed to start")}
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("test", rec)
	manager.SetDefault("test")
	svc.sandboxProviders = manager
	if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-t", ProjectID: "project-1", Type: "test", Name: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePool(ctx, &model.Pool{ID: "pool-t", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "pool-t", ProviderInstanceID: "prov-t"}}); err != nil {
		t.Fatal(err)
	}
	sb := &model.Sandbox{ProjectID: "project-1", PoolID: "pool-t", CreatedByUserID: "user-1", Name: "failing",
		SandboxManifest: model.SandboxManifest{HarnessMode: "run", Image: "image"}}
	sb.DesiredState = model.DesiredStatePresent
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.GetSandbox(ctx, "project-1", sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.NewSandboxReconciler().ReconcileSandbox(ctx, loaded); err == nil || err.Error() != "container failed to start" {
		t.Fatalf("reconcile error = %v, want the provider's failure", err)
	}
	if !slices.Contains(dirtySandboxIDs(t, svc), SandboxDirtyID("project-1", sb.ID)) {
		t.Fatal("a failed create did not queue the egress push")
	}
}

// A whole-row save of a project or a harness config — a default being set,
// the configure flow committing — loaded before a person changed its egress
// level must not save the old level back.
func TestAStaleWholeRowSaveDoesNotRevertProjectOrHarnessEgress(t *testing.T) {
	ctx := context.Background()
	_, st := newUpgradeEngineFixture(t)
	tightened := egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}}

	staleProject, err := st.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProjectEgress(ctx, "project-1", tightened); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProject(ctx, staleProject); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetProject(ctx, "project-1"); !got.Egress.DenyByDefault {
		t.Fatalf("project egress after a stale save = %+v", got.Egress)
	}

	config := &model.HarnessConfig{ProjectID: "project-1", Slug: "custom", Name: "Custom"}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	staleConfig, err := st.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetHarnessConfigEgress(ctx, "project-1", config.ID, tightened); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateHarnessConfig(ctx, staleConfig); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetHarnessConfig(ctx, "project-1", config.ID); !got.Egress.DenyByDefault {
		t.Fatalf("harness egress after a stale save = %+v", got.Egress)
	}
}
