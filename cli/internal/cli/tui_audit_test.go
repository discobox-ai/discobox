package cli

import (
	"encoding/json"
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
			if strings.HasSuffix(r.URL.Path, "/stream") && strings.HasPrefix(body, "DBW1") {
				w.Header().Set(httpAuditFormatHeader, "discobox-websocket-messages-v1")
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

// Indenting writes each value on its own line as deep as it is nested, so a
// body well under the cap, nested deep with many values, would lay out to many
// times the cap. What a card shows stays under the cap however the body is
// laid out.
func TestAuditBodyBoundsALaidOutDeepBody(t *testing.T) {
	const depth, values = 5000, 5000
	deep := strings.Repeat("[", depth) + strings.TrimSuffix(strings.Repeat("1,", values), ",") + strings.Repeat("]", depth)
	if len(deep) >= auditBodyShown || !json.Valid([]byte(deep)) {
		t.Fatalf("fixture: %d bytes, valid %v", len(deep), json.Valid([]byte(deep)))
	}
	ds := auditBodyServer(t, map[string]string{"response-body": deep})
	got, err := ds.AuditBody(t.Context(), "sbx_1", "http_42", tui.AuditResponseBody)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	if len(got) > auditBodyShown+1024 {
		t.Fatalf("a %d-byte body was shown as %d bytes, past the %d-byte cap", len(deep), len(got), auditBodyShown)
	}
}

// A body too deep to lay out under the cap is shown as recorded, and says so;
// a payload or event details in audit get is held to the same bound.
func TestAuditJSONTooDeepToLayOutIsShownAsRecorded(t *testing.T) {
	const depth, values = 5000, 5000
	deep := strings.Repeat("[", depth) + strings.TrimSuffix(strings.Repeat("1,", values), ",") + strings.Repeat("]", depth)
	ds := auditBodyServer(t, map[string]string{"response-body": deep})
	got, err := ds.AuditBody(t.Context(), "sbx_1", "http_42", tui.AuditResponseBody)
	if err != nil {
		t.Fatalf("response body: %v", err)
	}
	shown, note, _ := strings.Cut(got, "\n\n")
	if shown != deep || !strings.Contains(note, "shown as recorded") {
		t.Fatalf("shown %d bytes, then %q", len(shown), note)
	}
	if field := auditJSONField([]byte(deep)); !strings.HasPrefix(field, deep+"\n\n") || !strings.Contains(field, "shown as recorded") {
		t.Fatalf("audit get field: %d bytes, ends %q", len(field), field[max(0, len(field)-80):])
	}
	if text, note := indentAuditJSON([]byte(`{"a":[1]}`)); text != "{\n  \"a\": [\n    1\n  ]\n}" || note != "" {
		t.Fatalf("a small document = %q, note %q", text, note)
	}
}

// A body a service already sent indented is laid out as itself: its own
// whitespace is not counted on top of the layout's, so one under the cap is
// never shown as too big to lay out.
func TestAuditJSONAlreadyIndentedIsLaidOut(t *testing.T) {
	items := make([]map[string]any, 6000)
	for i := range items {
		items[i] = map[string]any{"id": i, "tags": []string{"a", "b"}, "nested": map[string]any{"ok": true}}
	}
	pretty, err := json.MarshalIndent(map[string]any{"items": items}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(pretty) < auditBodyShown/2 || len(pretty) >= auditBodyShown {
		t.Fatalf("fixture: %d bytes, want between half the cap and the cap", len(pretty))
	}
	if text, note := indentAuditJSON(pretty); text != string(pretty) || note != "" {
		t.Fatalf("an indented %d-byte document came back as %d bytes, note %q", len(pretty), len(text), note)
	}
}

// The console names a recording the way the command's --part does.
func TestAuditRecordingsAreTheCommandsParts(t *testing.T) {
	if tui.AuditRequestBody != httpAuditPartRequest || tui.AuditResponseBody != httpAuditPartResponse || tui.AuditStream != httpAuditPartStream {
		t.Fatal("the console's recordings and --part have drifted apart")
	}
}
