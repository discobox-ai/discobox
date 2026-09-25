package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	idpkg "github.com/discobox-ai/x/id"
)

// runAuditCreds runs `admin audit creds` against a server that answers with
// body and records the query the command sent.
func runAuditCreds(t *testing.T, body string, args ...string) (url.Values, string, error) {
	t.Helper()
	var query url.Values
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/credential-verdicts" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	out := new(strings.Builder)
	cmd := NewRootCommand()
	cmd.SetOut(out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "admin", "audit", "creds"}, args...))
	err := cmd.Execute()
	return query, out.String(), err
}

func TestAuditCredsSendsItsFilters(t *testing.T) {
	// A real generated ID, not a lookalike: only the exact generated shape
	// skips the short-ID lookup against the live discobox list.
	fullID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	before := time.Now()
	query, _, err := runAuditCreds(t, `{"credentialVerdicts":[]}`,
		"--discobox-id", fullID,
		"--use-id", "use_1", "--grant-id", "grant_1", "--denied", "--since", "1h", "--limit", "5")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// A full ID is sent as-is, with no lookup: the discobox may be long gone,
	// and its trail is still there.
	if got := query.Get("sandboxId"); got != fullID {
		t.Fatalf("sandboxId = %q", got)
	}
	if query.Get("useId") != "use_1" || query.Get("grantId") != "grant_1" || query.Get("limit") != "5" {
		t.Fatalf("filters = %v", query)
	}
	// --denied is allow=false, not an absent allow: the server reads absent as
	// every verdict.
	if got := query.Get("allow"); got != "false" {
		t.Fatalf("allow = %q, want false", got)
	}
	since, err := time.Parse(time.RFC3339, query.Get("since"))
	if err != nil {
		t.Fatalf("since %q: %v", query.Get("since"), err)
	}
	if want := before.Add(-time.Hour); since.Before(want.Add(-time.Minute)) || since.After(time.Now().Add(-time.Hour).Add(time.Second)) {
		t.Fatalf("since = %s, want about an hour ago (%s)", since, want)
	}
}

