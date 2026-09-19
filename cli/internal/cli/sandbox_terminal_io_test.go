package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/execstream/client"
	idpkg "github.com/discobox-ai/x/id"
)

// terminalCall is what the fake server saw of one request.
type terminalCall struct {
	method, path, query string
	body                map[string]any
}

// runTerminalCommand runs `admin terminal ARGS` against a server answering
// every request with reply, and returns what it was sent and printed.
func runTerminalCommand(t *testing.T, reply string, args ...string) (terminalCall, string, error) {
	t.Helper()
	var got terminalCall
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		got = terminalCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			_ = json.Unmarshal(data, &got.body)
		}
		if reply == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	defer server.Close()
	out := new(strings.Builder)
	cmd := NewRootCommand()
	cmd.SetOut(out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "admin", "terminal"}, args...))
	err := cmd.Execute()
	return got, out.String(), err
}

// jsonOf is value as the JSON a request carried it in.
func jsonOf(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func testSandboxID(t *testing.T) string {
	t.Helper()
	// A real generated ID: only the exact generated shape skips the short-ID
	// lookup against the live discobox list.
	id, err := idpkg.New("sbx")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTerminalScreenPrintsTheRenderedScreen(t *testing.T) {
	sandboxID := testSandboxID(t)
	call, out, err := runTerminalCommand(t,
		`{"rows":3,"cols":20,"cursorRow":0,"cursorCol":0,"cursorVisible":true,"altScreen":false,"lines":["$ make test","ok",""],"scrollback":["earlier"],"exited":false}`,
		"screen", "primary", "--discobox-id", sandboxID, "--scrollback", "5")
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	if call.path != "/api/projects/project-1/sandboxes/"+sandboxID+"/execs/primary/screen" || call.query != "scrollback=5" {
		t.Fatalf("request = %+v, want the primary terminal's screen with scrollback", call)
	}
	if out != "earlier\n$ make test\nok\n" {
		t.Fatalf("output = %q, want the scrollback then the screen, without trailing blank rows", out)
	}
}

// Input reads its arguments the way tmux send-keys does: a named key is that
// key, anything else is text, and --literal makes everything text.
func TestTerminalInputSendsKeysAndText(t *testing.T) {
	sandboxID := testSandboxID(t)
	call, out, err := runTerminalCommand(t, `{"resumeAfter":"2026-09-18T00:00:01.5Z"}`, "input", "primary", "--discobox-id", sandboxID, "run the tests", "Enter", "^C")
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if out != "2026-09-18T00:00:01.5Z\n" {
		t.Fatalf("output = %q, want the resume point to wait from", out)
	}
	if call.method != http.MethodPost || call.path != "/api/projects/project-1/sandboxes/"+sandboxID+"/execs/primary/input" {
		t.Fatalf("request = %+v", call)
	}
	if parts := jsonOf(t, call.body["input"]); parts != `[{"text":"run the tests"},{"key":"Enter"},{"key":"C-c"}]` {
		t.Fatalf("input = %s", parts)
	}

	call, _, err = runTerminalCommand(t, `{"resumeAfter":"2026-09-18T00:00:01Z"}`, "input", "primary", "--discobox-id", sandboxID, "--literal", "Enter")
	if err != nil {
		t.Fatalf("input --literal: %v", err)
	}
	if parts := jsonOf(t, call.body["input"]); parts != `[{"text":"Enter"}]` {
		t.Fatalf("literal input = %s", parts)
	}
}

func TestTerminalWaitIsOneCall(t *testing.T) {
	sandboxID := testSandboxID(t)
	call, out, err := runTerminalCommand(t,
		`{"reason":"hook","hook":{"id":"evt_2","terminalId":"exec_1","provider":"claude","event":"Stop","payload":{},"createdAt":"2026-09-18T00:00:02Z"},"resumeAfter":"2026-09-18T00:00:02Z"}`,
		"wait", "primary", "--discobox-id", sandboxID, "--hook", "Stop", "--after", "2026-09-18T00:00:01.5Z", "--quiet", "10s", "--timeout", "30s")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if call.path != "/api/projects/project-1/sandboxes/"+sandboxID+"/execs/primary/wait" {
		t.Fatalf("request = %+v", call)
	}
	if body := jsonOf(t, call.body); body != `{"timeoutSeconds":30,"until":{"after":"2026-09-18T00:00:01.5Z","hookEvents":["Stop"],"quietSeconds":10}}` {
		t.Fatalf("body = %s", body)
	}
	if out != "hook Stop evt_2 2026-09-18T00:00:02Z\n" {
		t.Fatalf("output = %q, want the reason, event, hook ID, and resume point", out)
	}

	// A wait that times out says so, and exits 124 as timeout(1) does.
	_, out, err = runTerminalCommand(t, `{"reason":"timeout","resumeAfter":"2026-09-18T00:00:01Z"}`, "wait", "primary", "--discobox-id", sandboxID, "--exit")
	var exit client.ExitError
	if !errors.As(err, &exit) || exit.Code != 124 || out != "timeout 2026-09-18T00:00:01Z\n" {
		t.Fatalf("err = %v, output = %q; want exit 124 and timeout", err, out)
	}

	for name, args := range map[string][]string{
		"nothing to wait for":               {},
		"a longer wait than one call holds": {"--exit", "--timeout", "90s"},
		"quiet in fractions":                {"--quiet", "1500ms"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := runTerminalCommand(t, `{"reason":"exit"}`, append([]string{"wait", "primary", "--discobox-id", sandboxID}, args...)...); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
