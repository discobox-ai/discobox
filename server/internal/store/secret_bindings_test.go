package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// bindingFixture is a project with a harness config, two sandboxes on it and
// a secret, for the binding tests.
func bindingFixture(t *testing.T) (*store.Store, *model.Sandbox, *model.Sandbox, *model.Secret) {
	t.Helper()
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)
	createTestPool(t, s, "project-1", "pool-1")
	config := &model.HarnessConfig{ID: "hc-1", ProjectID: "project-1", Slug: "codex", Name: "Codex", RunCommand: []string{"codex"}}
	if err := s.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	var sandboxes []*model.Sandbox
	for _, id := range []string{"sb-1", "sb-2"} {
		sandbox := &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: id,
			SandboxManifest: model.SandboxManifest{HarnessConfigID: &config.ID}}
		if err := s.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		sandboxes = append(sandboxes, sandbox)
	}
	secret := &model.Secret{ID: "sec-1", ProjectID: "project-1", Name: "github", Type: model.SecretTypeToken,
		Routes: []model.SecretRoute{{Hosts: []string{"github.com"}}}, EncryptedValue: []byte(`{"token":"t"}`)}
	if err := s.CreateSecret(ctx, secret); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	return s, sandboxes[0], sandboxes[1], secret
}

func createBinding(t *testing.T, s *store.Store, binding *model.SecretBinding) *model.SecretBinding {
	t.Helper()
	binding.ProjectID = "project-1"
	if err := s.CreateSecretBinding(context.Background(), binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	return binding
}

// Per variable the narrowest level that binds it wins, with no stacking; a
// delegation at any level applies; a lapsed binding, and one handed on under
// a lapsed parent, is not there (ADR 26-10-27-880 §2, §3).
func TestResolveBindingsNarrowestWins(t *testing.T) {
	ctx := context.Background()
	s, sb1, sb2, secret := bindingFixture(t)
	now := time.Now().UTC()
	lapsed := now.Add(-time.Minute)
	project := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "GH_TOKEN"})
	harness := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetHarnessConfig, TargetID: "hc-1", Use: true, EnvName: "GH_TOKEN"})
	createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "OTHER", ExpiresAt: &lapsed})
	delegate := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb1.ID, Delegate: true, ExpiresAt: &lapsed})
	own := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb2.ID, Use: true, EnvName: "GH_TOKEN"})
	createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb2.ID, Use: true, EnvName: "CHILD", ParentID: delegate.ID})

	one, err := s.ResolveSandboxBindings(ctx, sb1, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Use) != 1 || one.Use[0].Binding.ID != harness.ID || len(one.Delegate) != 0 {
		t.Fatalf("sb-1 = %+v, want the harness config's GH_TOKEN alone", one)
	}
	two, err := s.ResolveSandboxBindings(ctx, sb2, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(two.Use) != 1 || two.Use[0].Binding.ID != own.ID {
		t.Fatalf("sb-2 = %+v, want its own GH_TOKEN and nothing under a lapsed parent", two)
	}
	atProject, err := s.ResolveBindings(ctx, "project-1", store.BindingTargetsAt("project-1", model.SecretBindingTargetProject, "project-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(atProject.Use) != 1 || atProject.Use[0].Binding.ID != project.ID {
		t.Fatalf("project = %+v, want its own GH_TOKEN", atProject)
	}
}

// Removing a binding removes what was handed on under it, each recorded.
func TestDeleteSecretBindingCascades(t *testing.T) {
	ctx := context.Background()
	s, sb1, sb2, secret := bindingFixture(t)
	parent := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb1.ID, Delegate: true})
	child := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb2.ID, Use: true, EnvName: "GH_TOKEN", ParentID: parent.ID})

	deleted, err := s.DeleteSecretBinding(ctx, "project-1", parent.ID, store.BindingChange{Actor: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 || deleted[0].ID != parent.ID || deleted[1].ID != child.ID {
		t.Fatalf("deleted = %+v, want the parent then its child", deleted)
	}
	events, err := s.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: child.ID})
	if err != nil || len(events) != 2 || events[0].Event != model.SecretBindingEventDeleted ||
		events[0].Actor != model.SecretBindingActorSystem || events[0].Cause != parent.ID {
		t.Fatalf("child events = %+v, %v; want its removal caused by its parent", events, err)
	}
}

// Every sentinel has route lists, since a pool sends one with none nowhere:
// the secret's, the binding's when it has any, its parent's when it has any;
// one whose binding is gone gets a single empty list (ADR 26-10-27-880 §4).
func TestSentinelRouteListsCoverEverySentinel(t *testing.T) {
	ctx := context.Background()
	s, sb1, sb2, secret := bindingFixture(t)
	parent := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb1.ID, Delegate: true,
		Routes: []model.SecretRoute{{Hosts: []string{"api.github.com"}}}})
	child := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb2.ID, Use: true, EnvName: "GH_TOKEN", ParentID: parent.ID})
	rows := []model.SandboxSecret{
		{SecretID: secret.ID, BindingID: child.ID, Sentinel: "S-CHILD"},
		{SecretID: secret.ID, BindingID: "bind_gone", Sentinel: "S-GONE"},
		{SecretID: secret.ID, Sentinel: "S-NONE"},
	}
	got, err := s.SentinelRouteLists(ctx, "project-1", rows)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][][]secretroute.Route{
		"S-CHILD": {{{Hosts: []string{"github.com"}}}, {{Hosts: []string{"api.github.com"}}}},
		"S-GONE":  {{}},
		"S-NONE":  {{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("route lists = %+v, want %+v", got, want)
	}
}

// Every binding written with an expiry arms the expiry sweep at that instant,
// in the transaction that writes it, whoever writes it.
func TestABindingWithAnExpiryArmsTheSweep(t *testing.T) {
	ctx := context.Background()
	s, sb1, _, secret := bindingFixture(t)
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	binding := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb1.ID, Use: true, EnvName: "GH", ExpiresAt: &expires})
	read := func() []time.Time {
		t.Helper()
		var at []time.Time
		if err := s.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
			return tx.Table("reconcile_dirty").Where("resource_type = ? AND resource_id = ?", store.BindingExpiryResourceType, store.BindingExpiryID).Pluck("not_before", &at).Error
		}); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if at := read(); len(at) != 1 || !at[0].Equal(expires) {
		t.Fatalf("sweep armed at %v, want %v", at, expires)
	}
	sooner := expires.Add(-30 * time.Minute)
	binding.ExpiresAt = &sooner
	if err := s.UpdateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	if at := read(); len(at) != 1 || !at[0].Equal(sooner) {
		t.Fatalf("sweep armed at %v after shortening, want %v", at, sooner)
	}
}

