package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	idpkg "github.com/discobox-ai/x/id"
)

func TestSinceFlag(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
		err  bool
	}{
		{in: "90m", want: now.Add(-90 * time.Minute)},
		{in: "3d", want: now.Add(-72 * time.Hour)},
		{in: "2w", want: now.Add(-14 * 24 * time.Hour)},
		{in: "2026-09-01T00:00:00Z", want: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{in: "-1h", err: true},
		{in: "forever", err: true},
		{in: "yesterday", err: true},
	} {
		since := newSinceFlag()
		err := since.Set(tc.in)
		if tc.err {
			if err == nil {
				t.Fatalf("--since %q = %s, want an error", tc.in, since.from(now))
			}
			continue
		}
		if got := since.from(now); err != nil || !got.Equal(tc.want) {
			t.Fatalf("--since %q = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if got := newSinceFlag().from(now); !got.IsZero() {
		t.Fatalf("--since left out = %s, want the zero time", got)
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
		"--discobox", sandboxID, "--pool", poolID, "--host", "api.github.com", "--since", "30m", "--limit", "7")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if query.Get("sandboxId") != sandboxID || query.Get("poolId") != poolID || query.Get("host") != "api.github.com" ||
		query.Get("limit") != "7" || query.Get("since") == "" {
		t.Fatalf("query = %v, want every filter", query)
	}
}

const httpAuditBody = `{"exchanges":[
	{"poolId":"pool-a","id":"http_2","createdAt":"2026-09-17T10:01:00Z","sandboxId":"sbx_1","method":"POST",
	 "url":"https://api.github.com/repos/o/r/pulls\u001b[1A\u202e","host":"api.github.com","status":201,"blocked":false,"swappedSecretIds":["sec_x","sec_y"]},
	{"poolId":"pool-a","id":"http_1","createdAt":"2026-09-17T10:00:00Z","sandboxId":"sbx_1","method":"GET",
	 "url":"https://evil.example/","host":"evil.example","status":0,"blocked":true,"blockedReason":"host denied"}
],"unavailablePools":[{"poolId":"pool-c","reason":"its pool agent predates the audit read"}]}`

func TestAuditHTTPShowsWhatHappenedAndWhatIsMissing(t *testing.T) {
	_, stdout, stderr, err := runAuditHTTP(t, httpAuditBody)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{"201", "sec_x,sec_y", "blocked"} {
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

// A request the proxy refused says what refused it: its host policy, a
// credential's routes, or the discobox API's gate.
func TestAuditHTTPSaysWhatRefusedARequest(t *testing.T) {
	body := `{"exchanges":[
	{"poolId":"pool-a","id":"http_3","createdAt":"2026-09-17T10:02:00Z","sandboxId":"sbx_1","method":"POST",
	 "url":"https://api.discobox.internal/projects/default/sandboxes","host":"api.discobox.internal","status":201,"blocked":false},
	{"poolId":"pool-a","id":"http_2","createdAt":"2026-09-17T10:01:00Z","sandboxId":"sbx_1","method":"GET",
	 "url":"https://api.discobox.internal/projects/default/pools","host":"api.discobox.internal","status":0,"blocked":true,
	 "blockedReason":"gate: the control plane is not reachable"},
	{"poolId":"pool-a","id":"http_1","createdAt":"2026-09-17T10:00:00Z","sandboxId":"sbx_1","method":"POST",
	 "url":"https://api.individual.githubcopilot.com/mcp","host":"api.individual.githubcopilot.com","status":0,"blocked":true,
	 "blockedReason":"route: this credential's route githubcopilot.com/mcp=blocked refuses api.individual.githubcopilot.com/mcp","swappedSecretIds":["sec_gh"]}
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
	if row := find("http_3"); !strings.Contains(row, "201") {
		t.Fatalf("admitted gate row = %q, want its status", row)
	}
	if row := find("http_2"); !strings.Contains(row, "blocked") || !strings.Contains(row, "gate") {
		t.Fatalf("gate refusal row = %q, want it refused by the gate", row)
	}
	if row := find("http_1"); !strings.Contains(row, "route") || !strings.Contains(row, "sec_gh") {
		t.Fatalf("route refusal row = %q, want it refused by the route, with its secret", row)
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
		{true, "route: this credential has no route to example.com/", "route"},
		{true, "gate: the control plane is not reachable", "gate"},
		{true, "credential unavailable: control plane unreachable", "credential"},
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
	 "blockedReason":"gate: the control plane is not reachable"}`), &exchange); err != nil {
		t.Fatal(err)
	}
	if summary := httpAuditRecord(exchange).summary; !strings.Contains(summary, "refused: gate: the control plane is not reachable") {
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
		"opencode after":      {`{"tool":"shell","args":{"command":"ls -la"},"status":"completed"}`, "shell: ls -la"},
		"opencode 1 after":    {`{"tool":"bash","title":"ls -la"}`, "bash: ls -la"},
		"copilot before":      {`{"toolName":"bash","toolArgs":{"command":"go test ./...","description":"Run tests"}}`, "bash: go test ./..."},
		"copilot args string": {`{"toolName":"bash","toolArgs":"{\"command\":\"go vet ./...\"}"}`, "bash: go vet ./..."},
		"copilot args other":  {`{"toolName":"bash","toolArgs":42}`, "bash"},
		"copilot permission":  {`{"hookName":"permissionRequest","toolName":"edit","toolInput":{"path":"/src/a.go"}}`, "edit: /src/a.go"},
		"omp tool call":       {`{"type":"tool_call","toolName":"bash","toolCallId":"c1","input":{"command":"go test ./..."},"agent":{"kind":"main"}}`, "bash: go test ./..."},
		"omp tool result":     {`{"type":"tool_result","toolName":"read","toolCallId":"c2","input":{"path":"/src/a.go"},"isError":false}`, "read: /src/a.go"},
		"pi tool call":        {`{"toolName":"bash","toolCallId":"c1","input":{"command":"go test ./..."},"cwd":"/workspace"}`, "bash: go test ./..."},
		"pi prompt":           {`{"messageType":"user","prompt":"fix the tests","cwd":"/workspace"}`, "prompt: fix the tests"},
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

// The timeline folds a run of exchanges by what they have in common — method,
// status, and where they went — never by path, which is what tells one call
// of a run from the next.
func TestHTTPAuditRecordsGroupByMethodStatusAndOrigin(t *testing.T) {
	t.Parallel()
	group := func(method string, status int, blocked bool, rawURL, host string) auditRecord {
		return httpAuditRecord(apimodel.HTTPAuditExchange{Method: method, Status: status, Blocked: blocked, URL: rawURL, Host: host})
	}
	a := group("GET", 200, false, "https://api.github.com/user", "api.github.com")
	b := group("GET", 200, false, "https://api.github.com/repos/x/y?page=2", "api.github.com")
	if a.group != b.group || a.group != "GET 200 https://api.github.com" {
		t.Fatalf("groups %q and %q, want both GET 200 https://api.github.com", a.group, b.group)
	}
	if a.groupSummary != "GET 200 https://api.github.com/..." {
		t.Fatalf("group summary = %q", a.groupSummary)
	}
	for _, other := range []auditRecord{
		group("POST", 200, false, "https://api.github.com/user", "api.github.com"),
		group("GET", 404, false, "https://api.github.com/user", "api.github.com"),
		group("GET", 0, true, "https://api.github.com/user", "api.github.com"),
		group("GET", 200, false, "http://api.github.com/user", "api.github.com"),
	} {
		if other.group == a.group {
			t.Fatalf("%q should not fold with %q", other.group, a.group)
		}
	}
	if connect := group("CONNECT", 200, false, "registry.npmjs.org:443", "registry.npmjs.org:443"); connect.group != "CONNECT 200 registry.npmjs.org:443" {
		t.Fatalf("a CONNECT is named by its host, got %q", connect.group)
	}
}

// The timeline line names the secrets a request carried (ADR 26-10-01-240),
// and the http table gives them a column.
func TestHTTPAuditNamesTheSecretsSpent(t *testing.T) {
	t.Parallel()
	exchange := apimodel.HTTPAuditExchange{Method: "POST", Status: 200, URL: "https://api.anthropic.com/v1/messages", SwappedSecretIds: []string{"sec_key"}}
	if summary := httpAuditRecord(exchange).summary; !strings.Contains(summary, "secrets=sec_key") {
		t.Fatalf("summary = %q, want the secret", summary)
	}
	table := httpAuditTable(false)
	names := table.names()
	row := table.row(exchange)
	for i, name := range names {
		if name == "SECRETS" {
			if row[i] != "sec_key" {
				t.Fatalf("SECRETS = %q, want sec_key", row[i])
			}
			return
		}
	}
	t.Fatalf("columns %v, want SECRETS", names)
}
