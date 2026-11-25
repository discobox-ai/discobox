package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ownBoxID is the discobox these tests pretend to run in.
const ownBoxID = "sbx_d592csswavy7c93c"

// attachFromBox runs `discobox attach --raw target` as the CLI inside a
// discobox whose sandbox config is at configPath, against a server that
// answers every request with the server's refusal of a discobox without API
// access. It returns what the server was asked and the attach's error.
func attachFromBox(t *testing.T, configPath, target string) ([]string, error) {
	t.Helper()
	var asked []string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		http.Error(w, "this discobox may not call the discobox API; a person allows it by setting discoboxApi on it", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	cmd, app := newRootCommand(false)
	app.sandboxConfigPath = configPath
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "attach", "--raw", target})
	err := cmd.Execute()
	return asked, err
}

func ownBoxConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"sandboxId":"`+ownBoxID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// `discobox attach` run inside a discobox, naming that same discobox, is a
// person who forgot where they are typing. The server refuses a box without
// discoboxApi every call, but the refusal's advice (turn the API on) is the
// wrong one: the attach says it is already in that box, before asking the
// server anything.
func TestAttachToTheBoxItRunsInSaysSo(t *testing.T) {
	config := ownBoxConfig(t)
	t.Setenv("DISCOBOX_ADDRESS", "discobox://peer/"+ownBoxID)
	for _, target := range []string{
		ownBoxID,
		"sbx-d592csswavy7c93c", // as the box's hostname spells it
		"discobox:///" + ownBoxID,
		"discobox://peer/" + ownBoxID, // its own server's address
	} {
		t.Run(target, func(t *testing.T) {
			asked, err := attachFromBox(t, config, target)
			if err == nil {
				t.Fatal("attach to the discobox it runs in succeeded")
			}
			if got := err.Error(); !strings.Contains(got, "already in discobox "+ownBoxID) || strings.Contains(got, "discoboxApi") {
				t.Fatalf("attach error = %q, want it to say this is already %s and not suggest discoboxApi", got, ownBoxID)
			}
			if len(asked) > 0 {
				t.Fatalf("attach asked the server %v before saying it is already in the box", asked)
			}
		})
	}
}

// Anything that does not name the box it runs in for certain without the
// server — another discobox, a name, a short ID that may be another box's, the
// same ID on another server — or an attach run outside a discobox goes to the
// server as it always did, and its refusal stands.
func TestAttachToAnotherBoxStillAsksTheServer(t *testing.T) {
	config := ownBoxConfig(t)
	t.Setenv("DISCOBOX_ADDRESS", "discobox://peer/"+ownBoxID)
	for _, tc := range []struct{ name, config, target string }{
		{"another discobox", config, "sbx_0000000000000000"},
		{"a name", config, "mybox"},
		{"a short ID", config, "sbx_d592"},
		{"a random-part short ID", config, "d592"},
		{"outside a discobox", filepath.Join(t.TempDir(), "missing.json"), ownBoxID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked, err := attachFromBox(t, tc.config, tc.target)
			if err == nil || strings.Contains(err.Error(), "already in") {
				t.Fatalf("attach error = %v, want the server's refusal", err)
			}
			if len(asked) == 0 {
				t.Fatal("attach did not ask the server")
			}
		})
	}
}

// The same ID behind another server's address names a discobox on that server,
// which the attach goes on to reach rather than calling it this one.
func TestAttachToTheSameIDOnAnotherServerIsNotThisBox(t *testing.T) {
	t.Setenv("DISCOBOX_ADDRESS", "discobox://peer/"+ownBoxID)
	_, err := attachFromBox(t, ownBoxConfig(t), "discobox://other.invalid/"+ownBoxID)
	if err != nil && strings.Contains(err.Error(), "already in") {
		t.Fatalf("attach error = %q, want it to try the other server", err)
	}
}
