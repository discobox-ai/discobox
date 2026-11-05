package harnessconfigs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func TestCapturePreviewOmitsSettingsAlreadySaved(t *testing.T) {
	for _, scenario := range []string{"bundle upgraded", "another box saved"} {
		t.Run(scenario, func(t *testing.T) {
			svc, cfg, candidate := captureFixture(t)
			ctx := context.Background()
			candidate.Credentials = nil
			if scenario == "bundle upgraded" {
				// The box still has its old delivery baseline, but its local file
				// now matches the current bundle default after an upgrade.
				cfg.Files = []model.HarnessConfigFile{{Path: "~/.fixture", Content: candidate.Files[0].File.Content}}
				if err := svc.store.UpdateHarnessConfig(ctx, cfg); err != nil {
					t.Fatal(err)
				}
			} else {
				other := &model.Sandbox{ID: "sbx-other", PoolID: "pool-capture", ProjectID: cfg.ProjectID, Name: "Other", SandboxManifest: model.SandboxManifest{HarnessConfigID: &cfg.ID}}
				if err := svc.store.CreateSandbox(ctx, other); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.commitCapture(ctx, cfg.ProjectID, other.ID, cfg.ID, candidate); err != nil {
					t.Fatal(err)
				}
			}
			// The first box's immutable candidate remains pending against its
			// starting baseline. Read it through the service used by the console.
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(harnesscapture.Preview{State: "pending", CandidateID: candidate.ID, HarnessConfigID: cfg.ID, ConfigRevision: candidate.WriteRevision, Files: candidate.Files}); err != nil {
					t.Error(err)
				}
			}))
			defer endpoint.Close()
			runtime := &capturePreviewRuntime{captureDeliveryRuntime: &captureDeliveryRuntime{store: svc.store, endpoint: endpoint}}
			svc.sandboxes = runtime
			userCtx := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeUser, UserID: "user-1"})
			preview, err := svc.GetSandboxHarnessConfigChanges(userCtx, cfg.ProjectID, "sbx-capture")
			if err != nil {
				t.Fatal(err)
			}
			if runtime.use != services.SandboxObserve {
				t.Fatalf("preview use=%+v, want observe-only", runtime.use)
			}
			if len(preview.Files) != 0 {
				t.Fatalf("preview has %d settings changes already present in current saved state; want 0 (before=%q, after=%q)", len(preview.Files), preview.Files[0].Previous.Content, preview.Files[0].File.Content)
			}
		})
	}
}

func TestCapturePreviewComparesCurrentSettingsAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		bundle, saved, override, local string
		remove, createOnly, want       bool
	}{
		{name: "settings layout", bundle: `{"model":"local"}`, local: "{\n  \"model\": \"local\"\n}\n"},
		{name: "large integers remain distinct", bundle: `{"limit":9007199254740992}`, local: `{"limit":9007199254740993}`, want: true},
		{name: "real settings", bundle: `{"model":"remote"}`, local: `{"model":"local"}`, want: true},
		{name: "absent removal", remove: true},
		{name: "removal reveals same bundle", bundle: `{"model":"local"}`, saved: `{"model":"local"}`, remove: true},
		{name: "removal reveals changed bundle", bundle: `{"model":"remote"}`, saved: `{"model":"local"}`, remove: true, want: true},
		{name: "create only changes behavior", bundle: `{"model":"local"}`, local: `{"model":"local"}`, createOnly: true, want: true},
		{name: "override must not hide saved write", bundle: `{"model":"bundle"}`, override: `{"model":"local"}`, local: `{"model":"local"}`, want: true},
		{name: "already saved beneath override", saved: `{"model":"local"}`, override: `{"model":"mine"}`, local: `{"model":"local"}`},
		{name: "removal beneath override", saved: `{"model":"local"}`, override: `{"model":"mine"}`, remove: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, cfg, _ := captureFixture(t)
			ctx := context.Background()
			file := func(content string) []model.HarnessConfigFile {
				if content == "" {
					return nil
				}
				return []model.HarnessConfigFile{{Path: "~/.fixture", Content: content}}
			}
			cfg.Files, cfg.ConfiguredFiles, cfg.FileOverrides = file(tc.bundle), file(tc.saved), file(tc.override)
			if err := svc.store.UpdateHarnessConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			change := harnesscapture.FileChange{Path: "~/.fixture", Remove: tc.remove}
			if !tc.remove {
				change.File = &harnesscapture.File{Path: change.Path, Content: tc.local, CreateOnly: tc.createOnly}
			}
			preview := harnesscapture.Preview{CandidateID: "candidate", State: "pending", Files: []harnesscapture.FileChange{change}, Credentials: []harnesscapture.CredentialPreview{{Provider: "fixture", EnvName: "API_TOKEN", Type: "token"}}}
			if err := svc.describeCapturePreview(ctx, cfg.ProjectID, "sbx-capture", &preview); err != nil {
				t.Fatal(err)
			}
			if (len(preview.Files) > 0) != tc.want {
				t.Fatalf("files=%+v, want change=%v", preview.Files, tc.want)
			}
			if len(preview.Credentials) != 1 || preview.CandidateID != "candidate" {
				t.Fatal("credential or candidate lost")
			}
		})
	}
}

// Record the runtime use at the service boundary: an automatic preview must
// never wake a box even if its last listing still says it is running.
type capturePreviewRuntime struct {
	*captureDeliveryRuntime
	use services.SandboxUse
}

func (r *capturePreviewRuntime) AcquireSandboxHTTPClient(ctx context.Context, p, b string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	r.use = use
	return r.captureDeliveryRuntime.AcquireSandboxHTTPClient(ctx, p, b, scopes, use)
}
