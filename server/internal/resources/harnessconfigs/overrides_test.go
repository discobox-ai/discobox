package harnessconfigs

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

const overridesBundle = "discobox-harness-claude-code:v1"

// claudeBundle is a built-in bundle declaring a createOnly template and a
// settings file, as Claude Code's does, at digest.
func claudeBundle(digest, settings string) imageMetadata {
	return imageMetadata{BundleDigest: digest, ImageMetadata: harness.ImageMetadata{
		Harness: &harness.Image{ID: "claude-code", Name: "Claude Code", Files: []harness.File{
			{Path: ".claude.json", Content: "{}", CreateOnly: true},
			{Path: ".claude/settings.json", Content: settings},
		}},
	}}
}

// seededClaude seeds the claude-code built-in from claudeBundle and returns the
// service, its inspector and the seeded config.
func seededClaude(t *testing.T) (*Service, *stubInspector, *model.HarnessConfig) {
	t.Helper()
	ctx := context.Background()
	inspector := &stubInspector{byImage: map[string]imageMetadata{overridesBundle: claudeBundle("sha256:one", `{"v":1}`)}}
	svc := &Service{store: newTestStore(t), inspector: inspector, harnesses: bundleReferences(map[string]string{"claude-code": overridesBundle})}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	config, err := svc.store.GetHarnessConfigBySlug(ctx, "project-1", "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if config.FileOverrides == nil {
		t.Fatal("seeded config has an unwritten override set, want an empty one")
	}
	return svc, inspector, config
}

func setOverrides(t *testing.T, svc *Service, config *model.HarnessConfig, files ...apimodel.HarnessConfigFile) *model.HarnessConfig {
	t.Helper()
	body := services.UpdateHarnessConfigBody{Revision: config.Revision}
	body.SetFileOverrides(apigen.NewOptNilHarnessConfigFileArray(files))
	updated, err := svc.UpdateHarnessConfig(context.Background(), config.ProjectID, config.ID, body)
	if err != nil {
		t.Fatalf("update overrides: %v", err)
	}
	return updated
}

// unsplit leaves the config as the release before FileOverrides left it: no
// override set at all.
func unsplit(t *testing.T, svc *Service, configID string) {
	t.Helper()
	if err := svc.store.Transaction(context.Background(), func(_ *store.Store, db *gorm.DB) error {
		return db.Model(&model.HarnessConfig{}).Where("id = ?", configID).Update("file_overrides", nil).Error
	}); err != nil {
		t.Fatal(err)
	}
}

func wantFiles(t *testing.T, what string, got, want []model.HarnessConfigFile) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %+v, want %+v", what, got, want)
	}
}

// #524's Done when: a file a person adds to a built-in harness, then the
// server starting on a moved bundle, leaves the bundle's files (the template
// still createOnly) and the person's file all on the harness.
func TestAPersonsFileOnABuiltInSurvivesTheBundleMoving(t *testing.T) {
	ctx := context.Background()
	svc, inspector, config := seededClaude(t)
	config = setOverrides(t, svc, config, apimodel.HarnessConfigFile{Path: ".config/x.txt", Content: "mine"})
	wantFiles(t, "files after update", config.Files, []model.HarnessConfigFile{
		{Path: ".claude.json", Content: "{}", CreateOnly: true},
		{Path: ".claude/settings.json", Content: `{"v":1}`},
	})

	inspector.byImage[overridesBundle] = claudeBundle("sha256:two", `{"v":2}`)
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	got, err := svc.store.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BundleDigest != "sha256:two" {
		t.Fatalf("bundle digest = %q, want the reseed to have re-snapshotted", got.BundleDigest)
	}
	wantFiles(t, "files", got.Files, []model.HarnessConfigFile{
		{Path: ".claude.json", Content: "{}", CreateOnly: true},
		{Path: ".claude/settings.json", Content: `{"v":2}`},
	})
	wantFiles(t, "overrides", got.FileOverrides, []model.HarnessConfigFile{{Path: ".config/x.txt", Content: "mine"}})
}

