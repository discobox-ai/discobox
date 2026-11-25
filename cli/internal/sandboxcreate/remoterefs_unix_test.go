//go:build darwin || linux

package sandboxcreate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A lookup made again in its turn, for a remote that wants a credential, runs
// while a create's window or status line owns the terminal, before there is a
// discobox to offer the Git credentials choice for. Nothing hands git the
// terminal there, so git must not ask on it: its "Username for …" prompt would
// be drawn into the window, with keys going to the window rather than to git.
// An askpass program still may answer (it is how an IDE hands over its token);
// it inherits git's environment, which is what this reads.
func TestRemoteRefsLookupInTurnDoesNotPromptOnTheTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	prompt := filepath.Join(t.TempDir(), "prompt")
	askpass := filepath.Join(t.TempDir(), "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\nprintf '%s\\n' \"${GIT_TERMINAL_PROMPT-unset}\" >> "+prompt+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	os.Unsetenv("GIT_TERMINAL_PROMPT")
	t.Setenv("GIT_ASKPASS", askpass)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)

	refs := newRemoteRefs(context.Background(), nil)
	url := server.URL + "/o/r.git"
	refs.prefetch(url)
	if _, err := refs.resolve(context.Background(), url, "", false); err == nil {
		t.Fatal("resolve of a remote that refuses every credential succeeded")
	}
	got, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatalf("the lookup in its turn never ran git's askpass: %v", err)
	}
	for _, value := range strings.Fields(string(got)) {
		if value != "0" {
			t.Fatalf("lookup in its turn ran git with GIT_TERMINAL_PROMPT=%s, want 0: git falls back to asking on the window's terminal", value)
		}
	}
}
