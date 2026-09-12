package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/hostid"
)

func testSandboxWithID(id string) apimodel.Sandbox {
	return apimodel.Sandbox{ID: id}
}

func TestMatchSandboxArgFullGeneratedIDAlwaysMatches(t *testing.T) {
	id, ok, err := matchSandboxArg("sbx_23x11jnw03w11nf2", nil, configuredName)
	if err != nil {
		t.Fatalf("matchSandboxArg: %v", err)
	}
	if !ok || id != "sbx_23x11jnw03w11nf2" {
		t.Fatalf("id=%q ok=%v, want a match with no candidates needed", id, ok)
	}
}

func TestMatchSandboxArgShortIDResolvesUniqueMatch(t *testing.T) {
	sandboxes := []apimodel.Sandbox{testSandboxWithID("sbx_h1ssjzhp60emtc2n")}
	id, ok, err := matchSandboxArg("sbx_h1ssj", sandboxes, configuredName)
	if err != nil {
		t.Fatalf("matchSandboxArg: %v", err)
	}
	if !ok || id != "sbx_h1ssjzhp60emtc2n" {
		t.Fatalf("id=%q ok=%v, want the unique prefix match", id, ok)
	}
}

func TestMatchSandboxArgNoMatchTreatedAsCommand(t *testing.T) {
	sandboxes := []apimodel.Sandbox{testSandboxWithID("sbx_h1ssjzhp60emtc2n")}
	id, ok, err := matchSandboxArg("ls", sandboxes, configuredName)
	if err != nil {
		t.Fatalf("matchSandboxArg: %v", err)
	}
	if ok || id != "" {
		t.Fatalf("id=%q ok=%v, want no match so the caller treats it as a command", id, ok)
	}
}

func TestMatchSandboxArgNonShortIDShapeNeverMatches(t *testing.T) {
	sandboxes := []apimodel.Sandbox{testSandboxWithID("sbx_h1ssjzhp60emtc2n")}
	for _, arg := range []string{"git-status", "./script.sh", "Ls", "-la"} {
		if id, ok, err := matchSandboxArg(arg, sandboxes, configuredName); ok || err != nil || id != "" {
			t.Fatalf("matchSandboxArg(%q): id=%q ok=%v err=%v, want no match", arg, id, ok, err)
		}
	}
}

func TestMatchSandboxArgAmbiguousShortIDErrors(t *testing.T) {
	sandboxes := []apimodel.Sandbox{
		testSandboxWithID("sbx_ab" + strings.Repeat("1", 14)),
		testSandboxWithID("sbx_ab" + strings.Repeat("2", 14)),
	}
	id, ok, err := matchSandboxArg("ab", sandboxes, configuredName)
	if err == nil {
		t.Fatal("matchSandboxArg: error = nil, want an ambiguity error")
	}
	if ok || id != "" {
		t.Fatalf("id=%q ok=%v, want no match alongside the error", id, ok)
	}
	if !strings.Contains(err.Error(), "matches more than one discobox") {
		t.Fatalf("error = %q, want an ambiguity message", err)
	}
}

// A full generated ID is trusted by shape alone, so `discobox shell` must not pay
// for a sandbox listing it does not need before creating the exec — the same
// no-extra-round-trip path a fully-specified --sandbox-id takes elsewhere.
func TestShellFullSandboxIDSkipsListingAndRunsCommand(t *testing.T) {
	const sandboxID = "sbx_23x11jnw03w11nf2"
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sandboxes"):
			t.Fatal("shell listed sandboxes despite a full generated ID")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if want := "/api/projects/project-1/sandboxes/" + sandboxID + "/execs"; r.URL.Path != want {
				t.Fatalf("create path = %q, want %q", r.URL.Path, want)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":["ls","-la"],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", sandboxID, "ls", "-la"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute shell error = nil")
	}
	if !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %q", err)
	}
	if command, _ := createBody["command"].([]any); len(command) != 2 || command[0] != "ls" || command[1] != "-la" {
		t.Fatalf("create body command = %v, want [ls -la]", createBody["command"])
	}
	if shell, ok := createBody["shell"].(bool); ok && shell {
		t.Fatalf("create body shell = %v, want unset alongside an explicit command", createBody["shell"])
	}
}