func TestAuditCredsSendsNoAllowUnlessAsked(t *testing.T) {
	query, _, err := runAuditCreds(t, `{"credentialVerdicts":[]}`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if query.Has("allow") {
		t.Fatalf("allow = %q sent without --denied or --allowed", query.Get("allow"))
	}
}

func TestAuditCredsRefusesDeniedAndAllowedTogether(t *testing.T) {
	if _, _, err := runAuditCreds(t, `{"credentialVerdicts":[]}`, "--denied", "--allowed"); err == nil {
		t.Fatal("expected --denied with --allowed to be refused")
	}
}

// The reason, the argv and the prompt were written inside the discobox. A
// terminal escape in any of them must reach the screen as text, not as an
// instruction to the terminal — the case here moves the cursor up a row and
// overwrites it, which is how a denial is dressed up as an allow.
const hostileVerdicts = `{"credentialVerdicts":[{
	"id":"cv_1","projectId":"project-1","sandboxId":"sbx_gone","useId":"use_1\u001b[1A\u009b2K",
	"allow":false,"volunteered":true,"createdAt":"2026-09-02T10:00:00Z",
	"command":["gh","pr create","\u202eetaerc"],
	"reason":"denied\u001b[1A\u001b[2Kallow",
	"role":"judge","prompt":"facts:\n\u001b]52;c;cGF3bmVk\u0007line two"
}]}`

func TestAuditCredsEscapesWhatTheDiscoboxWrote(t *testing.T) {
	for _, args := range [][]string{nil, {"--prompt"}} {
		_, out, err := runAuditCreds(t, hostileVerdicts, args...)
		if err != nil {
			t.Fatalf("execute %v: %v", args, err)
		}
		// \u009b is the one-rune C1 form of ESC [, and it sits in the use ID:
		// a reported denial's use ID is the discobox's own text.
		for _, raw := range []string{"\x1b", "\a", "\u202e", "\u009b"} {
			if strings.Contains(out, raw) {
				t.Fatalf("%v: output carries raw %q:\n%s", args, raw, out)
			}
		}
		if !strings.Contains(out, `\x1b[1A`) {
			t.Fatalf("%v: escape sequence not shown as text:\n%s", args, out)
		}
		if !strings.Contains(out, `\u202e`) {
			t.Fatalf("%v: bidi override not shown as text:\n%s", args, out)
		}
		// Provenance survives rendering: this row is a report, not a use.
		if !strings.Contains(out, "deny") || !strings.Contains(out, "report") {
			t.Fatalf("%v: verdict or provenance missing:\n%s", args, out)
		}
	}
}

// JSON carries the recorded values exactly, but a C1 control or a bidi
// override inside a string is written as a \u escape rather than raw — Go's
// encoder escapes only C0 — so printing it to a terminal is as safe as the
// table. The decoded document must still be the one the server sent.
func TestAuditCredsJSONIsTerminalSafeAndExact(t *testing.T) {
	_, out, err := runAuditCreds(t, hostileVerdicts, "-o", "json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, raw := range []string{"\x1b", "\a", "\u202e", "\u009b"} {
		if strings.Contains(out, raw) {
			t.Fatalf("json output carries raw %q:\n%s", raw, out)
		}
	}
	var got, want struct {
		CredentialVerdicts []struct {
			UseID   string   `json:"useId"`
			Reason  string   `json:"reason"`
			Prompt  string   `json:"prompt"`
			Command []string `json:"command"`
		} `json:"credentialVerdicts"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(hostileVerdicts), &want); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(got.CredentialVerdicts) != 1 || !reflect.DeepEqual(got.CredentialVerdicts, want.CredentialVerdicts) {
		t.Fatalf("decoded json = %+v, want the recorded values %+v", got.CredentialVerdicts, want.CredentialVerdicts)
	}
}

// With --prompt the prompt keeps its line breaks — it is a block, not a cell —
// while everything else in it is still escaped.
func TestAuditCredsPromptKeepsItsLines(t *testing.T) {
	_, out, err := runAuditCreds(t, hostileVerdicts, "--prompt")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "\n  facts:\n") || !strings.Contains(out, `line two`) {
		t.Fatalf("prompt lost its layout:\n%s", out)
	}
	// An argv element holding a space stays one element.
	if !strings.Contains(out, `gh "pr create"`) {
		t.Fatalf("argv elements merged:\n%s", out)
	}
}

// requestVerdicts are the project's judge's answers about two requests: one
// allowed, one where it asked to be shown the body instead of deciding.
const requestVerdicts = `{"credentialVerdicts":[{
	"id":"cvd_2","projectId":"project-1","kind":"request","origin":"judge","sandboxId":"sbx_a","useId":"use_1",
	"allow":false,"volunteered":false,"createdAt":"2026-09-02T10:00:01Z",
	"request":{"method":"PATCH","url":"https://api.github.com/repos/org/repo","body":{"mediaType":"application/json","length":42}},
	"round":1,"need":{"body":"json","bytes":512},"reason":"the change is in the body",
	"role":"judge","prompt":"{\"kind\":\"request\"}","promptVersion":"2","latencyMs":1500,
	"judgeSandboxId":"sbx_judge","harnessConfigId":"hc_1","image":"harness:1","imageDigest":"sha256:one"
},{
	"id":"cvd_1","projectId":"project-1","kind":"request","origin":"judge","sandboxId":"sbx_a","useId":"use_1",
	"allow":true,"volunteered":false,"createdAt":"2026-09-02T10:00:00Z",
	"request":{"method":"POST","url":"https://api.github.com/repos/org/repo/pulls"},
	"round":1,"reason":"that is the approved use","role":"judge","prompt":"{}","promptVersion":"2","latencyMs":812,
	"judgeSandboxId":"sbx_judge"
}]}`

// A request verdict says what was judged — the method and destination, not an
// argv — who recorded it, how long the round trip took, and an ask to be shown
// the body reads as an ask, not a denial or an allow.
func TestAuditCredsShowsTheProjectJudgesVerdicts(t *testing.T) {
	_, out, err := runAuditCreds(t, requestVerdicts)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header and two rows:\n%s", len(lines), out)
	}
	for _, want := range []string{"RTT", "JUDGED"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("header %q lacks %s", lines[0], want)
		}
	}
	for i, want := range [][]string{
		{"ask", "judge", "1.5s", "PATCH https://api.github.com/repos/org/repo", "the change is in the body"},
		{"allow", "judge", "812ms", "POST https://api.github.com/repos/org/repo/pulls"},
	} {
		for _, field := range want {
			if !strings.Contains(lines[i+1], field) {
				t.Fatalf("row %d = %q, want %q in it", i+1, lines[i+1], field)
			}
		}
	}
}

// In full, a request verdict names the judge that gave it and what it ran, and
// what it was told of the body and asked to see.
func TestAuditCredsPromptShowsWhichJudgeAnswered(t *testing.T) {
	_, out, err := runAuditCreds(t, requestVerdicts, "--prompt")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{
		"cvd_2  ask  judge",
		"request:  PATCH https://api.github.com/repos/org/repo",
		"body:     application/json, 42 bytes",
		"asked:    the body as json, up to 512 bytes",
		"judge:    sbx_judge",
		"harness:  hc_1",
		"image:    harness:1@sha256:one",
		"version:  2",
		"rtt:      1.5s",
		"rtt:      812ms",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAuditCredsSendsItsKind(t *testing.T) {
	query, _, err := runAuditCreds(t, `{"credentialVerdicts":[]}`, "--kind", "request")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := query.Get("kind"); got != "request" {
		t.Fatalf("kind = %q, want request", got)
	}
	if _, _, err := runAuditCreds(t, `{"credentialVerdicts":[]}`, "--kind", "requests"); err == nil {
		t.Fatal("expected an unknown --kind to be refused")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
		err  bool
	}{
		{in: "90m", want: now.Add(-90 * time.Minute)},
		{in: "2026-09-01T00:00:00Z", want: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{in: "-1h", err: true},
		{in: "yesterday", err: true},
	} {
		got, err := parseSince(tc.in, now)
		if tc.err {
			if err == nil {
				t.Fatalf("parseSince(%q) = %s, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || !got.Equal(tc.want) {
			t.Fatalf("parseSince(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
}

// runAuditHTTP runs `admin audit http` against a server answering with body,
// and returns the query it sent and what it wrote to each stream.
func runAuditHTTP(t *testing.T, body string, args ...string) (url.Values, string, string, error) {
	t.Helper()
	var query url.Values
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/audit/http" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	stdout, stderr := new(strings.Builder), new(strings.Builder)
	cmd := NewRootCommand()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "admin", "audit", "http"}, args...))
	err := cmd.Execute()
	return query, stdout.String(), stderr.String(), err
}

func TestAuditHTTPSendsItsFilters(t *testing.T) {
	sandboxID, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	poolID, err := idpkg.New("pool")
	if err != nil {
		t.Fatal(err)
	}
	query, _, _, err := runAuditHTTP(t, `{"exchanges":[],"unavailablePools":[]}`,
		"--discobox-id", sandboxID, "--pool", poolID, "--host", "api.github.com", "--use-id", "use_1", "--since", "30m", "--limit", "7")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if query.Get("sandboxId") != sandboxID || query.Get("poolId") != poolID || query.Get("host") != "api.github.com" ||
		query.Get("useId") != "use_1" || query.Get("limit") != "7" || query.Get("since") == "" {
		t.Fatalf("query = %v, want every filter", query)
	}
}

const httpAuditBody = `{"exchanges":[
	{"poolId":"pool-a","id":"http_2","createdAt":"2026-09-17T10:01:00Z","sandboxId":"sbx_1","method":"POST",
	 "url":"https://api.github.com/repos/o/r/pulls\u001b[1A\u202e","host":"api.github.com","status":201,"blocked":false,"swappedUseIds":["use_x","use_y"]},
	{"poolId":"pool-a","id":"http_1","createdAt":"2026-09-17T10:00:00Z","sandboxId":"sbx_1","method":"GET",
	 "url":"https://evil.example/","host":"evil.example","status":0,"blocked":true,"blockedReason":"host denied","swappedUseIds":[]}
],"unavailablePools":[{"poolId":"pool-c","reason":"its pool agent predates the audit read"}]}`

func TestAuditHTTPShowsWhatHappenedAndWhatIsMissing(t *testing.T) {
	_, stdout, stderr, err := runAuditHTTP(t, httpAuditBody)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"201", "use_x,use_y", "blocked"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("table missing %q:\n%s", want, stdout)
		}
	}
	// A policy refusal never reached an upstream; a bare 0 would read as a
	// failure it was not.
	if strings.Contains(stdout, " 0 ") {
		t.Fatalf("blocked request shown with status 0:\n%s", stdout)
	}
	for _, raw := range []string{"\x1b", "\u202e"} {
		if strings.Contains(stdout, raw) {
			t.Fatalf("table carries raw %q:\n%s", raw, stdout)
		}
	}
	// The answer is short a pool, and saying so is not part of the answer.
	if !strings.Contains(stderr, "pool-c") || !strings.Contains(stderr, "missing") {
		t.Fatalf("stderr = %q, want pool-c named as missing", stderr)
	}
	if strings.Contains(stdout, "pool-c") {
		t.Fatalf("the unavailable pool was written into the table:\n%s", stdout)
	}
}

func TestAuditHTTPJSONCarriesTheMissingPools(t *testing.T) {
	_, stdout, _, err := runAuditHTTP(t, httpAuditBody, "-o", "json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got struct {
		Exchanges        []struct{ URL string } `json:"exchanges"`
		UnavailablePools []struct {
			PoolID string `json:"poolId"`
		} `json:"unavailablePools"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(got.UnavailablePools) != 1 || got.UnavailablePools[0].PoolID != "pool-c" {
		t.Fatalf("unavailablePools = %+v", got.UnavailablePools)
	}
	if strings.Contains(stdout, "\u202e") || got.Exchanges[0].URL != "https://api.github.com/repos/o/r/pulls\x1b[1A\u202e" {
		t.Fatalf("json is not terminal-safe or not exact:\n%s", stdout)
	}
}

// A request the proxy refused says what refused it: its host policy, the
// credential judge, or the discobox API's gate — and a gate call carries the
// use it was let in or refused under, so --use-id finds it.
func TestAuditHTTPSaysWhatRefusedARequest(t *testing.T) {
	body := `{"exchanges":[
	{"poolId":"pool-a","id":"http_3","createdAt":"2026-09-17T10:02:00Z","sandboxId":"sbx_1","method":"POST",
	 "url":"https://api.discobox.internal/projects/default/sandboxes","host":"api.discobox.internal","status":201,"blocked":false,"swappedUseIds":["use_api"]},
	{"poolId":"pool-a","id":"http_2","createdAt":"2026-09-17T10:01:00Z","sandboxId":"sbx_1","method":"GET",
	 "url":"https://api.discobox.internal/projects/default/pools","host":"api.discobox.internal","status":0,"blocked":true,
	 "blockedReason":"gate: the call carries no live use of ai.discobox.sandbox","swappedUseIds":[]},
	{"poolId":"pool-a","id":"http_1","createdAt":"2026-09-17T10:00:00Z","sandboxId":"sbx_1","method":"DELETE",
	 "url":"https://api.github.com/repos/o/r","host":"api.github.com","status":0,"blocked":true,
	 "blockedReason":"judge: deleting a repository is not what use_gh was approved for","swappedUseIds":["use_gh"]}
],"unavailablePools":[]}`
	_, stdout, _, err := runAuditHTTP(t, body)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	lines := strings.Split(stdout, "\n")
	find := func(id string) string {
		for _, line := range lines {
			if strings.Contains(line, id) {
				return line
			}
		}
		t.Fatalf("no row for %s:\n%s", id, stdout)
		return ""
	}
	if !strings.Contains(stdout, "REFUSED BY") {
		t.Fatalf("table has no REFUSED BY column:\n%s", stdout)
	}
	if row := find("http_3"); !strings.Contains(row, "201") || !strings.Contains(row, "use_api") {
		t.Fatalf("admitted gate row = %q, want its status and its use", row)
	}
	if row := find("http_2"); !strings.Contains(row, "blocked") || !strings.Contains(row, "gate") {
		t.Fatalf("gate refusal row = %q, want it refused by the gate", row)
	}
	if row := find("http_1"); !strings.Contains(row, "judge") || !strings.Contains(row, "use_gh") {
		t.Fatalf("judge refusal row = %q, want it refused by the judge, with its use", row)
	}
}

func TestHTTPAuditRefuserReadsTheReason(t *testing.T) {
	for _, tc := range []struct {
		blocked bool
		reason  string
		want    string
	}{
		{false, "", ""},
		{true, "host denied", "host"},
		{true, "judge: not what the use was for", "judge"},
		{true, "gate: no live use", "gate"},
		{true, "", "policy"},
	} {
		if got := httpAuditRefuser(tc.blocked, tc.reason); got != tc.want {
			t.Errorf("httpAuditRefuser(%v, %q) = %q, want %q", tc.blocked, tc.reason, got, tc.want)
		}
	}
}

// The timeline says why a request was refused, not only that it was.
func TestTheTimelineSaysWhyARequestWasRefused(t *testing.T) {
	var exchange apimodel.HTTPAuditExchange
	if err := json.Unmarshal([]byte(`{"poolId":"pool-a","id":"http_2","createdAt":"2026-09-17T10:01:00Z","sandboxId":"sbx_1","method":"GET",
	 "url":"https://api.discobox.internal/projects/default/pools","host":"api.discobox.internal","status":0,"blocked":true,
	 "blockedReason":"gate: the call carries no live use of ai.discobox.sandbox","swappedUseIds":[]}`), &exchange); err != nil {
		t.Fatal(err)
	}
	if summary := httpAuditRecord(exchange).summary; !strings.Contains(summary, "refused: gate: the call carries no live use") {
		t.Fatalf("summary = %q, want what refused it and why", summary)
	}
}

func TestHookSummaryNamesThePromptOrTheToolAndWhatItRanOn(t *testing.T) {
	for name, tc := range map[string]struct{ payload, want string }{
		"claude-code bash":    {`{"tool_name":"Bash","tool_input":{"command":"git status","description":"Show status"}}`, "Bash: git status"},
		"claude-code edit":    {`{"tool_name":"Edit","tool_input":{"file_path":"/src/a.go","old_string":"x"}}`, "Edit: /src/a.go"},
		"claude-code batch":   {`{"tool_calls":[{"tool_name":"Bash"},{"tool_name":"Read"}]}`, "Bash,Read"},
		"codex apply_patch":   {`{"tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: a.go\n@@\n*** Add File: b.go\n*** End Patch"}}`, "apply_patch: a.go b.go"},
		"search in a path":    {`{"tool_name":"Grep","tool_input":{"pattern":"TODO","path":"/src"}}`, "Grep: TODO"},
		"batch without names": {`{"tool_calls":[{"tool_name":"Bash"},{"other":1}]}`, "Bash"},
		"codex mcp":           {`{"tool_name":"mcp__fs__read","tool_input":{"path":"/etc/hosts"}}`, "mcp__fs__read: /etc/hosts"},
		"opencode before":     {`{"tool":"read","args":{"filePath":"/src/a.go"}}`, "read: /src/a.go"},
		"opencode after":      {`{"tool":"bash","title":"ls -la"}`, "bash: ls -la"},
		"no subject":          {`{"tool_name":"TodoWrite","tool_input":{"todos":[]}}`, "TodoWrite"},
		"prompt":              {`{"hook_event_name":"UserPromptSubmit","prompt":"fix the\n\n  audit\tlist"}`, "prompt: fix the audit list"},
		"long prompt":         {`{"prompt":"` + strings.Repeat("a", 150) + `"}`, "prompt: " + strings.Repeat("a", hookSummaryMaxText-1) + "…"},
		"multi-line command":  {`{"tool_name":"Bash","tool_input":{"command":"cd x &&\n  make"}}`, "Bash: cd x && make"},
		"not a tool hook":     {`{"hook_event_name":"Stop"}`, ""},
		"not json":            {`not json`, ""},
		"empty payload":       {``, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hookSummary([]byte(tc.payload)); got != tc.want {
				t.Fatalf("hookSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExecEventRecordLeadsWithTheMessage(t *testing.T) {
	event := apimodel.SandboxExecEvent{ID: "e1", ExecId: apiclientgen.NewOptString("ex_1"), Type: "exec.attach.opened", Message: apiclientgen.NewOptString("attach opened: claude \"fix it\"")}
	if got := execEventRecord("sbx_1")(event).summary; got != `ex_1 attach opened: claude "fix it"` {
		t.Fatalf("summary = %q", got)
	}
	event.Message = apiclientgen.OptString{}
	if got := execEventRecord("sbx_1")(event).summary; got != "ex_1 exec.attach.opened" {
		t.Fatalf("summary without a message = %q", got)
	}
}

func TestHarnessHookRecordEscapesThePrompt(t *testing.T) {
	hook := apimodel.HarnessHookLog{ID: "h1", Provider: "codex-cli", Event: "UserPromptSubmit",
		Payload: []byte(`{"prompt":"clear\u001b[2J\r\nnext\u202eline"}`)}
	got := harnessHookRecord("sbx_1")(hook).summary
	if strings.ContainsAny(got, "\x1b\r\n\u202e") || !strings.Contains(got, "prompt: clear") || !strings.Contains(got, "next") {
		t.Fatalf("summary = %q, want the prompt on one line with its controls escaped", got)
	}
}
