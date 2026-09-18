package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	idpkg "github.com/discobox-ai/x/id"
)

// An http record is read from the pool the discobox runs on, without the caller
// naming it, and shows the fields a listing leaves out.
func TestAuditGetReadsAnExchangeInFull(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	poolID, err := idpkg.New("pool")
	if err != nil {
		t.Fatal(err)
	}
	var gotPath, gotSandbox string
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sandboxes/"+sandboxID):
			// How the pool is resolved: the discobox names it.
			_, _ = w.Write([]byte(`{"id":"` + sandboxID + `","projectId":"project-1","createdByUserId":"user-1",
				"displayName":"box","poolId":"` + poolID + `","config":{"name":"box","image":""},
				"runtime":{"state":"ready","desiredState":"present","generation":1,"observedGeneration":1},
				"createdAt":"2026-09-17T09:00:00Z","updatedAt":"2026-09-17T09:00:01Z"}`))
		case strings.Contains(r.URL.Path, "/audit/http/"):
			gotPath, gotSandbox = r.URL.Path, r.URL.Query().Get("sandboxId")
			_, _ = w.Write([]byte(`{"poolId":"` + poolID + `","id":"http_10219","createdAt":"2026-09-17T10:00:00Z",
				"writtenAt":"2026-09-17T10:00:02Z","sandboxId":"` + sandboxID + `","method":"POST",
				"url":"https://api.github.com/x\u001b[1A","host":"api.github.com","status":201,"blocked":false,
				"swappedUseIds":["use_x"],"durationMillis":340,
				"requestHeaders":{"Authorization":["[REDACTED]"],"Accept":["application/json"]},
				"responseHeaders":{"Content-Type":["application/json"]},
				"appliedHeaders":["Authorization"],"appliedRuleId":"rule-1","appliedPattern":"api.github.com/*",
				"cacheKey":"key-1","cacheStored":true,
				"responseBodyRecorded":true,"responseBodyFormat":"raw","responseBytes":2048}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}

	stdout, stderr, err := runAudit(context.Background(), t, handler, "get", sandboxID, "http_10219")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotPath != "/projects/project-1/pools/"+poolID+"/audit/http/http_10219" || gotSandbox != sandboxID {
		t.Fatalf("read %s?sandboxId=%s", gotPath, gotSandbox)
	}
	for _, want := range []string{
		"record:", "http_10219", "201", "340ms", "use_x",
		"rule-1 (api.github.com/*)", "stored, key key-1",
		"2048 bytes, recorded as raw",
		"request headers:", "Authorization: [REDACTED]", "response headers:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("detail missing %q:\n%s", want, stdout)
		}
	}
	// The URL is what the discobox sent, so an escape in it is shown as text.
	if strings.ContainsRune(stdout, 0x1b) || !strings.Contains(stdout, `\x1b[1A`) {
		t.Fatalf("detail is not terminal-safe:\n%s", stdout)
	}
	// A body that exists is worth saying how to read, on stderr since it is
	// not part of the record.
	if !strings.Contains(stderr, "--body http_10219") {
		t.Fatalf("stderr = %q, want how to read the recorded body", stderr)
	}
}

// The ID says which trail to read. A verdict and the two trails the discobox
// keeps inside itself are all read by ID, and an ID in no trail says so.
func TestAuditGetRoutesByRecordID(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	asked := map[string]string{}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := r.URL.Query().Get("id")
		switch {
		case strings.HasSuffix(r.URL.Path, "/credential-verdicts"):
			asked["creds"] = id
			_, _ = w.Write([]byte(`{"credentialVerdicts":[{"id":"` + id + `","projectId":"project-1","sandboxId":"` + sandboxID +
				`","useId":"use_1","allow":false,"volunteered":true,"createdAt":"2026-09-17T10:00:00Z","command":["gh","pr create"],
				"reason":"denied\u001b[2K","role":"judge","prompt":"facts:\nline two"}]}`))
		case strings.HasSuffix(r.URL.Path, "/harness-hooks"):
			asked["hooks"] = id
			if id == "evt_hook" {
				_, _ = w.Write([]byte(`{"hooks":[{"id":"evt_hook","provider":"claude-code","event":"PreToolUse",
					"payload":{"tool":"Bash","command":"ls\u001b[2J"},"createdAt":"2026-09-17T10:00:00Z"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"hooks":[]}`))
		case strings.HasSuffix(r.URL.Path, "/exec-events"):
			asked["execs"] = id
			if id == "evt_event" {
				_, _ = w.Write([]byte(`{"events":[{"id":"evt_event","execId":"ex_1","type":"exec.start.failed",
					"message":"no such file","details":{"exitCode":127},"createdAt":"2026-09-17T10:00:00Z"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"events":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}

	verdict, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, "cvd_one")
	if err != nil {
		t.Fatalf("get verdict: %v", err)
	}
	if asked["creds"] != "cvd_one" || !strings.Contains(verdict, "deny") || !strings.Contains(verdict, "prompt:") ||
		!strings.Contains(verdict, `gh "pr create"`) {
		t.Fatalf("verdict = %q (asked %q)", verdict, asked["creds"])
	}
	if strings.ContainsRune(verdict, 0x1b) {
		t.Fatalf("verdict is not terminal-safe:\n%s", verdict)
	}

	hook, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, "evt_hook")
	if err != nil {
		t.Fatalf("get hook: %v", err)
	}
	if !strings.Contains(hook, "PreToolUse") || !strings.Contains(hook, `"tool": "Bash"`) || strings.ContainsRune(hook, 0x1b) {
		t.Fatalf("hook = %q", hook)
	}

	// An evt_ ID is in at most one of the two sandbox trails, so both are asked.
	event, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, "evt_event")
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if !strings.Contains(event, "exec.start.failed") || !strings.Contains(event, `"exitCode": 127`) {
		t.Fatalf("event = %q", event)
	}

	if _, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, "evt_missing"); err == nil {
		t.Fatal("an ID in no trail was accepted")
	}
	for _, bad := range []string{"10219", "nonsense", "sbx_1"} {
		if _, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, bad); err == nil {
			t.Fatalf("%q was accepted as a record ID", bad)
		}
	}
}

func TestAuditGetJSONIsTheRecord(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credentialVerdicts":[{"id":"cvd_one","projectId":"project-1","sandboxId":"` + sandboxID +
			`","useId":"use_1","allow":true,"volunteered":false,"createdAt":"2026-09-17T10:00:00Z","command":["gh"]}]}`))
	}
	stdout, _, err := runAudit(context.Background(), t, handler, "get", sandboxID, "cvd_one", "-o", "json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var got struct {
		ID      string   `json:"id"`
		Command []string `json:"command"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if got.ID != "cvd_one" || len(got.Command) != 1 {
		t.Fatalf("record = %+v", got)
	}
}