// A short ID that matches exactly one of the sandboxes `discobox ls` would show
// resolves to that sandbox, and everything after it is the command.
func TestShellShortIDMatchUsesListedSandbox(t *testing.T) {
	const sandboxID = "sbx_h1ssjzhp60emtc2n"
	var listed bool
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes":
			listed = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandboxes":[` + testSandboxJSON(sandboxID, "alpha", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z") + `]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if want := "/api/projects/project-1/sandboxes/" + sandboxID + "/execs"; r.URL.Path != want {
				t.Fatalf("create path = %q, want %q", r.URL.Path, want)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":["echo","hi"],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", "sbx_h1ssj", "echo", "hi"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute shell error = nil")
	}
	if !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %q", err)
	}
	if !listed {
		t.Fatal("expected a sandbox listing to resolve the short ID")
	}
	if command, _ := createBody["command"].([]any); len(command) != 2 || command[0] != "echo" || command[1] != "hi" {
		t.Fatalf("create body command = %v, want [echo hi]", createBody["command"])
	}
}

// When the first argument matches none of the sandboxes `discobox ls` would show,
// it is the start of the command instead, and the single listed sandbox is
// picked automatically the same way an omitted SANDBOX_ID would be elsewhere.
func TestShellUnmatchedFirstArgIsCommandAndPicksSoleSandbox(t *testing.T) {
	const sandboxID = "sbx_h1ssjzhp60emtc2n"
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandboxes":[` + testSandboxJSON(sandboxID, "alpha", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z") + `]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if want := "/api/projects/project-1/sandboxes/" + sandboxID + "/execs"; r.URL.Path != want {
				t.Fatalf("create path = %q, want %q", r.URL.Path, want)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":["echo","hi"],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", "echo", "hi"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute shell error = nil")
	}
	if !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %q", err)
	}
	if command, _ := createBody["command"].([]any); len(command) != 2 || command[0] != "echo" || command[1] != "hi" {
		t.Fatalf("create body command = %v, want [echo hi]", createBody["command"])
	}
}

// No command at all runs the sandbox user's login shell, exactly like the old
// root `discobox exec` did with no arguments.
func TestShellNoArgsRunsLoginShell(t *testing.T) {
	const sandboxID = "sbx_h1ssjzhp60emtc2n"
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandboxes":[` + testSandboxJSON(sandboxID, "alpha", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z") + `]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":[],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute shell error = nil")
	}
	if !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %q", err)
	}
	if shell, _ := createBody["shell"].(bool); !shell {
		t.Fatalf("create body shell = %v, want true for no command", createBody["shell"])
	}
	if _, ok := createBody["command"]; ok {
		t.Fatalf("create body command = %v, want unset alongside --shell", createBody["command"])
	}
}

func TestShellNoArgsIgnoresArchivedSandbox(t *testing.T) {
	const archivedID = "sbx_9kvq9z81yq2t3dwn"
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/projects/project-1/sandboxes" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		archived := strings.Replace(
			testSandboxJSON(archivedID, "old", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z"),
			`"desiredState":"present","displayState":"running"`,
			`"desiredState":"archived","displayState":"archived"`,
			1,
		)
		_, _ = w.Write([]byte(`{"sandboxes":[` + archived + `]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute shell error = nil")
	}
	if !strings.Contains(err.Error(), "no discoboxes were started from this directory") {
		t.Fatalf("execute shell error = %q, want the empty candidate message", err)
	}
}

// An ambiguous short ID is reported outright rather than silently falling
// back to treating it as a command, since its shape said it was meant as an
// ID reference.
func TestShellAmbiguousShortIDErrorsBeforeAnyExecCall(t *testing.T) {
	id1 := "sbx_ab" + strings.Repeat("1", 14)
	id2 := "sbx_ab" + strings.Repeat("2", 14)
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandboxes":[` +
				testSandboxJSON(id1, "alpha", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z") + `,` +
				testSandboxJSON(id2, "beta", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z") + `]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", "ab", "ls"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute shell error = nil")
	}
	if !strings.Contains(err.Error(), "matches more than one discobox") {
		t.Fatalf("execute shell error = %q, want an ambiguity message", err)
	}
}

// `discobox shell SANDBOX -- CMD` is the documented separator form. shell stops
// parsing flags at the sandbox, so pflag hands the -- through untouched and
// shell has to drop it itself; before it did, -- reached the sandbox as the
// command's argv[0].
func TestShellSeparatorAfterSandboxIsNotPartOfTheCommand(t *testing.T) {
	const sandboxID = "sbx_23x11jnw03w11nf2"
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":["ls","-la"],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", sandboxID, "--", "ls", "-la"})

	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %v", err)
	}
	command, _ := createBody["command"].([]any)
	if len(command) != 2 || command[0] != "ls" || command[1] != "-la" {
		t.Fatalf("create body command = %v, want [ls -la]", createBody["command"])
	}
}

