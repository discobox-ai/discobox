package access

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// What a command reads on stdin is shown to the judge (ADR 26-09-27-905): for
// `discobox run --json` and its kind, it is the whole of what the command does.

// The request a command reads on stdin is sent to be judged, and the command
// still reads every byte of it.
func TestRunShowsTheJudgeWhatTheCommandReadsOnStdin(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	out := filepath.Join(t.TempDir(), "read")
	input := `{"prompt": "fix issue 43", "grants": [{"id": "com.github.api", "uses": [{"description": "push issue-43"}]}]}` + "\n"

	_, stderr, code := capture(t, input, func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "cat > '" + filepath.ToSlash(out) + "'"})
	})
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if got := svc.gotUse.Stdin; got == nil || got.Content != input || got.Missing != "" {
		t.Fatalf("stdin sent = %#v, want all of what the command reads", got)
	}
	if read, _ := os.ReadFile(out); string(read) != input {
		t.Fatalf("the command read %q, want exactly what was sent", read)
	}
}

// Past the bound the judge is told there is more, and the command still reads
// all of it, in order.
func TestRunSaysWhatOfStdinItDidNotShow(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	out := filepath.Join(t.TempDir(), "read")
	input := strings.Repeat("0123456789abcdef", maxJudgedStdin/16) + "the part past the bound"

	_, _, code := capture(t, input, func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "cat > '" + filepath.ToSlash(out) + "'"})
	})
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if got := svc.gotUse.Stdin; got == nil || len(got.Content) != maxJudgedStdin || !strings.Contains(got.Missing, "longer than the 8192 bytes shown") {
		t.Fatalf("stdin sent = %d bytes, want the first %d and that there is more", len(svc.gotUse.Stdin.Content), maxJudgedStdin)
	}
	if read, _ := os.ReadFile(out); string(read) != input {
		t.Fatalf("the command read %d bytes, want all %d, in order", len(read), len(input))
	}
}

// Nothing on stdin says nothing: a command fed an empty pipe is judged on its
// argv as before.
func TestAnEmptyStdinAddsNothing(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	if _, _, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "true"})
	}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if svc.gotUse.Stdin != nil {
		t.Fatalf("stdin sent = %#v, want nothing said about an empty stdin", svc.gotUse.Stdin)
	}
}

// A pipe still open when the judge is asked is judged on what arrived, said to
// be incomplete, and the command reads what came before and after.
func TestAPipeStillOpenIsJudgedOnWhatArrived(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.WriteString("first half, "); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stdin := readStdin(ctx, r)
	if stdin == nil {
		t.Fatal("a pipe was not read")
	}
	shown := stdin.evidence()
	if shown.Content != "first half, " || !strings.Contains(shown.Missing, "had not ended") {
		t.Fatalf("stdin shown = %#v, want what arrived and that more may follow", shown)
	}
	go func() {
		_, _ = w.WriteString("second half")
		_ = w.Close()
	}()
	read, err := io.ReadAll(stdin.Reader())
	if err != nil || string(read) != "first half, second half" {
		t.Fatalf("the child reads %q, %v; want every byte, in order", read, err)
	}
}

// Bytes that are not text are counted, not shown.
func TestStdinThatIsNotTextIsNotShown(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		_, _ = w.Write([]byte{0xff, 0xfe, 0x00, 0x01})
		_ = w.Close()
	}()
	shown := readStdin(context.Background(), r).evidence()
	if shown.Content != "" || !strings.Contains(shown.Missing, "4 bytes on standard input that are not text") {
		t.Fatalf("stdin shown = %#v, want the bytes counted and not shown", shown)
	}
}

// Only a file or a pipe is read. A character device — a terminal, /dev/null —
// is passed straight to the command, unread.
func TestStdinThatIsNotAFileOrAPipeIsNotRead(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if readStdin(context.Background(), null) != nil {
		t.Fatal("a character device was read")
	}
}

// A here-document the shell writes to a file is read whole.
func TestAFileOnStdinIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heredoc")
	if err := os.WriteFile(path, []byte("from a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stdin := readStdin(context.Background(), file)
	if stdin == nil || stdin.evidence().Content != "from a file" {
		t.Fatal("a file on stdin was not shown")
	}
	if read, _ := io.ReadAll(stdin.Reader()); !bytes.Equal(read, []byte("from a file")) {
		t.Fatalf("the child reads %q", read)
	}
}

// A read cut inside a character shows the characters before it.
func TestACutCharacterIsNotShownHalf(t *testing.T) {
	if got := trimPartialRune([]byte("ab\xe2\x82")); string(got) != "ab" {
		t.Fatalf("trimPartialRune = %q, want the whole characters", got)
	}
	if got := trimPartialRune([]byte("ab€")); string(got) != "ab€" {
		t.Fatalf("trimPartialRune = %q, want a whole character kept", got)
	}
}

// A writer that keeps stdin open does not hold `run` open once its command has
// exited: the child is fed through a pipe of its own, which nothing waits on.
func TestRunReturnsWhenItsCommandDoesThoughStdinStaysOpen(t *testing.T) {
	serve(t, &fakeService{})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Past the bound, so the judge's read stops at once rather than waiting
	// for the pipe to end, and the rest is left for the child. Written as it
	// is read: a pipe buffer is smaller than this on Windows.
	go func() { _, _ = w.Write(bytes.Repeat([]byte("x"), maxJudgedStdin+1)) }()
	realIn := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = realIn }()

	done := make(chan int, 1)
	go func() { done <- Run([]string{"run", "--use", "use_7f3c", "--", "true"}) }()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("exit = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run is still waiting on stdin after its command exited")
	}
}

// However JSON escapes it, the most of stdin a judge is shown fits one
// protocol body beside the argv and where it runs (ADR 26-09-27-905 §5).
func TestTheLargestStdinShownFitsOneUseCall(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		// Valid text, each byte of which JSON writes as six.
		_, _ = w.Write(bytes.Repeat([]byte{0x01}, maxJudgedStdin+1))
		_ = w.Close()
	}()
	stdin := readStdin(context.Background(), r)
	// Escaped as six bytes each too, as the worst a report can be.
	long := strings.Repeat("\x01", agentcreds.MaxReportedBytes)
	body, err := json.Marshal(agentcreds.UseBody{
		UseID:    "use_7f3c",
		Command:  []string{"discobox", "new", "--json"},
		Stdin:    stdin.evidence(),
		Reported: &agentcreds.Reported{WorkingDirectory: long, RepositoryRoot: long, RefCommit: long, RefSubject: long},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > agentcreds.MaxBodyBytes {
		t.Fatalf("the use call is %d bytes, over the %d the protocol takes", len(body), agentcreds.MaxBodyBytes)
	}
}
