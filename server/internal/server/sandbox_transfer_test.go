package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// The import route is hand-wired, so nothing generated forces its response into
// the API's shape. Encoding the persistence model instead looks like it works —
// it is JSON with the right values in it — but `name` is top-level on
// model.Sandbox and under `config` on the API's, and a client decoding the
// generated type refuses the whole body (ADR 0118: whoever serves an API is
// strict). That is exactly what shipped, and neither side's unit tests caught
// it: the CLI's asserted against hand-written API-shaped JSON and the service's
// against *model.Sandbox, so the two never met.
func TestImportSandboxAnswersInTheAPIsShape(t *testing.T) {
	stubs := newRouterTestServices()
	stubs.importResult = &services.SandboxImportResult{
		Sandbox: &model.Sandbox{
			ID: "sbx_imported", ProjectID: testDefaultProjectID, Name: "restored",
			SandboxManifest: model.SandboxManifest{Image: "ghcr.io/example/stub:v1"},
		},
		Warnings: []string{"GITHUB_TOKEN was bound to a secret named \"github\", which this project does not have"},
	}
	server := newPoolLogsTestServer(t, stubs)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		server.URL+"/api/projects/"+testDefaultProjectID+"/sandboxes/import", bytes.NewReader([]byte("tar")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	// Decoded with the generated type the CLI uses, which is strict: an
	// unexpected field fails the decode rather than being ignored.
	var body struct {
		Sandbox  apimodel.Sandbox `json:"sandbox"`
		Warnings []string         `json:"warnings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("a client decoding the generated Sandbox could not read the response: %v", err)
	}
	if body.Sandbox.ID != "sbx_imported" {
		t.Errorf("sandbox id = %q", body.Sandbox.ID)
	}
	if body.Sandbox.Config.Name != "restored" {
		t.Errorf("config.name = %q; the name belongs under config in the API's shape", body.Sandbox.Config.Name)
	}
	// The warnings are the reason this route answers with an envelope rather
	// than a bare sandbox, so they have to survive the mapping.
	if len(body.Warnings) != 1 {
		t.Fatalf("warnings = %v, want the unmatched secret reported", body.Warnings)
	}
}