// A -- before the sandbox says no argument names one, so a command whose first
// word happens to match a sandbox still runs as a command. Without this the
// separator would mean the opposite of what it says: `ls` would be read as the
// sandbox and `-la` as the whole command.
func TestShellSeparatorBeforeSandboxStopsSandboxMatching(t *testing.T) {
	const sandboxID = "sbx_h1ssjzhp60emtc2n"
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandboxes":[` + testSandboxJSON(sandboxID, "ls", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z") + `]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":["ls","-la"],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", "--", "ls", "-la"})

	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %v", err)
	}
	command, _ := createBody["command"].([]any)
	if len(command) != 2 || command[0] != "ls" || command[1] != "-la" {
		t.Fatalf("create body command = %v, want [ls -la] run as a command, not in the sandbox named ls", createBody["command"])
	}
}

// Only the leading separator is shell's. One the caller typed inside the
// command belongs to the command -- `git log -- path` has to reach git whole.
func TestShellSeparatorInsideCommandIsPassedThrough(t *testing.T) {
	const sandboxID = "sbx_23x11jnw03w11nf2"
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":["git"],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", sandboxID, "git", "log", "--", "docs/"})

	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %v", err)
	}
	command, _ := createBody["command"].([]any)
	if len(command) != 4 || command[0] != "git" || command[2] != "--" || command[3] != "docs/" {
		t.Fatalf("create body command = %v, want [git log -- docs/]", createBody["command"])
	}
}

// A separator with nothing after it leaves no command, which is the login
// shell -- the same as naming the sandbox alone.
func TestShellSeparatorWithNoCommandRunsLoginShell(t *testing.T) {
	const sandboxID = "sbx_23x11jnw03w11nf2"
	var createBody map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"exec":{"id":"ex_1","status":"starting","command":[],"workdir":"","tty":false,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case strings.HasSuffix(r.URL.Path, "/attach"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "shell", sandboxID, "--"})

	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "has ended with exit code 0") {
		t.Fatalf("execute shell error = %v", err)
	}
	if shell, _ := createBody["shell"].(bool); !shell {
		t.Fatalf("create body = %v, want the login shell", createBody)
	}
	if command, ok := createBody["command"].([]any); ok && len(command) != 0 {
		t.Fatalf("create body command = %v, want none", createBody["command"])
	}
}

// titledSandbox is a discobox whose agent has titled its terminal: the NAME
// the listing prints is the title, and the configured name underneath it is the
// generated one nobody sees.
func titledSandbox(id, name, title string) apimodel.Sandbox {
	sandbox := apimodel.Sandbox{ID: id, DisplayName: title}
	sandbox.Config.Name = name
	return sandbox
}

// The whole point of the two settings: a window title is free-form text, so
// under configuredName it must not be taken as a discobox reference. `discobox
// shell vim` runs vim, even beside a discobox whose terminal is titled "vim" —
// while `discobox rm vim`, which has no command word to confuse it with,
// resolves that discobox.
func TestMatchSandboxArgTitleMatchesOnlyUnderListedName(t *testing.T) {
	sandboxes := []apimodel.Sandbox{titledSandbox("sbx_h1ssjzhp60emtc2n", "brave-otter", "vim")}

	id, ok, err := matchSandboxArg("vim", sandboxes, configuredName)
	if err != nil || ok || id != "" {
		t.Fatalf("matchSandboxArg(vim, configuredName): id=%q ok=%v err=%v, want no match so vim runs", id, ok, err)
	}

	id, ok, err = matchSandboxArg("vim", sandboxes, listedName)
	if err != nil {
		t.Fatalf("matchSandboxArg(vim, listedName): %v", err)
	}
	if !ok || id != "sbx_h1ssjzhp60emtc2n" {
		t.Fatalf("matchSandboxArg(vim, listedName): id=%q ok=%v, want the titled discobox", id, ok)
	}
}

