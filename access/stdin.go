package access

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// Showing the judge what a command reads on stdin (ADR 26-09-27-905).
//
// `discobox new --json`, `gh api --input -` and the rest take their request on
// stdin so that free text never passes through a shell, and for them the argv
// says nothing about what the command does. So `run` reads a bounded prefix of
// its stdin before it asks for a value, sends it to be judged with the argv,
// says what it did not send, and hands the child exactly those bytes followed
// by the rest.

const (
	// maxJudgedStdin is the most of stdin the judge is shown. It travels
	// inside one protocol body, which agentcreds.MaxBodyBytes caps at 64 KiB,
	// and inside the judge's job, and JSON can write one byte as six
	// (\u0001, \u003c). At 8 KiB even that worst case leaves the rest of the
	// body room, and it is the most of an input the judge accepts
	// (judge.MaxBodyBytes). A request that decides a command is a fraction of
	// this.
	maxJudgedStdin = 8 << 10
	// stdinArrivalWait bounds how long `run` waits for stdin to end or reach
	// the bound before judging what has arrived. A here-document or a file is
	// read at once; this is for a pipe whose writer is slow or never closes.
	stdinArrivalWait = 5 * time.Second
)

// judgedStdin is what `run` read of its stdin before judging, and what the
// child reads instead of it.
type judgedStdin struct {
	in   io.Reader
	done chan struct{}

	mu      sync.Mutex
	arrived *sync.Cond
	// read is every byte taken from in, in order: what the judge was shown
	// and, when the read stopped past the bound, the byte that proved there
	// was more. The child is handed all of it.
	read []byte
	// ended says in reached its end, so read is all of it.
	ended bool
	// failed is why reading stopped short of the end or the bound.
	failed error
	// finished says fill has stopped, and in is the child's to read.
	finished bool
}

// readStdin reads a bounded prefix of stdin for the judge, or returns nil when
// stdin is not a file or a pipe (ADR 26-09-27-905 §4): a terminal is a person,
// and a socket is what an agent's shell tool hands a command that reads
// nothing — reading either would take input meant for the child, or wait on
// input that never ends.
func readStdin(ctx context.Context, in *os.File) *judgedStdin {
	info, err := in.Stat()
	if err != nil {
		return nil
	}
	if mode := info.Mode(); !mode.IsRegular() && mode&os.ModeNamedPipe == 0 {
		return nil
	}
	s := &judgedStdin{in: in, done: make(chan struct{})}
	s.arrived = sync.NewCond(&s.mu)
	go s.fill()
	wait := time.NewTimer(stdinArrivalWait)
	defer wait.Stop()
	select {
	case <-s.done:
	case <-wait.C:
	case <-ctx.Done():
	}
	return s
}

// fill reads until the end of stdin, or one byte past the bound, whichever is
// first. It is the only reader of in until it has finished.
func (s *judgedStdin) fill() {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		s.finished = true
		s.arrived.Broadcast()
		s.mu.Unlock()
	}()
	chunk := make([]byte, 4096)
	for {
		s.mu.Lock()
		room := maxJudgedStdin + 1 - len(s.read)
		s.mu.Unlock()
		if room <= 0 {
			return
		}
		n, err := s.in.Read(chunk[:min(room, len(chunk))])
		s.mu.Lock()
		s.read = append(s.read, chunk[:n]...)
		switch {
		case errors.Is(err, io.EOF):
			s.ended = true
		case err != nil:
			s.failed = err
		}
		s.arrived.Broadcast()
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// Reader is the child's stdin: every byte read for the judge, as it arrives,
// then the rest of stdin once the judge's read has stopped — so no byte is
// taken twice or lost between them, and a slow pipe reaches the child as fast
// as it is written.
func (s *judgedStdin) Reader() io.Reader {
	return &judgedReader{s: s}
}

// Pipe is Reader as a file the child is handed directly, fed from here. The
// child gets an OS pipe rather than a Go reader because exec copies a reader
// into the child on a goroutine that Wait waits for: a writer that keeps
// stdin open without writing would hold `run` open long after its command
// exited. Fed from here, nothing waits on the copy; it ends when stdin does,
// or at the first write after the child has gone. The caller closes the file
// once the child has started, so the child's is the only read end.
func (s *judgedStdin) Pipe() (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("pass stdin on to the command: %w", err)
	}
	go func() {
		// A child that exits without reading all of it closes the pipe, and
		// the copy stops at the write that fails.
		_, _ = io.Copy(w, s.Reader())
		_ = w.Close()
	}()
	return r, nil
}

type judgedReader struct {
	s    *judgedStdin
	next int
}

func (r *judgedReader) Read(p []byte) (int, error) {
	s := r.s
	s.mu.Lock()
	for r.next == len(s.read) && !s.finished {
		s.arrived.Wait()
	}
	if r.next < len(s.read) {
		n := copy(p, s.read[r.next:])
		r.next += n
		s.mu.Unlock()
		return n, nil
	}
	ended := s.ended
	s.mu.Unlock()
	if ended {
		return 0, io.EOF
	}
	return s.in.Read(p)
}

// evidence is stdin as the judge is shown it: the text that was read, and a
// sentence for anything not shown. Stdin that ended empty, or that was never
// read, says nothing.
func (s *judgedStdin) evidence() *agentcreds.Stdin {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	read, ended, failed := append([]byte(nil), s.read...), s.ended, s.failed
	s.mu.Unlock()
	if ended && len(read) == 0 {
		return nil
	}

	shown, notShown := read, ""
	switch {
	case len(read) > maxJudgedStdin:
		shown = read[:maxJudgedStdin]
		notShown = fmt.Sprintf("The input is longer than the %d bytes shown; the rest is not shown.", maxJudgedStdin)
	case failed != nil:
		notShown = fmt.Sprintf("Reading it failed after %d bytes (%s); the rest is not shown.", len(read), oneLine(failed.Error()))
	case !ended:
		notShown = fmt.Sprintf("The input had not ended after %s; only the %d bytes that had arrived are shown, and more may follow.", stdinArrivalWait, len(read))
	}
	if !ended || len(read) > maxJudgedStdin {
		// Cut where the read stopped, which may be inside a character.
		shown = trimPartialRune(shown)
	}
	if !utf8.Valid(shown) {
		text := fmt.Sprintf("The command will read %d bytes on standard input that are not text; they are not shown.", len(shown))
		if notShown != "" {
			text += " " + notShown
		}
		return &agentcreds.Stdin{Missing: text}
	}
	return &agentcreds.Stdin{Content: string(shown), Missing: notShown}
}

// oneLine keeps a diagnostic from turning one failure into a page of text.
func oneLine(text string) string {
	joined := strings.Join(strings.Fields(text), " ")
	if len(joined) > 400 {
		return joined[:400] + "…"
	}
	return joined
}

// trimPartialRune drops an incomplete UTF-8 sequence from the end of data,
// where a read that stopped short may have cut one.
func trimPartialRune(data []byte) []byte {
	for i := len(data) - 1; i >= 0 && i >= len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				return data[:i]
			}
			return data
		}
	}
	return data
}