// An attachment ends with its harness config: deleting the config hands its
// deleted bindings to the caller to mark, and no sandbox names it after.
func TestDeletingAHarnessConfigEndsItsAttachments(t *testing.T) {
	ctx := context.Background()
	s, sb1, _, secret := bindingFixture(t)
	other := &model.HarnessConfig{ID: "hc-other", ProjectID: "project-1", Slug: "other", Name: "Other", RunCommand: []string{"other"}}
	if err := s.CreateHarnessConfig(ctx, other); err != nil {
		t.Fatal(err)
	}
	binding := createBinding(t, s, &model.SecretBinding{SecretID: secret.ID, TargetKind: model.SecretBindingTargetHarnessConfig, TargetID: other.ID, Use: true, EnvName: "GH"})
	if attached, err := s.AttachSandboxHarnessConfig(ctx, "project-1", sb1.ID, other.ID); err != nil || !attached {
		t.Fatalf("attach = %v, %v", attached, err)
	}
	var handed []string
	if err := s.DeleteHarnessConfig(ctx, "project-1", other.ID, func(txStore *store.Store, _ *gorm.DB, deleted []model.SecretBinding) error {
		covered, err := txStore.ListSandboxIDsCoveredBy(ctx, "project-1", model.SecretBindingTargetHarnessConfig, other.ID)
		if err != nil || len(covered) != 1 || covered[0] != sb1.ID {
			t.Errorf("covered while deleting = %v, %v; want the attached sandbox, to mark it", covered, err)
		}
		for _, d := range deleted {
			handed = append(handed, d.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(handed) != 1 || handed[0] != binding.ID {
		t.Fatalf("handed = %v, want the config's binding", handed)
	}
	after, err := s.GetSandbox(ctx, "project-1", sb1.ID)
	if err != nil || len(after.AttachedHarnessConfigIDs) != 0 {
		t.Fatalf("attached = %v, %v; want none", after.AttachedHarnessConfigIDs, err)
	}
}

// Every event's snapshot names the secret, whichever writer made the change.
func TestBindingEventsNameTheSecret(t *testing.T) {
	ctx := context.Background()
	s, sb1, _, secret := bindingFixture(t)
	binding := &model.SecretBinding{ProjectID: "project-1", SecretID: secret.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: sb1.ID, Use: true, EnvName: "GH"}
	if err := s.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	binding.Routes = []model.SecretRoute{{Hosts: []string{"api.github.com"}}}
	if err := s.UpdateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSecretBinding(ctx, "project-1", binding.ID, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: binding.ID})
	if err != nil || len(events) != 3 {
		t.Fatalf("events = %+v, %v; want three", events, err)
	}
	for _, event := range events {
		if event.Binding.SecretName != "github" {
			t.Fatalf("%s snapshot names %q, want github", event.Event, event.Binding.SecretName)
		}
	}
}

// A binding's events read newest first even when the clock does not move
// between them, as Windows' coarse clock often does not.
func TestBindingEventsKeepTheirOrderWithinOneClockTick(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)
	createSecret(t, s, "sec-tick", "tick")
	binding := &model.SecretBinding{ProjectID: "project-1", SecretID: "sec-tick", TargetKind: model.SecretBindingTargetProject, TargetID: "project-1", Use: true, EnvName: "TICK"}
	if err := s.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSecretBinding(ctx, "project-1", binding.ID, store.BindingChange{Actor: "user-1"}); err != nil {
		t.Fatal(err)
	}
	// Both in one tick, as the clock would have it, and the delete's ID
	// sorting below the create's, so neither the time nor the ID orders them.
	tick := time.Now().UTC()
	if err := db.Write.WithContext(ctx).Model(&model.SecretBindingEvent{}).Where("binding_id = ?", binding.ID).
		Update("at", tick).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Write.WithContext(ctx).Model(&model.SecretBindingEvent{}).Where("binding_id = ? AND event = ?", binding.ID, model.SecretBindingEventDeleted).
		Update("id", "bev_0").Error; err != nil {
		t.Fatal(err)
	}
	events, err := s.ListSecretBindingEvents(ctx, "project-1", store.SecretBindingEventFilter{BindingID: binding.ID})
	if err != nil || len(events) != 2 || events[0].Event != model.SecretBindingEventDeleted || !events[0].At.Equal(tick) {
		t.Fatalf("events = %+v, %v; want the delete first, at the clock's own time", events, err)
	}
}