// The configured name keeps resolving under listedName, so a name learned
// before the discobox titled itself does not stop working.
func TestMatchSandboxArgConfiguredNameMatchesUnderBothSettings(t *testing.T) {
	sandboxes := []apimodel.Sandbox{titledSandbox("sbx_h1ssjzhp60emtc2n", "brave-otter", "vim")}
	for _, match := range []nameMatch{configuredName, listedName} {
		id, ok, err := matchSandboxArg("brave-otter", sandboxes, match)
		if err != nil {
			t.Fatalf("matchSandboxArg(brave-otter, %v): %v", match, err)
		}
		if !ok || id != "sbx_h1ssjzhp60emtc2n" {
			t.Fatalf("matchSandboxArg(brave-otter, %v): id=%q ok=%v, want the discobox", match, id, ok)
		}
	}
}

// A title longer than the NAME column is printed cut short, and the cut form is
// the only spelling the user can copy. It resolves, and so does the full title.
func TestMatchSandboxArgMatchesTheNameAsListed(t *testing.T) {
	const title = "Fixing the flaky reconciler integration test"
	listed := truncateTableValue(title, sandboxNameColumnWidth)
	if listed == title {
		t.Fatalf("fixture title is not long enough to be truncated: %q", listed)
	}
	sandboxes := []apimodel.Sandbox{titledSandbox("sbx_h1ssjzhp60emtc2n", "brave-otter", title)}

	for _, arg := range []string{title, listed} {
		id, ok, err := matchSandboxArg(arg, sandboxes, listedName)
		if err != nil {
			t.Fatalf("matchSandboxArg(%q): %v", arg, err)
		}
		if !ok || id != "sbx_h1ssjzhp60emtc2n" {
			t.Fatalf("matchSandboxArg(%q): id=%q ok=%v, want the titled discobox", arg, id, ok)
		}
	}
}

// An empty argument names nothing, even beside a discobox carrying no name of
// its own.
func TestMatchSandboxArgEmptyNamesNothing(t *testing.T) {
	sandboxes := []apimodel.Sandbox{titledSandbox("sbx_h1ssjzhp60emtc2n", "", "")}
	for _, match := range []nameMatch{configuredName, listedName} {
		if id, ok, err := matchSandboxArg("", sandboxes, match); ok || err != nil || id != "" {
			t.Fatalf("matchSandboxArg(\"\", %v): id=%q ok=%v err=%v, want no match", match, id, ok, err)
		}
	}
}

// `discobox shell` passes configuredName, and this is the test that says so:
// the settings above are only worth having if the command that must not take a
// window title as its argument actually asks for that.
//
// A discobox titled "vim" sits in the directory, and `discobox shell vim` has
// to leave "vim" as the command it runs. Under listedName the title would be
// eaten as the discobox reference and the command would come back empty, which
// is `shell` with no command -- the login shell, and no vim.
func TestResolveShellTargetLeavesATitleToTheCommand(t *testing.T) {
	const sandboxID = "sbx_h1ssjzhp60emtc2n"
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/projects/project-1/sandboxes" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"sandboxes":[` + namedSandboxJSON(sandboxID, "brave-otter", "vim", "present") + `]}`))
	}))
	t.Cleanup(server.Close)

	app := &App{serverURL: server.URL, projectID: "project-1", source: "."}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	_, gotID, _, cmdArgs, err := app.resolveShellTarget(cmd, []string{"vim"})
	if err != nil {
		t.Fatalf("resolveShellTarget: %v", err)
	}
	if got, want := strings.Join(cmdArgs, " "), "vim"; got != want {
		t.Fatalf("command = %q, want %q -- the window title was taken as the discobox", got, want)
	}
	// The discobox still comes from the directory's one candidate, as it would
	// with no argument at all.
	if gotID != sandboxID {
		t.Fatalf("discobox = %q, want %q", gotID, sandboxID)
	}
}
