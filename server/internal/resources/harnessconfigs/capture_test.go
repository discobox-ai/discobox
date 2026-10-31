package harnessconfigs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

func captureFixture(t *testing.T) (*Service, *model.HarnessConfig, harnesscapture.Candidate) {
	t.Helper()
	return captureFixtureStore(t, newTestStore(t))
}
func captureFixtureStore(t *testing.T, st *store.Store) (*Service, *model.HarnessConfig, harnesscapture.Candidate) {
	t.Helper()
	ctx := context.Background()
	cfg := &model.HarnessConfig{ID: "hc-capture", ProjectID: "project-1", Name: "Capture", ManifestDigest: "sha256:fixture", SaveBack: &harnesscapture.Definition{Version: 1, Watch: []string{"~/.fixture"}, ExportCommand: []string{"/bin/export"}, HandoffCommand: []string{"/bin/handoff"}, Providers: []harnesscapture.Provider{{ID: "fixture", EnvName: "API_TOKEN", Types: []string{"token"}, Host: "api.example.com", Routes: []harnesscapture.Route{{Hosts: []string{"api.example.com"}}}}}, Settings: []harnesscapture.Setting{{Path: "~/.fixture", Format: "json", Exclude: []string{"apiKey", "api_key", "env"}}}}}
	if err := st.CreateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: "prov-capture", ProjectID: "project-1", Type: "docker", Name: "capture"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePool(ctx, &model.Pool{ID: "pool-capture", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "capture", ProviderInstanceID: "prov-capture"}}); err != nil {
		t.Fatal(err)
	}
	box := &model.Sandbox{PoolID: "pool-capture", ID: "sbx-capture", ProjectID: "project-1", Name: "Capture", SandboxManifest: model.SandboxManifest{HarnessConfigID: &cfg.ID}}
	if err := st.CreateSandbox(ctx, box); err != nil {
		t.Fatal(err)
	}
	baseline, err := st.CaptureBaseline(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := harnesscapture.Candidate{ID: "candidate-1", Baseline: baseline, WriteRevision: cfg.Revision, SecretRevisions: baseline.SecretRevisions, Files: []harnesscapture.FileChange{{Path: "~/.fixture", File: &harnesscapture.File{Path: "~/.fixture", Content: `{"model":"local"}`}}}, Credentials: []harnesscapture.Credential{{Provider: "fixture", EnvName: "API_TOKEN", Type: "token", LocalID: "fixture", Generation: "generation", Value: json.RawMessage(`{"token":"new-secret"}`)}}}
	return &Service{store: st, dirtier: stubDirtier{}}, cfg, c
}

func TestCaptureCommitsPatchSecretsAndReceiptIdempotently(t *testing.T) {
	s, cfg, c := captureFixture(t)
	ctx := context.Background()
	receipt, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Baseline.ConfigRevision != cfg.Revision+1 {
		t.Fatal("revision did not advance")
	}
	stored, err := s.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.ConfiguredFiles) != 1 || len(stored.ConfiguredSecretIDs) != 1 {
		t.Fatalf("missing committed assets: %+v", stored)
	}
	secret, err := s.store.GetSecret(ctx, cfg.ProjectID, stored.ConfiguredSecretIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(secret.Routes) != 1 {
		t.Fatal("provider authority missing")
	}
	encoded, marshalErr := json.Marshal(receipt)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(encoded, []byte("new-secret")) {
		t.Fatal("receipt contains credential")
	}
	replay, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Baseline.ConfigRevision != receipt.Baseline.ConfigRevision {
		t.Fatal("replay mutated config")
	}
}

