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
