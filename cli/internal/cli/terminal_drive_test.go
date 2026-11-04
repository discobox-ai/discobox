package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/pkg/execstream/client"
	idpkg "github.com/discobox-ai/x/id"
)

// runDriveCommand runs a top-level terminal command against a server that
// answers its nth request with replies[n] (the last one again past the end),
// and returns every request it was sent and what it printed.
func runDriveCommand(t *testing.T, replies []string, args ...string) ([]terminalCall, string, error) {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []terminalCall
	)
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		call := terminalCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			_ = json.Unmarshal(data, &call.body)
		}
		reply := replies[min(len(calls), len(replies)-1)]
		calls = append(calls, call)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	defer server.Close()
	out := new(strings.Builder)
	cmd := NewRootCommand()
	cmd.SetOut(out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1"}, args...))
	err := cmd.Execute()
	return calls, out.String(), err
}

// send types its words as one text part, on the primary terminal unless -t
// names another, and prints nothing (ADR 26-11-04-204 §2).
func TestSendTypesTextAndPrintsNothing(t *testing.T) {
	sandboxID := testSandboxID(t)
	calls, out, err := runDriveCommand(t, []string{`{"resumeAfter":"2026-11-04T00:00:01.5Z"}`}, "send", sandboxID, "run the", "tests Enter")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if out != "" {
		t.Fatalf("send printed %q, want nothing", out)
	}
	if len(calls) != 1 || calls[0].path != "/api/projects/project-1/sandboxes/"+sandboxID+"/execs/primary/input" {
		t.Fatalf("requests = %+v, want one input to the primary terminal", calls)
	}
	if parts := jsonOf(t, calls[0].body["input"]); parts != `[{"text":"run the tests Enter"}]` {
		t.Fatalf("input = %s, want every word as text", parts)
	}

	terminalID, err := idpkg.New("exec")
	if err != nil {
		t.Fatal(err)
	}
	calls, out, err = runDriveCommand(t, []string{`{"resumeAfter":"2026-11-04T00:00:01.5Z"}`}, "-o", "json", "send", sandboxID, "-t", terminalID, "hi")
	if err != nil {
		t.Fatalf("send -o json: %v", err)
	}
	if !strings.Contains(calls[0].path, "/execs/"+terminalID+"/input") || !strings.Contains(out, `"resumeAfter": "2026-11-04T00:00:01.5Z"`) {
		t.Fatalf("send -t TERMINAL -o json: request %s printed %q", calls[0].path, out)
	}
}

// send-key presses keys by their tmux names, and refuses a word that names no
// key rather than typing it.
func TestSendKeyPressesKeysAndRefusesText(t *testing.T) {
	sandboxID := testSandboxID(t)
	calls, out, err := runDriveCommand(t, []string{`{"resumeAfter":"2026-11-04T00:00:01Z"}`}, "send-key", sandboxID, "Escape", "BSpace", "Space", "C-c", "Enter")
	if err != nil {
		t.Fatalf("send-key: %v", err)
	}
	if out != "" {
		t.Fatalf("send-key printed %q, want nothing", out)
	}
	if parts := jsonOf(t, calls[0].body["input"]); parts != `[{"key":"Escape"},{"key":"Backspace"},{"key":"Space"},{"key":"C-c"},{"key":"Enter"}]` {
		t.Fatalf("input = %s", parts)
	}

	calls, _, err = runDriveCommand(t, []string{`{}`}, "send-key", sandboxID, "Enter", "Entr")
	if err == nil || !strings.Contains(err.Error(), `"Entr" names no key`) {
		t.Fatalf("send-key Entr: err = %v, want it refused as no key", err)
	}
	if len(calls) != 0 {
		t.Fatalf("send-key with a misspelled key sent %+v, want nothing sent", calls)
	}
}

