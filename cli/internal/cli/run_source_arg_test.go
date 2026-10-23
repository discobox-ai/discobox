package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runWith runs `discobox run` against server with args and stdin, and answers
// with what it wrote to stderr.
func runWith(t *testing.T, serverURL, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var errOut bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1", "run"}, args...))
	err := cmd.Execute()
	return errOut.String(), err
}

// refusingServer fails the test on any request: what it guards is refused
// before anything is asked of a server.
func refusingServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// SOURCE is the first argument, and only a run with no arguments at all asks
// for one. Without a terminal there is nobody to ask, so it is required — and
// a --json request that names none is the same case.
func TestRunNeedsASourceWhenThereIsNobodyToAsk(t *testing.T) {
	t.Chdir(newRunSourceTestRepo(t))
	url := refusingServer(t)
	for name, tc := range map[string]struct {
		args  []string
		stdin string
	}{
		"no arguments":         {args: []string{"-d"}},
		"a prompt only by -p":  {args: []string{"-d", "-p", "fix it"}},
		"a json request":       {args: []string{"--json"}, stdin: `{"prompt": "fix it"}`},
		"a json request empty": {args: []string{"--json"}, stdin: `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runWith(t, url, tc.stdin, tc.args...); !errors.Is(err, errRunNeedsSource) {
				t.Fatalf("err = %v, want SOURCE required", err)
			}
		})
	}
}

// A prompt written where SOURCE goes, the way run used to take one, is refused
// with the form that works rather than a bare "no such file".
func TestRunSaysSourceComesFirst(t *testing.T) {
	t.Chdir(newRunSourceTestRepo(t))
	_, err := runWith(t, refusingServer(t), "", "-d", "fix the tests")
	// Quoted the way this platform's shell reads it, which is what the reader
	// pastes it into.
	want := "SOURCE comes first, before the prompt: discobox run . " + shellQuote("fix the tests", runtime.GOOS == "windows")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want the reminder that SOURCE comes first", err)
	}
}

// --no-source takes SOURCE's place, so every argument is the prompt.
func TestRunWithNoSourceTakesEveryArgumentAsThePrompt(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	var posted map[string]any
	server := runJSONServer(t, &posted)

	if errOut, err := runWith(t, server.URL, "", "-d", "--no-source", "draft", "a", "proposal"); err != nil {
		t.Fatalf("run: %v\n%s", err, errOut)
	}
	config := posted["config"].(map[string]any)
	if config["source"] != nil {
		t.Fatalf("source = %#v, want none", config["source"])
	}
	if prompt, _ := config["prompt"].([]any); len(prompt) != 3 || prompt[0] != "draft" {
		t.Fatalf("prompt = %#v, want every argument", config["prompt"])
	}
}

// Only committed history goes in unless --include-dirty asks for the rest, and
// what is left behind is said, so a forgotten flag is not a silent loss.
func TestRunLeavesUncommittedWorkOutUnlessAsked(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := newRunSourceTestRepo(t)
	t.Chdir(repo)
	if err := os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		args     []string
		wantMode string
		warned   bool
	}{
		{name: "by default", args: []string{"-d", "."}, wantMode: "clean", warned: true},
		{name: "--include-dirty", args: []string{"-d", "--include-dirty", "."}, wantMode: "dirty"},
		{name: "json by default", args: []string{"--json"}, wantMode: "clean", warned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posted map[string]any
			server := runJSONServer(t, &posted)
			stdin := ""
			if tc.args[0] == "--json" {
				stdin = `{"source": "."}`
			}
			errOut, err := runWith(t, server.URL, stdin, tc.args...)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, errOut)
			}
			source := posted["config"].(map[string]any)["source"].(map[string]any)
			if mode := source["workspace"].(map[string]any)["mode"]; mode != tc.wantMode {
				t.Fatalf("workspace mode = %v, want %s", mode, tc.wantMode)
			}
			if warned := strings.Contains(errOut, "1 uncommitted change left out (--include-dirty carries them)"); warned != tc.warned {
				t.Fatalf("stderr = %q, want the warning %v", errOut, tc.warned)
			}
		})
	}
}
