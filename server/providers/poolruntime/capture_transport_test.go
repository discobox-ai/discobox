package poolruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// Exercise Create, including ogen's request encoder, rather than only the
// intermediate mapper: capture has to survive the real control-plane/pool hop.
func TestCreateTransmitsHarnessCaptureDefinitionAndBaseline(t *testing.T) {
	definition := harnesscapture.Definition{Version: 1, Watch: []string{"~/.settings.json"}, ExportCommand: []string{"/bin/export"}, HandoffCommand: []string{"/bin/handoff"}, Settings: []harnesscapture.Setting{{Path: "~/.settings.json", Format: "json", Exclude: []string{"apiKey", "env"}}}, Providers: []harnesscapture.Provider{{ID: "vendor", EnvName: "API_TOKEN", Types: []string{"token"}, Host: "api.example.com"}}}
	baseline := harnesscapture.Baseline{HarnessConfigID: "harness-1", ConfigRevision: 7, ManifestDigest: "sha256:pinned", SecretRevisions: map[string]string{"API_TOKEN": "secret-id:revision"}, Files: []harnesscapture.File{{Path: "~/.settings.json", Content: `{"model":"configured"}`, CreateOnly: true}}}
	var got struct {
		ResolvedHarnessConfig struct {
			ID              string                     `json:"id"`
			SaveBack        *harnesscapture.Definition `json:"saveBack"`
			CaptureBaseline *harnesscapture.Baseline   `json:"captureBaseline"`
		} `json:"resolvedHarnessConfig"`
	}
	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"title":"test ends at pool boundary","status":503}`))
	}))
	defer server.Close()
	lease := transport.NewHTTPClientLeaseWithBaseURLAndAuthProvider(server.Client(), server.URL, func(context.Context) (string, error) { return "test-token", nil }, nil)
	client := &poolAgentClient{poolID: "pool-1", lease: lease}
	_, _, _ = client.Create(context.Background(), sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}, nil, sandbox.CreateOptions{ResolvedHarnessConfig: &sandbox.ResolvedHarnessConfig{ID: "harness-1", Name: "Harness", SaveBack: &definition, CaptureBaseline: &baseline}})
	if !reached {
		t.Fatal("create never reached pool")
	}
	if !reflect.DeepEqual(got.ResolvedHarnessConfig.SaveBack, &definition) || !reflect.DeepEqual(got.ResolvedHarnessConfig.CaptureBaseline, &baseline) {
		t.Fatalf("capture contract lost at HTTP boundary: %#v", got.ResolvedHarnessConfig)
	}
}