// capture prints every row of the screen, blank ones included; --trim drops
// the blank rows below the last line written.
func TestCapturePrintsEveryRowUnlessTrimmed(t *testing.T) {
	sandboxID := testSandboxID(t)
	screen := `{"rows":4,"cols":20,"cursorRow":0,"cursorCol":0,"cursorVisible":true,"altScreen":false,"lines":["$ make test","","ok",""],"scrollback":["earlier"],"exited":false}`
	calls, out, err := runDriveCommand(t, []string{screen}, "capture", sandboxID, "--scrollback", "5")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if calls[0].path != "/api/projects/project-1/sandboxes/"+sandboxID+"/execs/primary/screen" || calls[0].query != "scrollback=5" {
		t.Fatalf("request = %+v", calls[0])
	}
	if out != "earlier\n$ make test\n\nok\n\n" {
		t.Fatalf("capture = %q, want every row", out)
	}
	_, out, err = runDriveCommand(t, []string{screen}, "capture", sandboxID, "--scrollback", "5", "--trim")
	if err != nil {
		t.Fatalf("capture --trim: %v", err)
	}
	if out != "earlier\n$ make test\n\nok\n" {
		t.Fatalf("capture --trim = %q, want the blank rows below the last line dropped", out)
	}
}

func TestParseWaitUntil(t *testing.T) {
	for _, tc := range []struct {
		untils  []string
		program string
		hooks   string
		quiet   string
		exit    bool
		err     string
	}{
		{untils: []string{"finished"}, program: `[{"state":"idle"},{"state":"done"},{"state":"error"}]`},
		{untils: []string{"blocked", "blocked:question"}, program: `[{"state":"blocked"},{"state":"blocked","kind":"question"}]`},
		{untils: []string{"idle"}, quiet: "5s"},
		{untils: []string{"idle=10s", "idle=3s"}, quiet: "3s"},
		{untils: []string{"exit", "Stop", "hook:idle", "session.idle"}, hooks: `["Stop","idle","session.idle"]`, exit: true},
		{untils: nil, err: "nothing to wait for"},
		{untils: []string{"blocked:questoin"}, err: "a blocked kind is permission, question, auth"},
		{untils: []string{"idle=1.5s"}, err: "whole number of seconds"},
		{untils: []string{"idle=0s"}, err: "whole number of seconds"},
		{untils: []string{"hook:"}, err: "names no hook"},
	} {
		until, err := parseWaitUntil(tc.untils)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: err = %v, want %q", tc.untils, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.untils, err)
			continue
		}
		if tc.program != "" && jsonOf(t, until.program) != tc.program {
			t.Errorf("%q: program = %s, want %s", tc.untils, jsonOf(t, until.program), tc.program)
		}
		if tc.hooks != "" && jsonOf(t, until.hooks) != tc.hooks {
			t.Errorf("%q: hooks = %s, want %s", tc.untils, jsonOf(t, until.hooks), tc.hooks)
		}
		if tc.quiet != "" && until.quiet.String() != tc.quiet {
			t.Errorf("%q: quiet = %s, want %s", tc.untils, until.quiet, tc.quiet)
		}
		if until.exit != tc.exit {
			t.Errorf("%q: exit = %v, want %v", tc.untils, until.exit, tc.exit)
		}
	}
}

// With no --after, the first wait counts from the terminal's last input, and
// one that runs out its 60s is made again from where it left off until
// something holds: here the agent finishing (ADR 26-11-04-204 §§4-5).
func TestWaitCountsFromTheLastInputAndCallsAgainUntilSomethingHolds(t *testing.T) {
	sandboxID := testSandboxID(t)
	calls, out, err := runDriveCommand(t, []string{
		`{"reason":"timeout","resumeAfter":"2026-11-04T00:00:01.5Z"}`,
		`{"reason":"program","programStatus":{"state":"idle","reportedAt":"2026-11-04T00:01:30Z"},"resumeAfter":"2026-11-04T00:01:30.25Z"}`,
	}, "wait", sandboxID, "--until", "finished", "--until", "blocked")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if out != "finished 2026-11-04T00:01:30.25Z\n" {
		t.Fatalf("wait printed %q, want what held and the resume point", out)
	}
	if len(calls) != 2 {
		t.Fatalf("wait made %d calls, want 2", len(calls))
	}
	first, second := calls[0].body, calls[1].body
	if until := first["until"].(map[string]any); until["fromLastInput"] != true || until["after"] != nil {
		t.Fatalf("first wait = %s, want it counting from the last input", jsonOf(t, first))
	}
	if first["timeoutSeconds"] != float64(60) {
		t.Fatalf("first wait timeout = %v, want the route's 60s", first["timeoutSeconds"])
	}
	if until := second["until"].(map[string]any); until["after"] != "2026-11-04T00:00:01.5Z" || until["fromLastInput"] != nil {
		t.Fatalf("second wait = %s, want it resuming where the first left off", jsonOf(t, second))
	}
	if states := jsonOf(t, first["until"].(map[string]any)["programStates"]); states != `[{"state":"idle"},{"state":"done"},{"state":"error"},{"state":"blocked"}]` {
		t.Fatalf("program states = %s", states)
	}
}

