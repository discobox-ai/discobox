package harnessconfigs

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

func newVariant(t *testing.T, svc *Service, parent *model.HarnessConfig, slug string) *model.HarnessConfig {
	t.Helper()
	cfg, err := svc.CreateHarnessConfig(context.Background(), parent.ProjectID, services.CreateHarnessConfigBody{Extends: apigen.NewOptString(parent.ID), Slug: apigen.NewOptString(slug)})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
func variantRoot(t *testing.T, svc *Service) *model.HarnessConfig {
	t.Helper()
	cfg, err := svc.CreateHarnessConfig(context.Background(), "project-1", services.CreateHarnessConfigBody{Bundle: apigen.NewOptString("example.com/acme:v1")})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestVariantFollowsWholeSnapshotAndKeepsAccount(t *testing.T) {
	ctx := context.Background()
	svc, inspector := bundleService(t)
	parent := variantRoot(t, svc)
	parent.ConfiguredFiles = []model.HarnessConfigFile{{Path: "account", Content: "parent"}}
	parent.FileOverrides = []model.HarnessConfigFile{{Path: "personal", Content: "parent"}}
	if err := svc.store.UpdateHarnessConfig(ctx, parent); err != nil {
		t.Fatal(err)
	}
	child := newVariant(t, svc, parent, "acme-work")
	if len(child.ConfiguredFiles) != 0 || len(child.FileOverrides) != 0 {
		t.Fatal("parent personal files inherited")
	}
	child.ConfiguredFiles = []model.HarnessConfigFile{{Path: "account", Content: "child"}}
	child.FileOverrides = []model.HarnessConfigFile{{Path: "personal", Content: "child"}}
	if err := svc.store.UpdateHarnessConfig(ctx, child); err != nil {
		t.Fatal(err)
	}
	stale := *child
	runtime := &stubSandboxRuntime{}
	svc.sandboxes = runtime
	// A manifest-only change must reach the child even though its bundle digest stays put.
	metadata := inspector.byImage[parent.Bundle]
	metadata.Env = map[string]string{"NEW": "yes"}
	metadata.Harness = &harness.Image{RunCommand: []string{"new-run"}, Files: []harness.File{{Path: "baseline", Content: "new"}}}
	inspector.byImage[parent.Bundle] = metadata
	if _, err := svc.RefreshHarnessConfigImage(ctx, parent.ProjectID, parent.ID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.store.GetHarnessConfig(ctx, child.ProjectID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BundleDigest != child.BundleDigest || got.ManifestDigest == child.ManifestDigest || got.Env["NEW"] != "yes" || !slices.Equal(got.RunCommand, []string{"new-run"}) || len(got.Files) != 1 {
		t.Fatalf("snapshot not propagated: %+v", got)
	}
	if !reflect.DeepEqual(got.ConfiguredFiles, child.ConfiguredFiles) || !reflect.DeepEqual(got.FileOverrides, child.FileOverrides) {
		t.Fatal("child account changed")
	}
	if len(runtime.upgraded) != 2 || !slices.Contains(runtime.upgraded, child.ID) {
		t.Fatalf("fanout %v", runtime.upgraded)
	}
	if err := svc.store.UpdateHarnessConfig(ctx, &stale); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	revision := got.Revision
	if _, err := svc.RefreshHarnessConfigImage(ctx, parent.ProjectID, parent.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.store.GetHarnessConfig(ctx, child.ProjectID, child.ID)
	if got.Revision != revision {
		t.Fatal("unchanged snapshot bumped child revision")
	}
	if _, err := svc.RefreshHarnessConfigImage(ctx, child.ProjectID, child.ID); err == nil || !strings.Contains(err.Error(), parent.Slug) {
		t.Fatalf("child refresh: %v", err)
	}
}

func TestVariantConversionDetachAndRelationshipValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := bundleService(t)
	root := variantRoot(t, svc)
	child := newVariant(t, svc, root, "work")
	for name, parent := range map[string]string{"self": child.ID, "chain": child.ID, "missing": "absent"} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.UpdateHarnessConfig(ctx, root.ProjectID, root.ID, services.UpdateHarnessConfigBody{Revision: root.Revision, Extends: apigen.NewOptString(parent)})
			if err == nil {
				t.Fatal("invalid relationship accepted")
			}
		})
	}
	if err := svc.DeleteHarnessConfig(ctx, root.ProjectID, root.ID); err == nil || !strings.Contains(err.Error(), child.Slug) {
		t.Fatalf("parent delete: %v", err)
	}
	if _, err := svc.CreateHarnessConfig(ctx, root.ProjectID, services.CreateHarnessConfigBody{Bundle: apigen.NewOptString(root.Bundle), Extends: apigen.NewOptString(root.ID)}); err == nil {
		t.Fatal("both sources accepted")
	}
	// Detach retains identity and personal data, and the old parent becomes deletable.
	child.ConfiguredFiles = []model.HarnessConfigFile{{Path: "account", Content: "work"}}
	if err := svc.store.UpdateHarnessConfig(ctx, child); err != nil {
		t.Fatal(err)
	}
	detached, err := svc.UpdateHarnessConfig(ctx, child.ProjectID, child.ID, services.UpdateHarnessConfigBody{Revision: child.Revision, Bundle: apigen.NewOptString(root.Bundle)})
	if err != nil {
		t.Fatal(err)
	}
	if detached.ExtendsID != nil || detached.ID != child.ID || !reflect.DeepEqual(detached.ConfiguredFiles, child.ConfiguredFiles) {
		t.Fatal("detach lost state")
	}
	converted, err := svc.UpdateHarnessConfig(ctx, child.ProjectID, child.ID, services.UpdateHarnessConfigBody{Revision: detached.Revision, Extends: apigen.NewOptString(root.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if converted.ExtendsID == nil || *converted.ExtendsID != root.ID || !reflect.DeepEqual(converted.ConfiguredFiles, child.ConfiguredFiles) {
		t.Fatal("conversion lost state")
	}
}

func TestShellVariantUsesParentsResolvedConvention(t *testing.T) {
	ctx := context.Background()
	svc, _ := bundleService(t)
	root := &model.HarnessConfig{ProjectID: "project-1", Slug: "shell", Name: "Shell", Bundle: "shell:local", BundleDigest: "sha256:1", FileOverrides: []model.HarnessConfigFile{}}
	snapshotImageMetadata(root, imageMetadata{})
	if err := svc.store.CreateHarnessConfig(ctx, root); err != nil {
		t.Fatal(err)
	}
	child := newVariant(t, svc, root, "shell-work")
	if !slices.Equal(child.RunCommand, root.RunCommand) || !slices.Equal(child.RelaunchCommand, root.RelaunchCommand) {
		t.Fatalf("commands %v/%v, parent %v/%v", child.RunCommand, child.RelaunchCommand, root.RunCommand, root.RelaunchCommand)
	}
}

func TestBuiltInRepinReachesVariant(t *testing.T) {
	ctx := context.Background()
	svc, inspector := bundleService(t)
	svc.harnesses = bundleReferences(map[string]string{"shell": "example.com/acme:v1"})
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatal(err)
	}
	parent, err := svc.store.GetHarnessConfigBySlug(ctx, "project-1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	child := newVariant(t, svc, parent, "shell-work")
	metadata := inspector.byImage[parent.Bundle]
	metadata.BundleDigest = "sha256:repinned"
	inspector.byImage[parent.Bundle] = metadata
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.store.GetHarnessConfig(ctx, child.ProjectID, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BundleDigest != "sha256:repinned" || got.Revision <= child.Revision {
		t.Fatalf("child not repinned: %+v", got)
	}
}

func TestLegacyConversionPreservesFilesAgainstOldBaseline(t *testing.T) {
	ctx := context.Background()
	svc, _ := bundleService(t)
	root := variantRoot(t, svc)
	legacy := &model.HarnessConfig{ProjectID: root.ProjectID, Slug: "legacy", Name: "Legacy", Bundle: root.Bundle, Files: []model.HarnessConfigFile{{Path: "mine", Content: "personal"}}}
	if err := svc.store.CreateHarnessConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := svc.UpdateHarnessConfig(ctx, legacy.ProjectID, legacy.ID, services.UpdateHarnessConfigBody{Revision: legacy.Revision, Extends: apigen.NewOptString(root.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FileOverrides) != 1 || got.FileOverrides[0].Path != "mine" {
		t.Fatalf("lost personal file: %+v", got)
	}
	missing := &model.HarnessConfig{ProjectID: root.ProjectID, Slug: "missing", Name: "Missing", Bundle: "", Files: legacy.Files}
	if err := svc.store.CreateHarnessConfig(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateHarnessConfig(ctx, missing.ProjectID, missing.ID, services.UpdateHarnessConfigBody{Revision: missing.Revision, Extends: apigen.NewOptString(root.ID)}); err == nil {
		t.Fatal("conversion guessed unknown baseline")
	}
}

func TestVariantCreationSerializesWithPublicationAndDeletion(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for range 5 {
			ctx := context.Background()
			svc, inspector := bundleService(t)
			root := variantRoot(t, svc)
			metadata := inspector.byImage[root.Bundle]
			metadata.BundleDigest = "sha256:new"
			inspector.byImage[root.Bundle] = metadata
			start := make(chan struct{})
			created := make(chan error, 1)
			changed := make(chan error, 1)
			go func() {
				<-start
				_, err := svc.CreateHarnessConfig(ctx, root.ProjectID, services.CreateHarnessConfigBody{Extends: apigen.NewOptString(root.ID), Slug: apigen.NewOptString("work")})
				created <- err
			}()
			go func() {
				<-start
				if deleting {
					changed <- svc.DeleteHarnessConfig(ctx, root.ProjectID, root.ID)
				} else {
					_, err := svc.RefreshHarnessConfigImage(ctx, root.ProjectID, root.ID)
					changed <- err
				}
			}()
			close(start)
			createErr, changeErr := <-created, <-changed
			configs, err := svc.store.ListHarnessConfigs(ctx, root.ProjectID)
			if err != nil {
				t.Fatal(err)
			}
			if deleting {
				if createErr == nil && changeErr == nil {
					t.Fatal("deleted a parent with a newly committed child")
				}
				for _, cfg := range configs {
					if cfg.ExtendsID != nil {
						if _, err := svc.store.GetHarnessConfig(ctx, cfg.ProjectID, *cfg.ExtendsID); err != nil {
							t.Fatal("dangling parent")
						}
					}
				}
			} else {
				if createErr != nil || changeErr != nil {
					t.Fatalf("create %v, refresh %v", createErr, changeErr)
				}
				for _, cfg := range configs {
					if cfg.BundleDigest != "sha256:new" {
						t.Fatal("stale snapshot committed")
					}
				}
			}
		}
	}
}

func TestVariantKeepsIndependentBindingsAndRollsBackPublication(t *testing.T) {
	ctx := context.Background()
	svc, inspector := bundleService(t)
	parent := variantRoot(t, svc)
	child := newVariant(t, svc, parent, "work")
	for _, cfg := range []*model.HarnessConfig{parent, child} {
		secret := &model.Secret{ID: "secret-" + cfg.Slug, ProjectID: cfg.ProjectID, Name: "secret-" + cfg.Slug, Type: "token", EncryptedValue: []byte(`{"token":"test"}`)}
		if err := svc.store.CreateSecret(ctx, secret); err != nil {
			t.Fatal(err)
		}
		if err := putHarnessBinding(ctx, svc.store, harnessBinding{ProjectID: cfg.ProjectID, HarnessConfigID: cfg.ID, EnvName: "TOKEN", SecretID: secret.ID}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := svc.store.ListSecretBindings(ctx, child.ProjectID, store.SecretBindingFilter{TargetKind: model.SecretBindingTargetHarnessConfig, TargetID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Fail specifically when the child snapshot is written: the root write must
	// roll back too, and no fan-out may have happened.
	if err := svc.store.Transaction(ctx, func(_ *store.Store, db *gorm.DB) error {
		return db.Exec("CREATE TRIGGER fail_child BEFORE UPDATE ON harness_configs WHEN OLD.slug = 'work' BEGIN SELECT RAISE(ABORT, 'child write failed'); END").Error
	}); err != nil {
		t.Fatal(err)
	}
	metadata := inspector.byImage[parent.Bundle]
	metadata.BundleDigest = "sha256:next"
	inspector.byImage[parent.Bundle] = metadata
	runtime := &stubSandboxRuntime{}
	svc.sandboxes = runtime
	if _, err := svc.RefreshHarnessConfigImage(ctx, parent.ProjectID, parent.ID); err == nil {
		t.Fatal("failed child write accepted")
	}
	for _, cfg := range []*model.HarnessConfig{parent, child} {
		got, err := svc.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.BundleDigest != cfg.BundleDigest || got.Revision != cfg.Revision {
			t.Fatal("partial publication")
		}
	}
	if len(runtime.upgraded) != 0 {
		t.Fatal("fan-out before commit")
	}
	if err := svc.store.Transaction(ctx, func(_ *store.Store, db *gorm.DB) error { return db.Exec("DROP TRIGGER fail_child").Error }); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshHarnessConfigImage(ctx, parent.ProjectID, parent.ID); err != nil {
		t.Fatal(err)
	}
	after, err := svc.store.ListSecretBindings(ctx, child.ProjectID, store.SecretBindingFilter{TargetKind: model.SecretBindingTargetHarnessConfig, TargetID: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("parent publication altered child bindings")
	}
}

func TestLegacyParentCannotSupplyPersonalFilesToVariant(t *testing.T) {
	ctx := context.Background()
	svc, _ := bundleService(t)
	parent := variantRoot(t, svc)
	parent.FileOverrides = nil
	parent.Files = append(parent.Files, model.HarnessConfigFile{Path: "personal-account", Content: "parent-only"})
	if err := svc.store.UpdateHarnessConfig(ctx, parent); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateHarnessConfig(ctx, parent.ProjectID, services.CreateHarnessConfigBody{Extends: apigen.NewOptString(parent.ID), Slug: apigen.NewOptString("work")})
	if err == nil || !strings.Contains(err.Error(), "refresh") {
		t.Fatalf("legacy parent accepted: %v", err)
	}
	parent, err = svc.RefreshHarnessConfigImage(ctx, parent.ProjectID, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	child := newVariant(t, svc, parent, "work")
	if len(child.FileOverrides) != 0 || slices.ContainsFunc(child.Files, func(f model.HarnessConfigFile) bool { return f.Path == "personal-account" }) {
		t.Fatal("parent personal file inherited")
	}
}
