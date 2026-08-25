package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/discobox-ai/discobox/health"
)

// sgr matches the color escapes the styled line carries, so a test can measure
// what a terminal would actually show.
var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visible(text string) string { return sgr.ReplaceAllString(text, "") }

// statusLineOnPTY draws the given lines on a real terminal and returns
// everything written to it. A pty is the only way to exercise the branch that
// matters: isTerminalStream and terminalWidth both answer off the file
// descriptor, so a bytes.Buffer can only ever test the other half.
func statusLineOnPTY(t *testing.T, cols int, draw func(*statusLine)) string {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: uint16(cols)}); err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				captured.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	draw(newStatusLine(tty))
	tty.Close()
	<-done
	return captured.String()
}

// The whole point of the line: a start that passes through five phases costs
// one row, not five. A newline anywhere in what was written means a phase
// scrolled instead of being replaced.
func TestStatusLineRewritesPhasesInPlaceOnATerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	phases := []string{"opening the database", "migrating the database", "starting services"}
	out := statusLineOnPTY(t, 90, func(line *statusLine) {
		for _, phase := range phases {
			line.set(serverStartupText(health.Status{Status: health.StatusStarting, Phase: phase}))
		}
		line.clear()
	})
	if strings.Contains(out, "\n") {
		t.Fatalf("a phase scrolled instead of being replaced: %q", out)
	}
	for _, phase := range phases {
		if !strings.Contains(visible(out), phase) {
			t.Errorf("%q was never drawn", phase)
		}
	}
	// Erased before each write, so a shorter phase cannot leave the tail of a
	// longer one behind it, and once more by clear().
	if got, want := strings.Count(out, "\r\x1b[K"), len(phases)+1; got < want {
		t.Errorf("erase sequences = %d, want at least %d", got, want)
	}
	if !strings.HasSuffix(out, "\r\x1b[K") {
		t.Error("clear() left the line on the screen")
	}
}

// The spinner is prepended after the text has been fitted, so the budget has to
// account for it. A line that reaches the last cell of the row leaves the
// cursor somewhere \r\x1b[K cannot erase, and the overflow is then permanent.
func TestStatusLineLeavesRoomForTheSpinner(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	const cols = 60
	long := "preloading images — discobox-harness-claude-code:v0.1.0-alpha.7, 146.7 MiB of 1.8 GiB, 12/40 layers"
	out := statusLineOnPTY(t, cols, func(line *statusLine) {
		line.set(serverStartupText(health.Status{Status: health.StatusStarting, Phase: long}))
		line.clear()
	})
	for _, frame := range strings.Split(out, "\r\x1b[K") {
		if width := runeLen(visible(frame)); width >= cols {
			t.Fatalf("drew %d columns on a %d column terminal: %q", width, cols, visible(frame))
		}
	}
}

// The window's colors, so that a command which narrates a server start and then
// a discobox it is waiting for does not paint the two differently.
func TestStatusLineCarriesTheWindowsColors(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	out := statusLineOnPTY(t, 90, func(line *statusLine) {
		line.set("waiting for a pool to take it")
		line.clear()
	})
	if !strings.Contains(out, statusSpinnerColor) || !strings.Contains(out, statusTextColor) {
		t.Fatalf("line is unstyled: %q", out)
	}
}

// NO_COLOR takes the paint and leaves the glyph: the spinner is how a wait says
// it is alive, which is not a decoration.
func TestStatusLineKeepsTheSpinnerWithoutColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	out := statusLineOnPTY(t, 90, func(line *statusLine) {
		line.set("waiting for a pool to take it")
		line.clear()
	})
	if sgr.MatchString(out) {
		t.Errorf("NO_COLOR was ignored: %q", out)
	}
	if !strings.Contains(out, statusSpinnerFrames[0]) {
		t.Errorf("the spinner went with the color: %q", out)
	}
}

// Off a terminal there is no cursor to move and every reason to keep the record
// of what the wait was spent on — and no reason to write a spinner frame into
// a log file.
func TestStatusLineAppendsPlainLinesOffATerminal(t *testing.T) {
	var out bytes.Buffer
	line := newStatusLine(&out)
	line.set(serverStartupText(health.Status{Status: health.StatusStarting, Phase: "migrating the database"}))
	line.set(serverStartupText(health.Status{Status: health.StatusStarting, Phase: "starting services"}))
	line.clear()
	got := out.String()
	want := "starting discobox server · migrating the database\nstarting discobox server · starting services\n"
	if got != want {
		t.Fatalf("off-terminal output = %q, want %q", got, want)
	}
}

// The narration runs in its own goroutine and is canceled rather than joined,
// so a report can arrive after the caller has taken the stream back. Writing it
// then would leave text on a line the caller believes it cleared — and the
// spinner is a second writer with the same problem.
func TestStatusLineDrawsNothingAfterClear(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	out := statusLineOnPTY(t, 90, func(line *statusLine) {
		line.set("waiting for a pool to take it")
		line.clear()
		line.set("a report that lost the race")
		time.Sleep(3 * statusSpinnerInterval)
	})
	if strings.Contains(visible(out), "lost the race") {
		t.Fatalf("a late report was drawn: %q", out)
	}
	if !strings.HasSuffix(out, "\r\x1b[K") {
		t.Fatalf("the spinner drew after clear(): %q", out)
	}
}

func TestStatusLineIsANoOpWhenThereIsNowhereToNarrate(t *testing.T) {
	app := &App{quiet: true, errOut: &bytes.Buffer{}}
	if app.serverStartupLine() != nil {
		t.Fatal("--quiet still narrates")
	}
	app = &App{}
	line := app.serverStartupLine()
	if line != nil {
		t.Fatal("a launch with no error stream still narrates")
	}
	// Every method takes a nil line, so the launch path has no reporting to
	// switch on.
	line.set("anything")
	line.clear()
}

// "starting discobox server · starting" is the same word twice.
func TestServerStartupTextOmitsAnAbsentPhase(t *testing.T) {
	if got := serverStartupText(health.Status{Status: health.StatusStarting}); got != "starting discobox server" {
		t.Fatalf("serverStartupText() = %q", got)
	}
	if got := serverStartupText(health.Status{Status: health.StatusStarting, Phase: "  "}); got != "starting discobox server" {
		t.Fatalf("serverStartupText() with a blank phase = %q", got)
	}
}