// A person's override of a bundle path keeps winning after a refresh, which
// leaves the bundle's own file beneath it current.
func TestAnOverrideOfABundlePathSurvivesARefresh(t *testing.T) {
	ctx := context.Background()
	svc, inspector, config := seededClaude(t)
	setOverrides(t, svc, config, apimodel.HarnessConfigFile{Path: ".claude/settings.json", Content: `{"mine":true}`})
	inspector.byImage[overridesBundle] = claudeBundle("sha256:two", `{"v":2}`)
	got, err := svc.RefreshHarnessConfigImage(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	wantFiles(t, "files", got.Files, []model.HarnessConfigFile{
		{Path: ".claude.json", Content: "{}", CreateOnly: true},
		{Path: ".claude/settings.json", Content: `{"v":2}`},
	})
	wantFiles(t, "overrides", got.FileOverrides, []model.HarnessConfigFile{{Path: ".claude/settings.json", Content: `{"mine":true}`}})
}

// A plain reset takes back what boxes saved, not a person's files; asked to,
// it clears those too, to an empty set rather than nil, which would read as
// a config not yet split (ADR 26-11-04-028 §1).
func TestResetKeepsAPersonsFilesUnlessAsked(t *testing.T) {
	ctx := context.Background()
	svc, _, config := seededClaude(t)
	config = setOverrides(t, svc, config, apimodel.HarnessConfigFile{Path: ".config/x.txt", Content: "mine"})
	config.ConfiguredFiles = []model.HarnessConfigFile{{Path: ".claude/settings.json", Content: `{"saved":true}`}}
	if err := svc.store.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ResetHarnessConfig(ctx, "project-1", config.ID, false)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	wantFiles(t, "saved files", got.ConfiguredFiles, nil)
	wantFiles(t, "overrides", got.FileOverrides, []model.HarnessConfigFile{{Path: ".config/x.txt", Content: "mine"}})

	got, err = svc.ResetHarnessConfig(ctx, "project-1", config.ID, true)
	if err != nil {
		t.Fatalf("reset including overrides: %v", err)
	}
	if got.FileOverrides == nil || len(got.FileOverrides) != 0 {
		t.Fatalf("overrides = %#v, want an empty, split set", got.FileOverrides)
	}
}

// A config from before the set existed keeps a person's files in Files. The
// first re-snapshot moves every path the bundle doesn't declare out of it,
// once (ADR 26-11-04-028 §6).
func TestAConfigFromBeforeOverridesHasItsAddedFilesMovedOnce(t *testing.T) {
	ctx := context.Background()
	svc, inspector, config := seededClaude(t)
	// As the release before this one left it: the person's file in Files, and
	// no override set at all.
	unsplit(t, svc, config.ID)
	config, err := svc.store.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	config.Files = append(config.Files, model.HarnessConfigFile{Path: ".config/x.txt", Content: "mine"})
	if err := svc.store.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if config.FileOverrides != nil {
		t.Fatalf("overrides = %#v, want the config unsplit", config.FileOverrides)
	}

	inspector.byImage[overridesBundle] = claudeBundle("sha256:two", `{"v":2}`)
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	got, err := svc.store.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles(t, "files", got.Files, []model.HarnessConfigFile{
		{Path: ".claude.json", Content: "{}", CreateOnly: true},
		{Path: ".claude/settings.json", Content: `{"v":2}`},
	})
	wantFiles(t, "overrides", got.FileOverrides, []model.HarnessConfigFile{{Path: ".config/x.txt", Content: "mine"}})

	// Split once: a person removing the file is not undone by the next pass.
	got = setOverrides(t, svc, got)
	if got.FileOverrides == nil {
		t.Fatal("an emptied override set reads as unsplit")
	}
	inspector.byImage[overridesBundle] = claudeBundle("sha256:three", `{"v":3}`)
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	got, err = svc.store.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles(t, "overrides after removal", got.FileOverrides, nil)
}

// A built-in from before the set existed is split by the first seed even
// when its bundle hasn't moved, so a person can change its files right after
// the upgrade.
func TestTheFirstSeedSplitsABuiltInWhoseBundleHasNotMoved(t *testing.T) {
	ctx := context.Background()
	svc, _, config := seededClaude(t)
	unsplit(t, svc, config.ID)
	config, err := svc.store.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	config.Files = append(config.Files, model.HarnessConfigFile{Path: ".config/old.txt", Content: "old"})
	if err := svc.store.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("reseed: %v", err)
	}
	got, err := svc.store.GetHarnessConfig(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BundleDigest != "sha256:one" {
		t.Fatalf("bundle digest = %q, want it unchanged", got.BundleDigest)
	}
	wantFiles(t, "files", got.Files, []model.HarnessConfigFile{
		{Path: ".claude.json", Content: "{}", CreateOnly: true},
		{Path: ".claude/settings.json", Content: `{"v":1}`},
	})
	wantFiles(t, "overrides", got.FileOverrides, []model.HarnessConfigFile{{Path: ".config/old.txt", Content: "old"}})
	// Split: a person's write now goes through, --clear-file included.
	got = setOverrides(t, svc, got)
	wantFiles(t, "overrides after clear", got.FileOverrides, nil)
}

// A config not split yet refuses a person's set, which would end the split
// that moves their earlier files out, and names what splits it: a refresh,
// or for a config registered before bundles, its bundle.
func TestAnUnsplitConfigRefusesAPersonsFilesUntilSplit(t *testing.T) {
	ctx := context.Background()
	svc, _ := bundleService(t)
	registered, err := svc.CreateHarnessConfig(ctx, "project-1", services.CreateHarnessConfigBody{Bundle: apigen.NewOptString("example.com/acme:v1")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Registered before bundles: no bundle to refresh from.
	legacy := &model.HarnessConfig{ProjectID: "project-1", Slug: "old", Name: "Old", RunCommand: []string{"old"}}
	if err := svc.store.CreateHarnessConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		config *model.HarnessConfig
		says   string
	}{
		{registered, "harness refresh acme"},
		{legacy, "--bundle"},
	} {
		unsplit(t, svc, tc.config.ID)
		config, err := svc.store.GetHarnessConfig(ctx, "project-1", tc.config.ID)
		if err != nil {
			t.Fatal(err)
		}
		config.Files = append(config.Files, model.HarnessConfigFile{Path: ".config/old.txt", Content: "old"})
		if err := svc.store.UpdateHarnessConfig(ctx, config); err != nil {
			t.Fatal(err)
		}
		body := services.UpdateHarnessConfigBody{Revision: config.Revision}
		body.SetFileOverrides(apigen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{}))
		_, err = svc.UpdateHarnessConfig(ctx, "project-1", config.ID, body)
		var status apperrors.StatusError
		if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict || !strings.Contains(err.Error(), tc.says) {
			t.Fatalf("%s: err = %v, want a 409 naming %q", config.Slug, err, tc.says)
		}
		got, err := svc.store.GetHarnessConfig(ctx, "project-1", config.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.FileOverrides != nil || len(got.Files) == 0 || got.Files[len(got.Files)-1].Path != ".config/old.txt" {
			t.Fatalf("%s: config = %+v, want it untouched", config.Slug, got)
		}
	}

	// The refresh splits it, and the write goes through.
	refreshed, err := svc.RefreshHarnessConfigImage(ctx, "project-1", registered.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	wantFiles(t, "overrides after refresh", refreshed.FileOverrides, []model.HarnessConfigFile{{Path: ".config/old.txt", Content: "old"}})
	setOverrides(t, svc, refreshed)
}

func TestOverridesRefuseAPathGivenTwiceOrNone(t *testing.T) {
	svc, _, config := seededClaude(t)
	for _, files := range [][]apimodel.HarnessConfigFile{
		{{Path: "a", Content: "1"}, {Path: "a", Content: "2"}},
		{{Path: " ", Content: "1"}},
	} {
		body := services.UpdateHarnessConfigBody{Revision: config.Revision}
		body.SetFileOverrides(apigen.NewOptNilHarnessConfigFileArray(files))
		_, err := svc.UpdateHarnessConfig(context.Background(), "project-1", config.ID, body)
		var status apperrors.StatusError
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
			t.Fatalf("err = %v, want %+v refused as a bad request", err, files)
		}
	}
}

// create --file gives the new config its first overrides, beside the bundle's
// own files.
func TestCreateTakesAPersonsFilesAsItsFirstOverrides(t *testing.T) {
	svc, _ := bundleService(t)
	body := services.CreateHarnessConfigBody{Bundle: apigen.NewOptString("example.com/acme:v1")}
	body.SetFileOverrides(apigen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{{Path: ".acme", Content: "mine", CreateOnly: apigen.NewOptBool(true)}}))
	config, err := svc.CreateHarnessConfig(context.Background(), "project-1", body)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wantFiles(t, "overrides", config.FileOverrides, []model.HarnessConfigFile{{Path: ".acme", Content: "mine", CreateOnly: true}})
}

// A save never writes a person's file (ADR 26-11-04-028 §5): a path a person
// overrode is saved to the saved set beneath the override, where a plain reset
// can take it back, and the override is left as the person wrote it.
func TestCaptureSavesAnOverriddenPathBeneathTheOverride(t *testing.T) {
	s, cfg, c := captureFixture(t)
	ctx := context.Background()
	cfg.ConfiguredFiles = []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"model":"configured"}`}}
	cfg.FileOverrides = []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"model":"mine"}`}}
	if err := s.store.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	c.WriteRevision = cfg.Revision
	baseline, err := s.store.CaptureBaseline(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(baseline.Files); n != 1 || baseline.Files[0].Content != `{"model":"mine"}` {
		t.Fatalf("baseline files = %+v, want the override on top", baseline.Files)
	}
	c.Baseline, c.SecretRevisions = baseline, baseline.SecretRevisions
	if _, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false); err != nil {
		t.Fatal(err)
	}
	stored, err := s.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles(t, "overrides", stored.FileOverrides, []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"model":"mine"}`}})
	wantFiles(t, "saved files", stored.ConfiguredFiles, []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"model":"local"}`}})

	reset, err := s.ResetHarnessConfig(ctx, cfg.ProjectID, cfg.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles(t, "saved files after reset", reset.ConfiguredFiles, nil)
	wantFiles(t, "overrides after reset", reset.FileOverrides, []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"model":"mine"}`}})
}

// A removal in a preview falls back to what a box is given once the saved
// file is gone: a person's override, which a save never touches, else the
// bundle's file.
func TestFileBeneathASavedFileIsTheOverrideElseTheBundles(t *testing.T) {
	cfg := &model.HarnessConfig{
		Files:           []model.HarnessConfigFile{{Path: "a", Content: "bundle"}, {Path: "b", Content: "bundle-b"}},
		ConfiguredFiles: []model.HarnessConfigFile{{Path: "a", Content: "configured"}, {Path: "b", Content: "saved-b"}},
		FileOverrides:   []model.HarnessConfigFile{{Path: "a", Content: "mine"}},
	}
	for path, want := range map[string]string{"a": "mine", "b": "bundle-b", "c": ""} {
		got, ok := fileBeneath(cfg, path)
		if want == "" {
			if ok {
				t.Errorf("beneath %s = %+v, want nothing", path, got)
			}
			continue
		}
		if !ok || got.Content != want {
			t.Errorf("beneath %s = %+v, want %q", path, got, want)
		}
	}
}
