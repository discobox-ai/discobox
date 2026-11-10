package tui

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

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
