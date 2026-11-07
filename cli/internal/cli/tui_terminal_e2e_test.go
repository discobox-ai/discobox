package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/endpoint"
)

const paneE2EEnv = "DISCOBOX_PANE_E2E"

// TestPaneTerminalsE2E opens the launcher's two pane terminals against a real
// control plane and reads until each proves it is alive.
//
// It is opt-in because it needs a running server and a running sandbox. It
// exists because the bug it catches cannot be caught anywhere else: a created
// exec is not a running one, and a pane that attaches without starting it draws
// an empty screen forever with no error to report. Nothing but a real sandbox
// can tell the difference between "attached and waiting for output" and
// "attached to something that was never started".
//
//	DISCOBOX_PANE_E2E=1 DISCOBOX_PANE_E2E_SANDBOX=sbx_... go test ./internal/cli -run PaneTerminalsE2E -v
func TestPaneTerminalsE2E(t *testing.T) {
	if os.Getenv(paneE2EEnv) != "1" {
		t.Skip("set " + paneE2EEnv + "=1, with DISCOBOX_PANE_E2E_SANDBOX naming a running sandbox")
	}
	sandboxID := strings.TrimSpace(os.Getenv("DISCOBOX_PANE_E2E_SANDBOX"))
	if sandboxID == "" {
		t.Fatal("DISCOBOX_PANE_E2E_SANDBOX is required")
	}

	app := &App{
		serverURL: envOrDefault("DISCOBOX_PANE_E2E_SERVER", endpoint.DefaultEndpoint()),
		projectID: envOrDefault("DISCOBOX_PANE_E2E_PROJECT", defaultProjectAlias),
		token:     os.Getenv("DISCOBOX_TOKEN"),
		autoStart: autoStartServerFalse,
	}
	client, err := app.apiClient()
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	ds := &apiDataSource{app: app, client: client, projectID: app.projectID}

	for _, action := range []console.Interaction{console.InteractShell, console.InteractAttach} {
		t.Run(string(action), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			var term console.Terminal
			var err error
			if action == console.InteractShell {
				_, term, err = ds.NewShell(ctx, sandboxID, 80, 24)
			} else {
				term, err = ds.OpenExec(ctx, sandboxID, console.ExecPrimary, 80, 24)
			}
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer term.Close()

			// A terminal that is running answers. A shell prints a prompt on
			// its own; anything else is made to speak by typing at it.
			if _, err := term.Write([]byte("echo PANE-ALIVE\r")); err != nil {
				t.Fatalf("write: %v", err)
			}
			out := readFor(ctx, term, 10*time.Second)
			if strings.TrimSpace(stripEscapes(out)) == "" {
				t.Fatalf("%s produced no output at all: attached to something that was never started", action)
			}
			t.Logf("%s: %q", action, truncateForLog(out))
		})
	}
}

// readFor accumulates everything the terminal says within a window. It is a
// window rather than a first-byte wait because a shell's own echo arrives ahead
// of its prompt, and one byte of echo is not proof that anything is running.
func readFor(ctx context.Context, term console.Terminal, within time.Duration) string {
	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		buf := make([]byte, 4096)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()

	var seen strings.Builder
	deadline := time.After(within)
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return seen.String()
			}
			seen.Write(chunk)
		case <-deadline:
			return seen.String()
		case <-ctx.Done():
			return seen.String()
		}
	}
}

// stripEscapes removes CSI and OSC sequences so a screen made only of cursor
// movement does not read as output.
func stripEscapes(s string) string {
	return ansi.Strip(s)
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
