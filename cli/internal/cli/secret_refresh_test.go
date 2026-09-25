package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

//nolint:gosec // G101: a test fixture naming a command, not a credential.
const refreshTestSecretJSON = `{"id":"secret-1","projectId":"project-1","name":"github","type":"token","host":"github.com","maxGrantTTLSeconds":3600,"ttlSeconds":300,"refreshCommand":["printf","gho_fresh"],"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`

// A token got from a command starts with a value: create runs the command
// here for the first one, and a well-known ID supplies the command, name, and
// host nobody typed.
func TestSecretCreateRunsTheRefreshCommandForTheFirstValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs printf")
	}
	var posted map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/secrets" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(refreshTestSecretJSON))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "create", "--well-known", "com.github.api", "--refresh-command", "printf 'gho_first'", "--ttl", "10m"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create: %v", err)
	}
	value, _ := posted["value"].(map[string]any)
	if value["token"] != "gho_first" {
		t.Fatalf("posted value = %#v, want the command's output", posted["value"])
	}
	command, _ := posted["refreshCommand"].([]any)
	if len(command) != 2 || command[0] != "printf" || command[1] != "gho_first" {
		t.Fatalf("refreshCommand = %#v, want the argument vector as split", posted["refreshCommand"])
	}
	if posted["ttlSeconds"] != float64(600) || posted["name"] != "github" || posted["host"] != "github.com" || posted["wellKnownId"] != "com.github.api" {
		t.Fatalf("posted = %#v, want the lifetime and the well-known name and host", posted)
	}
}

// Entering a value for a well-known credential stores the value and no
// command: the two answers the window offers, from a shell.
func TestSecretCreateWellKnownWithATokenTakesNoCommand(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(refreshTestSecretJSON))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "create", "--well-known", "com.github.api", "--token", "ghp_typed"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := posted["refreshCommand"]; ok {
		t.Fatalf("posted = %#v, want no command with a typed value", posted)
	}
}

// Refresh runs the stored command and answers the open refresh request with
// what it printed, saying the command made it.
func TestSecretRefreshRunsTheCommandAndAnswersTheRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs printf")
	}
	var refreshed map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets":
			_, _ = w.Write([]byte(`{"secrets":[` + refreshTestSecretJSON + `]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets/secret-1":
			_, _ = w.Write([]byte(refreshTestSecretJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests":
			_, _ = w.Write([]byte(`{"secretRequests":[
				{"id":"req-other","projectId":"project-1","requestedBy":"sandbox:sb-1","type":"token","status":"pending","secretId":"secret-1","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"},
				{"id":"req-refresh","projectId":"project-1","requestedBy":"sandbox:sb-1","type":"token","status":"pending","secretId":"secret-1","reason":"refresh","refreshCause":"stale","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/secrets/secret-1/refresh":
			if err := json.NewDecoder(r.Body).Decode(&refreshed); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(refreshTestSecretJSON))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	// Without --run it says what it would run, and runs nothing.
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "refresh", "github"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "printf gho_fresh") || !strings.Contains(err.Error(), "--run") || refreshed != nil {
		t.Fatalf("bare refresh err = %v, sent %v; want the command named and nothing run", err, refreshed)
	}

	cmd = NewRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "refresh", "github", "--run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed["value"] != "gho_fresh" || refreshed["via"] != "command" || refreshed["requestId"] != "req-refresh" {
		t.Fatalf("refresh body = %#v, want the command's output answering the refresh request", refreshed)
	}
	if !bytes.Contains(out.Bytes(), []byte("REFRESH COMMAND")) {
		t.Fatalf("output = %q, want the refreshed secret", out.String())
	}
}

func TestSecretRefreshReadsAnEnteredValueFromStdin(t *testing.T) {
	var refreshed map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets":
			_, _ = w.Write([]byte(`{"secrets":[` + refreshTestSecretJSON + `]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets/secret-1":
			_, _ = w.Write([]byte(refreshTestSecretJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests":
			_, _ = w.Write([]byte(`{"secretRequests":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/secrets/secret-1/refresh":
			if err := json.NewDecoder(r.Body).Decode(&refreshed); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(refreshTestSecretJSON))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetIn(bytes.NewBufferString("gho_pasted\n"))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "refresh", "secret-1", "--value", "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed["value"] != "gho_pasted" || refreshed["via"] != "entered" {
		t.Fatalf("refresh body = %#v", refreshed)
	}
	if _, ok := refreshed["requestId"]; ok {
		t.Fatalf("refresh body = %#v, want no request when none is open", refreshed)
	}
}
