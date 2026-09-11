package tui

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// The rewriter is what a pane asks where a URL on its screen actually goes: the
// sandbox's port, answered with the local one the forward bound, on the name
// rather than the address.
func TestForwardedURLPointsAtTheLocalEnd(t *testing.T) {
	t.Parallel()
	m := &Model{forward: newFakeForward(
		Binding{Port: 8080, Local: 8081},
		Binding{Port: 443, Local: 8443},
		Binding{Port: 80, Local: 8000},
	)}

	for _, tc := range []struct{ raw, want string }{
		// The port moved, so the URL does.
		{"http://localhost:8080/", "http://localhost:8081/"},
		// The bind address a server prints is not one to open; the port is
		// still the sandbox's, and the answer is the same either way.
		{"http://0.0.0.0:8080/health?x=1", "http://localhost:8081/health?x=1"},
		{"http://127.0.0.1:8080", "http://localhost:8081"},
		{"http://[::1]:8080/", "http://localhost:8081/"},
		// A port left out is the scheme's, and forwardable like any other.
		{"https://localhost/admin", "https://localhost:8443/admin"},
		{"http://localhost/", "http://localhost:8000/"},
		// Everything else is already right, and saying so is what leaves it as
		// text for the terminal's own linking.
		{"http://localhost:9999/", "http://localhost:9999/"},
		{"https://github.com/discobox-ai/discobox", "https://github.com/discobox-ai/discobox"},
		{"postgres://localhost:8080/db", "postgres://localhost:8080/db"},
		{"http://example.com:8080/", "http://example.com:8080/"},
	} {
		if got := m.forwardedURL(tc.raw); got != tc.want {
			t.Errorf("forwardedURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// With nothing forwarded — every screen but the workspace, and the workspace
// until its forward binds — nothing is moved and nothing is linked.
func TestForwardedURLIsInertWithoutAForward(t *testing.T) {
	t.Parallel()
	var m Model
	if got, want := m.forwardedURL("http://localhost:8080/"), "http://localhost:8080/"; got != want {
		t.Errorf("forwardedURL = %q, want %q", got, want)
	}
}

// Whether a URL opened is the launcher's exit status, so that is what a press
// reports: xdg-open with no desktop session starts fine and exits 3, and a
// window that said "opening" over that would be a click that did nothing
// while saying it had. One still running past the grace period is the browser
// it exec'd, and has opened.
func TestLaunchReportsWhatTheLauncherSaid(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in launchers are POSIX sh and sleep")
	}
	for _, tc := range []struct {
		name   string
		script string
		// grace is generous for a launcher that exits, so a slow start on a
		// loaded machine cannot turn its answer into a timeout, and short for
		// the one that stays up, which is the case that needs one.
		grace   time.Duration
		exits   bool
		wantErr bool
	}{
		{name: "a launcher that handed the URL on", script: "exit 0", grace: time.Minute, exits: true},
		{name: "a launcher with nowhere to send it", script: "exit 3", grace: time.Minute, exits: true, wantErr: true},
		{name: "a launcher that stays up as the browser", script: "exec sleep 30", grace: 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The test's context ends the one that stays up, so nothing is
			// left running behind the test.
			start := time.Now()
			//nolint:gosec // the scripts are this table's own literals
			err := launch(exec.CommandContext(t.Context(), "sh", "-c", tc.script), tc.grace)
			if (err != nil) != tc.wantErr {
				t.Fatalf("launch = %v, want error %v", err, tc.wantErr)
			}
			// A launcher that exits is answered by its exit, not by the grace
			// running out — which is what would make a clean exit and a hung
			// launcher read the same.
			if elapsed := time.Since(start); tc.exits && elapsed >= tc.grace {
				t.Fatalf("launch took %v, want it answered by the exit rather than the %v grace", elapsed, tc.grace)
			}
		})
	}
}
