package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// attachArchivedServer answers the discobox's record with the given desired
// and display states, and refuses everything else, counting it in beyond: the
// attach a refused discobox must never reach.
func attachArchivedServer(t *testing.T, sandboxID, desired, display string, beyond *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/projects/project-1/sandboxes/"+sandboxID {
			beyond.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Replace(
			testSandboxJSON(sandboxID, "old", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z"),
			`"desiredState":"present","displayState":"running"`,
			`"desiredState":"`+desired+`","displayState":"`+display+`"`,
			1,
		)))
	}))
	t.Cleanup(server.Close)
	return server
}

func runAttach(serverURL, sandboxID string, raw bool) error {
	args := []string{"--server", serverURL, "--project", "project-1", "attach", sandboxID}
	if raw {
		args = append(args, "--raw")
	}
	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

// A discobox archived by intent is refused before anything is attached —
// before the window opens, which would otherwise ask the terminal things it
// then ends without reading the answers to — and the refusal names the
// command that brings the discobox back. It is the desired state the server
// refuses on, so it is the desired state tested: an archive still being
// carried out, or one that failed, is refused as well.
func TestAttachRefusesAnArchivedDiscoboxWithTheUnarchiveCommand(t *testing.T) {
	const sandboxID = "sbx_9kvq9z81yq2t3dwn"
	want := "the discobox is archived; unarchive it first: discobox admin box unarchive " + sandboxID
	for _, display := range []string{"archived", "archiving", "error"} {
		for _, raw := range []bool{false, true} {
			var beyond atomic.Int32
			server := attachArchivedServer(t, sandboxID, "archived", display, &beyond)
			if err := runAttach(server.URL, sandboxID, raw); err == nil || err.Error() != want {
				t.Fatalf("attach (shown %s, raw %v) error = %v, want %q", display, raw, err, want)
			}
			if n := beyond.Load(); n != 0 {
				t.Fatalf("attach (shown %s, raw %v) asked %d more of the server after the record", display, raw, n)
			}
		}
	}
}

// An unarchive is recorded at once and carried out later, so until then the
// discobox still shows as archived. The server takes the attach then, and so
// does the command.
func TestAttachTakesADiscoboxBeingUnarchived(t *testing.T) {
	const sandboxID = "sbx_9kvq9z81yq2t3dwn"
	var beyond atomic.Int32
	server := attachArchivedServer(t, sandboxID, "present", "archived", &beyond)
	// The test server refuses the attach itself; what is tested is that
	// the command went on to ask for it.
	err := runAttach(server.URL, sandboxID, true)
	if err != nil && strings.Contains(err.Error(), "unarchive it first") {
		t.Fatalf("attach to a discobox being unarchived was refused: %v", err)
	}
	if beyond.Load() == 0 {
		t.Fatalf("attach to a discobox being unarchived never asked for the attach (error %v)", err)
	}
}