func TestCaptureConflictsAfterSettingsEditWithoutWritingSecret(t *testing.T) {
	s, cfg, c := captureFixture(t)
	ctx := context.Background()
	cfg.Name = "Edited"
	if err := s.store.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false); err == nil {
		t.Fatal("stale candidate accepted")
	}
	bindings, err := s.store.ListTargetUseBindings(ctx, cfg.ProjectID, model.SecretBindingTargetHarnessConfig, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 0 {
		t.Fatal("conflicting candidate wrote secret")
	}
	if _, err := s.store.GetHarnessCaptureReceipt(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("conflicting candidate wrote receipt")
	}
}
func TestCaptureConflictsAfterSecretRefreshAndPreservesUserBinding(t *testing.T) {
	s, cfg, c := captureFixture(t)
	ctx := context.Background()
	sec := &model.Secret{ProjectID: cfg.ProjectID, Name: "User token", Type: "token", EncryptedValue: []byte(`{"token":"original"}`)}
	if err := s.store.CreateSecret(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if err := putHarnessBinding(ctx, s.store, harnessBinding{ProjectID: cfg.ProjectID, HarnessConfigID: cfg.ID, EnvName: "API_TOKEN", SecretID: sec.ID}); err != nil {
		t.Fatal(err)
	}
	baseline, err := s.store.CaptureBaseline(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Baseline = baseline
	c.SecretRevisions = baseline.SecretRevisions
	sec.EncryptedValue = []byte(`{"token":"refreshed"}`)
	if err := s.store.UpdateSecret(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if _, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false); err == nil {
		t.Fatal("rotated credential overwritten")
	}
	if _, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, true); err != nil {
		t.Fatal(err)
	}
	bindings, err := s.store.ListTargetUseBindings(ctx, cfg.ProjectID, model.SecretBindingTargetHarnessConfig, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bindings[0].SecretID == sec.ID {
		t.Fatal("hand-bound secret overwritten")
	}
	existing, err := s.store.GetSecret(ctx, cfg.ProjectID, sec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(existing.EncryptedValue) != `{"token":"refreshed"}` {
		t.Fatal("hand-bound value changed")
	}
}
func TestHarnessConfigUpdateRejectsStaleSnapshot(t *testing.T) {
	s, cfg, _ := captureFixture(t)
	stale := *cfg
	if err := s.store.UpdateHarnessConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateHarnessConfig(context.Background(), &stale); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("want CAS conflict: %v", err)
	}
}

func TestCaptureRollsBackAssetsWhenDeliveryPreparationFails(t *testing.T) {
	s, cfg, c := captureFixture(t)
	s.sandboxes = &failingCaptureRuntime{}
	if _, err := s.commitCapture(context.Background(), cfg.ProjectID, "sbx-capture", cfg.ID, c, false); err == nil {
		t.Fatal("wanted delivery failure")
	}
	stored, err := s.store.GetHarnessConfig(context.Background(), cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != cfg.Revision || len(stored.ConfiguredFiles) != 0 || len(stored.ConfiguredSecretIDs) != 0 {
		t.Fatal("partial config commit")
	}
	bindings, err := s.store.ListTargetUseBindings(context.Background(), cfg.ProjectID, model.SecretBindingTargetHarnessConfig, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 0 {
		t.Fatal("partial credential binding")
	}
}

type failingCaptureRuntime struct{ stubSandboxRuntime }

func (*failingCaptureRuntime) MarkBindingHoldersDirtyTx(context.Context, *store.Store, *gorm.DB, string, []model.SecretBinding) error {
	return errors.New("injected transaction failure")
}

func TestCaptureRejectsCredentialHiddenInTemplate(t *testing.T) {
	s, cfg, c := captureFixture(t)
	cfg.SaveBack.CredentialFiles = []string{"~/.auth.json"}
	cfg.SaveBack.CredentialSettings = []harnesscapture.Setting{{Path: "~/.auth.json", Format: "json", Fields: map[string]string{"token": "secret"}}}
	if err := s.store.UpdateHarnessConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	c.WriteRevision = cfg.Revision
	c.Files = []harnesscapture.FileChange{{Path: "~/.auth.json", File: &harnesscapture.File{Path: "~/.auth.json", Content: `{"token":"raw-stolen-secret"}`}}}
	if _, err := s.commitCapture(context.Background(), cfg.ProjectID, "sbx-capture", cfg.ID, c, false); err == nil {
		t.Fatal("raw secret template accepted")
	}
}

func TestCapturePreviewRejectsUnclassifiedFileAndRedactsDiagnostics(t *testing.T) {
	s, cfg, _ := captureFixture(t)
	p := harnesscapture.Preview{HarnessConfigID: cfg.ID, Files: []harnesscapture.FileChange{{Path: "~/.fixture", File: &harnesscapture.File{Path: "~/.fixture", Content: `{"api_key":"raw-secret"}`}}}}
	if err := s.validateCapturePreview(context.Background(), cfg.ProjectID, "sbx-capture", &p); err == nil {
		t.Fatal("credential-bearing preview accepted")
	}
	p.Files = nil
	p.Diagnostics = []harnesscapture.Diagnostic{{Code: "raw-secret", Path: "raw-secret", Message: "raw-secret"}}
	if err := s.validateCapturePreview(context.Background(), cfg.ProjectID, "sbx-capture", &p); err != nil {
		t.Fatal(err)
	}
	data, marshalErr := json.Marshal(p)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if bytes.Contains(data, []byte("raw-secret")) {
		t.Fatal("diagnostic leaked secret")
	}
	p.Diagnostics = []harnesscapture.Diagnostic{{Code: "partial", Path: cfg.SaveBack.Watch[0], Message: "raw-secret"}}
	if err := s.validateCapturePreview(context.Background(), cfg.ProjectID, "sbx-capture", &p); err != nil {
		t.Fatal(err)
	}
	if p.Diagnostics[0].Path != cfg.SaveBack.Watch[0] || p.Diagnostics[0].Code != "capture-incomplete" || strings.Contains(p.Diagnostics[0].Message, "raw-secret") {
		t.Fatalf("diagnostic should identify the registered file without its error text: %+v", p.Diagnostics)
	}
}

func TestHarnessEditorRejectsOriginalRevisionAfterCapture(t *testing.T) {
	s, cfg, c := captureFixture(t)
	ctx := context.Background()
	if _, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false); err != nil {
		t.Fatal(err)
	}
	_, err := s.UpdateHarnessConfig(ctx, cfg.ProjectID, cfg.ID, services.UpdateHarnessConfigBody{Revision: cfg.Revision, Name: serverapi.NewOptString("stale editor")})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 409 {
		t.Fatalf("stale editor update not rejected: %v", err)
	}
	current, err := s.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Name != cfg.Name || len(current.ConfiguredFiles) != 1 {
		t.Fatal("stale editor clobbered capture")
	}
}

func TestCaptureTemplateRequiresBoundOrNewCredential(t *testing.T) {
	for _, supply := range []bool{false, true} {
		t.Run(fmt.Sprint(supply), func(t *testing.T) {
			s, cfg, c := captureFixture(t)
			ctx := context.Background()
			cfg.SaveBack.CredentialFiles = []string{"~/.auth.json"}
			cfg.SaveBack.CredentialSettings = []harnesscapture.Setting{{Path: "~/.auth.json", Format: "json", Fields: map[string]string{"token": "secret"}}}
			if err := s.store.UpdateHarnessConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			c.WriteRevision = cfg.Revision
			c.Files = []harnesscapture.FileChange{{Path: "~/.auth.json", File: &harnesscapture.File{Path: "~/.auth.json", Content: `{"token":{"$secret":"API_TOKEN"}}`}}}
			if !supply {
				c.Credentials = nil
			}
			_, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false)
			if supply && err != nil {
				t.Fatal(err)
			}
			if !supply && err == nil {
				t.Fatal("unbound template persisted")
			}
			current, loadErr := s.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if !supply && len(current.ConfiguredFiles) != 0 {
				t.Fatal("rejected template left a file")
			}
		})
	}
}

// A preview's before is what the server stores, not anything the sandbox says:
// the configured file over the bundle's, and a removal's after is the
// bundle's own file it falls back to.
func TestCapturePreviewIsDescribedFromStoredConfig(t *testing.T) {
	s, cfg, _ := captureFixture(t)
	ctx := context.Background()
	cfg.Files = []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"theme":"bundle"}`}, {Path: "~/.bundled", Content: `{"a":1}`}}
	cfg.ConfiguredFiles = []model.HarnessConfigFile{{Path: "~/.fixture", Content: `{"theme":"dark"}`}}
	if err := s.store.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	forged := &harnesscapture.File{Path: "~/.fixture", Content: `{"theme":"forged"}`}
	p := harnesscapture.Preview{Files: []harnesscapture.FileChange{
		{Path: "~/.fixture", File: &harnesscapture.File{Path: "~/.fixture", Content: `{"theme":"light"}`}, Previous: forged},
		{Path: "~/.new", File: &harnesscapture.File{Path: "~/.new", Content: `{}`}},
		{Path: "~/.bundled", Remove: true},
		{Path: "~/.gone", Remove: true},
	}}
	if err := s.describeCapturePreview(ctx, cfg.ProjectID, "sbx-capture", &p); err != nil {
		t.Fatal(err)
	}
	content := func(f *harnesscapture.File) string {
		if f == nil {
			return "<none>"
		}
		return f.Content
	}
	for _, tc := range []struct {
		change           harnesscapture.FileChange
		previous, result string
	}{
		{p.Files[0], `{"theme":"dark"}`, `{"theme":"light"}`},
		{p.Files[1], "<none>", `{}`},
		{p.Files[2], `{"a":1}`, `{"a":1}`},
		{p.Files[3], "<none>", "<none>"},
	} {
		if got := content(tc.change.Previous); got != tc.previous {
			t.Errorf("%s previous = %s, want %s", tc.change.Path, got, tc.previous)
		}
		if got := content(tc.change.File); got != tc.result {
			t.Errorf("%s file = %s, want %s", tc.change.Path, got, tc.result)
		}
	}
}

func TestForceCaptureOverwritesStaleRevision(t *testing.T) {
	s, cfg, c := captureFixture(t)
	ctx := context.Background()
	cfg.Name = "Concurrent edit"
	cfg.ConfiguredFiles = append(cfg.ConfiguredFiles, model.HarnessConfigFile{Path: "unrelated", Content: "keep"})
	if err := s.store.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, false); err == nil {
		t.Fatal("expected conflict")
	}
	receipt, err := s.commitCapture(ctx, cfg.ProjectID, "sbx-capture", cfg.ID, c, true)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Baseline.ConfigRevision <= c.WriteRevision {
		t.Fatal("revision did not advance")
	}
	saved, err := s.store.GetHarnessConfig(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Name != "Concurrent edit" || len(saved.ConfiguredFiles) != 2 {
		t.Fatalf("lost unrelated changes: %+v", saved)
	}
}

func TestForceCaptureStillRejectsChangedManifest(t *testing.T) {
	s, cfg, c := captureFixture(t)
	c.Baseline.ManifestDigest = "different"
	if _, err := s.commitCapture(context.Background(), cfg.ProjectID, "sbx-capture", cfg.ID, c, true); err == nil {
		t.Fatal("force bypassed manifest check")
	}
}