// --after replaces the last input as the point the wait counts from, and the
// answer names what held as --until would write it.
func TestWaitAnswersInTheWordsItWasAsked(t *testing.T) {
	sandboxID := testSandboxID(t)
	for _, tc := range []struct{ reply, want string }{
		{`{"reason":"program","programStatus":{"state":"blocked","kind":"question","reportedAt":"2026-11-04T00:00:02Z"},"resumeAfter":"P"}`, "blocked:question P\n"},
		{`{"reason":"program","programStatus":{"state":"blocked","reportedAt":"2026-11-04T00:00:02Z"},"resumeAfter":"P"}`, "blocked P\n"},
		{`{"reason":"quiet","resumeAfter":"P"}`, "idle P\n"},
		{`{"reason":"exit","resumeAfter":"P"}`, "exit P\n"},
		{`{"reason":"hook","hook":{"id":"evt_2","terminalId":"exec_1","provider":"opencode","event":"session.idle","canonicalEvent":"Stop","payload":{},"createdAt":"2026-11-04T00:00:02Z"},"resumeAfter":"P"}`, "hook:Stop evt_2 P\n"},
		{`{"reason":"hook","hook":{"id":"evt_3","terminalId":"exec_1","provider":"x","event":"odd\u001b[2J","payload":{},"createdAt":"2026-11-04T00:00:02Z"},"resumeAfter":"P"}`, "hook:odd\\x1b[2J evt_3 P\n"},
	} {
		calls, out, err := runDriveCommand(t, []string{tc.reply}, "wait", sandboxID, "--until", "blocked", "--until", "idle", "--until", "exit", "--until", "Stop", "--after", "2026-11-04T00:00:01Z")
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if out != tc.want {
			t.Errorf("wait printed %q, want %q", out, tc.want)
		}
		if until := calls[0].body["until"].(map[string]any); until["after"] != "2026-11-04T00:00:01Z" || until["fromLastInput"] != nil {
			t.Fatalf("wait = %s, want it counting from --after", jsonOf(t, calls[0].body))
		}
	}
}

// --for gives up when it has passed, says timeout, and exits 124. A call never
// asks for more than what is left of it.
func TestWaitForGivesUpAndExits124(t *testing.T) {
	sandboxID := testSandboxID(t)
	calls, out, err := runDriveCommand(t, []string{`{"reason":"timeout","resumeAfter":"2026-11-04T00:00:01Z"}`}, "wait", sandboxID, "--until", "finished", "--for", "1s")
	var exit client.ExitError
	if !errors.As(err, &exit) || exit.Code != 124 {
		t.Fatalf("wait --for 1s: err = %v, want exit 124", err)
	}
	if out != "timeout 2026-11-04T00:00:01Z\n" {
		t.Fatalf("wait --for printed %q, want timeout and the resume point", out)
	}
	if calls[0].body["timeoutSeconds"] != float64(1) {
		t.Fatalf("wait --for 1s asked the route for %v seconds, want 1", calls[0].body["timeoutSeconds"])
	}

	if _, _, err := runDriveCommand(t, []string{`{}`}, "wait", sandboxID); err == nil || !strings.Contains(err.Error(), "nothing to wait for") {
		t.Fatalf("wait with no --until: err = %v, want it refused", err)
	}
}
