package shimruntime

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
)

func screenRuntime(t *testing.T, rows, cols uint16) *Runtime {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	r := New("test", done, nil)
	_, tty := screenPipe(t)
	r.EnableScreen(rows, cols, DefaultScrollbackLines, tty)
	return r
}

// The screen reads as the cells show it: a line a program overwrote is there
// once, as it ended, with no escape sequences and no trailing blanks.
func TestScreenTextRendersTheCells(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	r.Broadcast(frame.Stdout, []byte("\x1b]0;building\x07one\r\n\x1b[1mprogress 10%\x1b[0m\rprogress 99%\r\n$ "))

	text, err := r.ScreenText(0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"one", "progress 99%", "$", ""}; !slices.Equal(text.Lines, want) {
		t.Fatalf("lines = %q, want %q", text.Lines, want)
	}
	if text.Rows != 4 || text.Cols != 20 || text.CursorRow != 2 || text.CursorCol != 2 || !text.CursorVisible {
		t.Fatalf("geometry = %+v", text)
	}
	if text.Title != "building" || text.AltScreen || text.OutputAt == nil {
		t.Fatalf("title, alt screen, output time = %q, %v, %v", text.Title, text.AltScreen, text.OutputAt)
	}
}

// Scrollback is the lines above the screen, oldest first, up to the count asked.
func TestScreenTextScrollback(t *testing.T) {
	r := screenRuntime(t, 2, 10)
	r.Broadcast(frame.Stdout, []byte("a\r\nb\r\nc\r\nd"))

	text, err := r.ScreenText(1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(text.Lines, []string{"c", "d"}) || !slices.Equal(text.Scrollback, []string{"b"}) {
		t.Fatalf("lines = %q, scrollback = %q", text.Lines, text.Scrollback)
	}
}

// A runtime with no screen — a pipe exec — has nothing to read or type into.
func TestTerminalOperationsNeedAScreen(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	r := New("test", done, nil)
	if _, err := r.ScreenText(0); !errors.Is(err, ErrNoScreen) {
		t.Fatalf("screen text err = %v, want ErrNoScreen", err)
	}
	if _, err := r.EncodeInput([]InputPart{{Text: "x"}}); !errors.Is(err, ErrNoScreen) {
		t.Fatalf("encode input err = %v, want ErrNoScreen", err)
	}
}

// Input arrives the way the program asked for it: text as a bracketed paste
// once it enabled that, cursor keys in application mode once it set that.
func TestEncodeInputFollowsTheProgramsModes(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	parts := []InputPart{{Text: "fix it\x1b[201~now"}, {Key: "Up"}, {Key: "Enter"}, {Key: "C-c"}}

	got, err := r.EncodeInput(parts)
	if err != nil {
		t.Fatal(err)
	}
	if want := "fix it\x1b[201~now\x1b[A\r\x03"; string(got) != want {
		t.Fatalf("plain input = %q, want %q", got, want)
	}

	r.Broadcast(frame.Stdout, []byte("\x1b[?2004h\x1b[?1h"))
	got, err = r.EncodeInput(parts)
	if err != nil {
		t.Fatal(err)
	}
	if want := "\x1b[200~fix itnow\x1b[201~\x1bOA\r\x03"; string(got) != want {
		t.Fatalf("input under paste and app cursor = %q, want %q", got, want)
	}
}

func TestEncodeInputRefusesMalformedParts(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	for name, part := range map[string]InputPart{
		"both":        {Text: "x", Key: "Enter"},
		"neither":     {},
		"unknown key": {Key: "Hyper"},
		"bad control": {Key: "C-1"},
	} {
		if _, err := r.EncodeInput([]InputPart{part}); err == nil {
			t.Errorf("%s: encoded without error", name)
		}
	}
}

// Quiet is measured from the last output: output restarts it.
func TestAwaitQuietRestartsOnOutput(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	r.Broadcast(frame.Stdout, []byte("start"))

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; i < 5; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				r.Broadcast(frame.Stdout, []byte("."))
			}
		}
	}()
	began := time.Now()
	if got := r.AwaitQuiet(context.Background(), 60*time.Millisecond, nil); got != WaitQuiet {
		t.Fatalf("reason = %q, want quiet", got)
	}
	if elapsed := time.Since(began); elapsed < 140*time.Millisecond {
		t.Fatalf("quiet after %v, before the output stopped", elapsed)
	}
}

// Output from before a wait does not make the terminal quiet: a program just
// typed to has not had its chance to answer (the race a lead hit typing and
// then waiting).
func TestAwaitQuietCountsFromTheCall(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	r.Broadcast(frame.Stdout, []byte("ready"))
	time.Sleep(80 * time.Millisecond)

	began := time.Now()
	if got := r.AwaitQuiet(context.Background(), 60*time.Millisecond, nil); got != WaitQuiet {
		t.Fatalf("reason = %q, want quiet", got)
	}
	if elapsed := time.Since(began); elapsed < 60*time.Millisecond {
		t.Fatalf("quiet after %v, on output from before the wait", elapsed)
	}
}

// Input restarts the quiet period the way output does.
func TestAwaitQuietRestartsOnInput(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	go func() {
		time.Sleep(40 * time.Millisecond)
		r.NoteInput()
	}()
	began := time.Now()
	if got := r.AwaitQuiet(context.Background(), 60*time.Millisecond, nil); got != WaitQuiet {
		t.Fatalf("reason = %q, want quiet", got)
	}
	if elapsed := time.Since(began); elapsed < 100*time.Millisecond {
		t.Fatalf("quiet after %v, before the input's quiet period ran out", elapsed)
	}
}

func TestAwaitQuietEndsOnExitOrContext(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	done := make(chan struct{})
	close(done)
	if got := r.AwaitQuiet(context.Background(), time.Hour, done); got != WaitExit {
		t.Fatalf("reason = %q, want exit", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if got := r.AwaitQuiet(ctx, 0, nil); got != WaitTimeout {
		t.Fatalf("reason = %q, want timeout", got)
	}
}
