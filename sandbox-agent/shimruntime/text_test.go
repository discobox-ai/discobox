package shimruntime

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/programstatus"
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
	parts := []InputPart{{Text: "fix it\x1b[201~now"}, {Key: "Up"}, {Key: "Enter"}, {Key: "C-c"}, {Key: "Space"}}

	got, err := r.EncodeInput(parts)
	if err != nil {
		t.Fatal(err)
	}
	if want := "fix it\x1b[201~now\x1b[A\r\x03 "; string(got) != want {
		t.Fatalf("plain input = %q, want %q", got, want)
	}

	r.Broadcast(frame.Stdout, []byte("\x1b[?2004h\x1b[?1h"))
	got, err = r.EncodeInput(parts)
	if err != nil {
		t.Fatal(err)
	}
	// Space is a key press even under bracketed paste, never a pasted " ".
	if want := "\x1b[200~fix itnow\x1b[201~\x1bOA\r\x03 "; string(got) != want {
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
	if got := r.Await(context.Background(), Await{Quiet: 60 * time.Millisecond}).Reason; got != WaitQuiet {
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
	if got := r.Await(context.Background(), Await{Quiet: 60 * time.Millisecond}).Reason; got != WaitQuiet {
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
	if got := r.Await(context.Background(), Await{Quiet: 60 * time.Millisecond}).Reason; got != WaitQuiet {
		t.Fatalf("reason = %q, want quiet", got)
	}
	if elapsed := time.Since(began); elapsed < 100*time.Millisecond {
		t.Fatalf("quiet after %v, before the input's quiet period ran out", elapsed)
	}
}

// Given a point to count from, quiet is measured from it rather than from the
// call: a wait resumed after a terminal went quiet since that point answers at
// once, and one that has not been quiet long enough waits only the rest
// (ADR 26-11-04-204 §4).
func TestAwaitQuietCountsFromSince(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	r.NoteInput()
	since := time.Now().UTC()
	time.Sleep(80 * time.Millisecond)

	began := time.Now()
	if got := r.Await(context.Background(), Await{Since: since, Quiet: 60 * time.Millisecond}).Reason; got != WaitQuiet {
		t.Fatalf("reason = %q, want quiet", got)
	}
	if elapsed := time.Since(began); elapsed > 40*time.Millisecond {
		t.Fatalf("quiet after %v, though the terminal had been quiet since the point", elapsed)
	}
}

// A program status ends a wait only on a report after the point it counts
// from: a state that held before it (the agent idle before it was sent a
// message) does not, and the same state reported again after it does.
func TestAwaitProgramCountsOnlyReportsAfterSince(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	r.Broadcast(frame.Stdout, []byte("\x1b]7501;state=idle\x1b\\"))
	time.Sleep(5 * time.Millisecond)
	since := time.Now().UTC()
	finished := []ProgramMatch{{State: programstatus.Idle}, {State: programstatus.Done}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if got := r.Await(ctx, Await{Since: since, Program: finished}).Reason; got != WaitTimeout {
		t.Fatalf("reason = %q, want timeout: the idle report came before the point", got)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		r.Broadcast(frame.Stdout, []byte("\x1b]7501;state=working\x1b\\"))
		time.Sleep(10 * time.Millisecond)
		r.Broadcast(frame.Stdout, []byte("\x1b]7501;state=done\x1b\\"))
	}()
	got := r.Await(context.Background(), Await{Since: since, Program: finished})
	if got.Reason != WaitProgram || got.Program == nil || got.Program.State != programstatus.Done {
		t.Fatalf("result = %+v, want the done report", got)
	}
	if !got.Program.ReportedAt.After(since) {
		t.Fatalf("reported at %v, not after %v", got.Program.ReportedAt, since)
	}
}

// blocked with a kind matches only that kind; without one, any.
func TestAwaitProgramMatchesBlockedKind(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	since := time.Now().UTC().Add(-time.Second)
	r.Broadcast(frame.Stdout, []byte("\x1b]7501;state=blocked:kind=permission\x1b\\"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	question := []ProgramMatch{{State: programstatus.Blocked, Kind: programstatus.Question}}
	if got := r.Await(ctx, Await{Since: since, Program: question}).Reason; got != WaitTimeout {
		t.Fatalf("reason = %q, want timeout: blocked on permission is not a question", got)
	}
	got := r.Await(context.Background(), Await{Since: since, Program: []ProgramMatch{{State: programstatus.Blocked}}})
	if got.Reason != WaitProgram || got.Program.Kind != programstatus.Permission {
		t.Fatalf("result = %+v, want blocked:permission", got)
	}
}

// The screen says when the input route last wrote to the terminal, which is
// where a wait counting from the last input begins. Other input — an attach's
// frames, a focus report among them — does not move it.
func TestScreenTextCarriesSentAt(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	r.NoteInput()
	if screen, err := r.ScreenText(0); err != nil || screen.SentAt != nil {
		t.Fatalf("screen = %+v, %v; want nothing sent yet", screen.SentAt, err)
	}
	before := time.Now().UTC()
	r.NoteSent()
	sent, err := r.ScreenText(0)
	if err != nil || sent.SentAt == nil || sent.SentAt.Before(before) {
		t.Fatalf("sentAt = %v, %v; want at or after %v", sent.SentAt, err, before)
	}
	r.NoteInput()
	if screen, _ := r.ScreenText(0); !screen.SentAt.Equal(*sent.SentAt) {
		t.Fatalf("attach input moved sentAt from %v to %v", sent.SentAt, screen.SentAt)
	}
}

func TestAwaitQuietEndsOnExitOrContext(t *testing.T) {
	r := screenRuntime(t, 4, 20)
	done := make(chan struct{})
	close(done)
	if got := r.Await(context.Background(), Await{Quiet: time.Hour, Done: done}).Reason; got != WaitExit {
		t.Fatalf("reason = %q, want exit", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if got := r.Await(ctx, Await{}).Reason; got != WaitTimeout {
		t.Fatalf("reason = %q, want timeout", got)
	}
}
