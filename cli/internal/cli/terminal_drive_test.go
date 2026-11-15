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

// wait on a discobox that is still starting waits for it to start, within
// --for, and then waits on its terminal; one whose start fails exits with why;
// one stopped is named, not started; one still starting when --for passes
// exits 124. The terminal's wait is an observe and refuses a discobox not yet
// running (ADR 26-10-12-711 §1), so the waiting for the start is the CLI's.
func TestWaitOnAStartingDiscoboxWaitsForItToStart(t *testing.T) {
	sandboxID := testSandboxID(t)
	sandboxJSON := func(state, display, errorMessage string, more ...string) string {
		extra, runtimeState := strings.Join(more, ""), display
		if display == "error" {
			runtimeState = "stopped" // runtimeState is the container's; a failed start left none running
		}
		if errorMessage != "" {
			extra += `,"errorMessage":` + jsonString(errorMessage)
		}
		return `{"id":"` + sandboxID + `","displayName":"box","projectId":"project-1","createdByUserId":"user-1","config":{"name":"box","image":""},` +
			`"runtime":{"state":"` + state + `","runtimeState":"` + runtimeState + `","displayState":"` + display + `","desiredState":"present","generation":1,"observedGeneration":1` + extra + `},` +
			`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	}
	serve := func(started func(gets int) string) (*httptest.Server, *int) {
		var mu sync.Mutex
		gets, waits := 0, new(int)
		running := false
		return httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes/"+sandboxID):
				gets++
				body := started(gets)
				running = strings.Contains(body, `"displayState":"running"`)
				_, _ = w.Write([]byte(body))
			case strings.HasSuffix(r.URL.Path, "/execs/primary/wait"):
				*waits++
				if !running {
					// What the pool answers for a box it has not made yet:
					// plain text, which the transport hands back as an error.
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte("sandbox not found\n"))
					return
				}
				_, _ = w.Write([]byte(`{"reason":"program","programStatus":{"state":"idle","reportedAt":"2026-11-04T00:01:30Z"},"resumeAfter":"P"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"unexpected ` + r.Method + ` ` + r.URL.Path + `"}`))
			}
		})), waits
	}
	run := func(serverURL string, args ...string) (string, string, error) {
		out, errOut := new(strings.Builder), new(strings.Builder)
		cmd := NewRootCommand()
		cmd.SetOut(out)
		cmd.SetErr(errOut)
		cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1"}, args...))
		err := cmd.Execute()
		return out.String(), errOut.String(), err
	}

	t.Run("starts", func(t *testing.T) {
		server, waits := serve(func(gets int) string {
			if gets < 3 {
				return sandboxJSON("pending", "starting", "")
			}
			return sandboxJSON("ready", "running", "")
		})
		defer server.Close()
		out, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "30s")
		if err != nil {
			t.Fatalf("wait on a starting discobox: %v, want it to wait for the start", err)
		}
		if out != "finished P\n" {
			t.Fatalf("wait printed %q, want the agent's state once started", out)
		}
		if *waits != 2 {
			t.Fatalf("wait called the terminal %d times, want once refused and once after the start", *waits)
		}
	})

	t.Run("fails to start", func(t *testing.T) {
		server, _ := serve(func(gets int) string {
			if gets < 2 {
				return sandboxJSON("pending", "starting", "")
			}
			return sandboxJSON("failed", "error", "feature node: install failed")
		})
		defer server.Close()
		_, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "30s")
		if err == nil || !strings.Contains(err.Error(), "feature node: install failed") {
			t.Fatalf("wait on a discobox that fails to start: err = %v, want the start's failure", err)
		}
	})

	t.Run("stopped", func(t *testing.T) {
		server, waits := serve(func(int) string { return sandboxJSON("ready", "stopped", "") })
		defer server.Close()
		_, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "30s")
		if err == nil || !strings.Contains(err.Error(), "is stopped") || strings.Contains(err.Error(), "not found") {
			t.Fatalf("wait on a stopped discobox: err = %v, want it to say the box is stopped", err)
		}
		if *waits != 1 {
			t.Fatalf("wait called the terminal %d times on a stopped discobox, want once", *waits)
		}
	})

	t.Run("still starting when --for passes", func(t *testing.T) {
		server, _ := serve(func(int) string { return sandboxJSON("pending", "starting", "") })
		defer server.Close()
		out, errOut, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "1s")
		var exit client.ExitError
		if !errors.As(err, &exit) || exit.Code != 124 {
			t.Fatalf("wait --for 1s on a box that does not start: err = %v, want exit 124", err)
		}
		if out != "" || !strings.Contains(errOut, "still starting") {
			t.Fatalf("wait printed %q and %q, want nothing and that the box is still starting", out, errOut)
		}
	})

	t.Run("provisioning fails with no message of the server's", func(t *testing.T) {
		server, _ := serve(func(int) string {
			return sandboxJSON("ready", "error", "", `,"agentStatus":{"provision":{"ready":false,"error":"feature node: install failed"}}`)
		})
		defer server.Close()
		_, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "30s")
		if err == nil || !strings.Contains(err.Error(), "discobox preparation failed: feature node: install failed") {
			t.Fatalf("wait on a discobox whose provisioning failed: err = %v, want the agent's reason", err)
		}
	})

	t.Run("a failure the box wrote is escaped", func(t *testing.T) {
		server, _ := serve(func(int) string { return sandboxJSON("failed", "error", "install failed\n\u001b]52;c;cHduZWQ=\u0007") })
		defer server.Close()
		_, _, err := run(server.URL, "wait", sandboxID, "--until", "finished")
		if err == nil || strings.ContainsAny(err.Error(), "\x1b\x07") || !strings.Contains(err.Error(), "install failed\n") {
			t.Fatalf("wait on a discobox that failed: err = %q, want its reason with the escape sequence escaped and its lines kept", err)
		}
	})

	// The agent starts the primary terminal once provisioning is ready, and
	// refuses it until then even though the discobox reads running.
	// refusals < 0 refuses every call; ended has the refusal say the terminal
	// ended, as the agent says of a restarted box's previous primary.
	primaryNotUp := func(refusals int, ended bool) (*httptest.Server, *int) {
		var mu sync.Mutex
		waits := new(int)
		return httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(sandboxJSON("ready", "running", "")))
				return
			}
			*waits++
			if refusals < 0 || *waits <= refusals {
				w.WriteHeader(http.StatusConflict)
				if ended {
					_, _ = w.Write([]byte(`{"error":"sandbox exec has ended"}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"discobox primary terminal is not available; it may still be preparing; retry or attach to start it"}`))
				}
				return
			}
			_, _ = w.Write([]byte(`{"reason":"program","programStatus":{"state":"idle","reportedAt":"2026-11-04T00:01:30Z"},"resumeAfter":"P"}`))
		})), waits
	}

	t.Run("running, with its terminal not up yet", func(t *testing.T) {
		server, waits := primaryNotUp(2, false)
		defer server.Close()
		out, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "30s")
		if err != nil || out != "finished P\n" || *waits != 3 {
			t.Fatalf("wait while the terminal comes up: out %q, err %v, %d calls; want it to wait the terminal out", out, err, *waits)
		}
	})

	t.Run("running, with its terminal never up", func(t *testing.T) {
		server, _ := primaryNotUp(-1, false)
		defer server.Close()
		_, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "2s")
		if err == nil || !strings.Contains(err.Error(), "primary terminal is not available") {
			t.Fatalf("wait on a terminal that never comes up: err = %v, want the refusal once --for passes", err)
		}
	})

	// A restarted box still lists its previous run's primary, ended, until it
	// launches the new one, and the agent answers that it has ended: the
	// refusal is waited out as for one not yet started.
	t.Run("restarted, with its previous terminal listed", func(t *testing.T) {
		server, waits := primaryNotUp(2, true)
		defer server.Close()
		out, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "30s")
		if err != nil || out != "finished P\n" || *waits != 3 {
			t.Fatalf("wait on a restarted box: out %q, err %v, %d calls; want it to wait for the new terminal", out, err, *waits)
		}
	})

	t.Run("running, with its terminal ended", func(t *testing.T) {
		server, waits := primaryNotUp(-1, true)
		defer server.Close()
		_, _, err := run(server.URL, "wait", sandboxID, "--until", "finished", "--for", "2s")
		if err == nil || !strings.Contains(err.Error(), "has ended") || *waits < 2 {
			t.Fatalf("wait on a terminal that ended: err = %v after %d calls, want the refusal once --for passes", err, *waits)
		}
	})

	t.Run("running leaves the refusal", func(t *testing.T) {
		var gets int
		server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				gets++
				_, _ = w.Write([]byte(sandboxJSON("ready", "running", "")))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"exec not found"}`))
		}))
		defer server.Close()
		_, _, err := run(server.URL, "wait", sandboxID, "--until", "finished")
		if err == nil || !strings.Contains(err.Error(), "exec not found") || gets != 1 {
			t.Fatalf("wait refused on a running discobox: err = %v after %d reads, want the refusal after one", err, gets)
		}
	})
}

func jsonString(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
