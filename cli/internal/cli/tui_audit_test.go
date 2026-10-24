package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// auditBodyServer answers a discobox on pool-1 and, for its record http_42,
// the recorded body the test gives each part.
func auditBodyServer(t *testing.T, bodies map[string]string) *apiDataSource {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/project-1/sandboxes/sbx_1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sbx_1","projectId":"project-1","createdByUserId":"user-1",
				"displayName":"box","poolId":"pool-1","config":{"name":"box","image":""},
				"runtime":{"state":"ready","desiredState":"present","generation":1,"observedGeneration":1},
				"createdAt":"2026-09-17T09:00:00Z","updatedAt":"2026-09-17T09:00:01Z"}`))
		case strings.HasPrefix(r.URL.Path, "/api/projects/project-1/pools/pool-1/audit/http/http_42/"):
			body, ok := bodies[strings.TrimPrefix(r.URL.Path, "/api/projects/project-1/pools/pool-1/audit/http/http_42/")]
			if !ok || r.URL.Query().Get("sandboxId") != "sbx_1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	client, err := app.apiClient()
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	return &apiDataSource{app: app, client: client, projectID: "project-1"}
}

// A body is read from the pool the discobox runs on, laid out when it is JSON,
// and escaped: it is what a service sent, and it is going on a terminal.
func TestAuditBodyReadsTheRecordingForTheConsole(t *testing.T) {
	ds := auditBodyServer(t, map[string]string{
		"response-body": `{"id":"msg_01","text":"hi\u001b[2J"}`,
		"request-body":  "plain " + string(rune(0x1b)) + "[1A text",
	})
	got, err := ds.AuditBody(t.Context(), "sbx_1", "http_42", tui.AuditResponseBody)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if !strings.Contains(got, "\n  \"id\": \"msg_01\"") {
		t.Fatalf("a JSON body should be laid out:\n%s", got)
	}
	got, err = ds.AuditBody(t.Context(), "sbx_1", "http_42", tui.AuditRequestBody)
	if err != nil {
		t.Fatalf("request body: %v", err)
	}
	if strings.ContainsRune(got, 0x1b) || !strings.Contains(got, `plain \x1b[1A text`) {
		t.Fatalf("a body should be escaped for the terminal: %q", got)
	}
}

// A body past what a card shows is cut, and says so and how to read the rest.
func TestAuditBodyCutsALongRecording(t *testing.T) {
	long := strings.Repeat("x", auditBodyShown+10)
	ds := auditBodyServer(t, map[string]string{"response-body": long})
	got, err := ds.AuditBody(t.Context(), "sbx_1", "http_42", tui.AuditResponseBody)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	shown, rest, _ := strings.Cut(got, "\n\n")
	if shown != long[:auditBodyShown] || !strings.Contains(rest, "discobox admin audit http --discobox sbx_1 --body http_42 --part response") {
		t.Fatalf("shown %d bytes, then %q", len(shown), rest)
	}
}

// The console names a recording the way the command's --part does.
func TestAuditRecordingsAreTheCommandsParts(t *testing.T) {
	if tui.AuditRequestBody != httpAuditPartRequest || tui.AuditResponseBody != httpAuditPartResponse || tui.AuditStream != httpAuditPartStream {
		t.Fatal("the console's recordings and --part have drifted apart")
	}
}
