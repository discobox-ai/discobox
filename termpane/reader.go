package termpane

import (
	"errors"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// outputMsg carries a chunk of terminal output into Update. It is unexported
// because a host has nothing to do with it but pass it on, which it does by
// delegating every message to the pane.
type outputMsg struct{ data []byte }

// closedMsg reports that the stream ended, and is turned into the exported
// [ClosedMsg] once the pane has recorded it.
type closedMsg struct{ err error }

// DetachMsg is emitted when the reserved prefix is followed by the detach key.
// The pane keeps running: what to do about it — take focus away, close it, put
// the terminal in a corner — is the host's decision.
type DetachMsg struct{}

// ClosedMsg reports that the session ended, with the error that ended it or nil
// when the far end simply exited. End of file is an exit, not an error.
type ClosedMsg struct{ Err error }

// reader pumps a stream's output off the blocking read and into a channel the
// update loop drains, so no read ever happens inside Update.
//
// The channel is buffered because terminal output arrives in bursts — a screen
// repaint is dozens of writes — and a reader that has to wait for the UI
// between every one of them turns a burst into a stutter.
type reader struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once

	mu  sync.Mutex
	err error
}

const (
	// readChunk is the read buffer. A screenful of dense output is a few tens
	// of kilobytes, so this is one Read for most repaints.
	readChunk = 32 * 1024
	// readQueue is how many chunks may be waiting. Deep enough for a burst,
	// shallow enough that a pane nobody is draining cannot hold a megabyte.
	readQueue = 64
)

func newReader(stream Stream) *reader {
	r := &reader{ch: make(chan []byte, readQueue), done: make(chan struct{})}
	go r.loop(stream)
	return r
}

func (r *reader) loop(stream Stream) {
	defer close(r.ch)
	buf := make([]byte, readChunk)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case r.ch <- chunk:
			case <-r.done:
				return
			}
		}
		if err != nil {
			// End of file is the session ending, not something going wrong
			// with it. Recording it would have every host reporting a normal
			// exit as a failure.
			if !errors.Is(err, io.EOF) {
				r.setErr(err)
			}
			return
		}
	}
}

// next waits for the next output and delivers it as one message.
//
// Everything already queued behind it comes along in the same message: a burst
// of writes is one screen the user is waiting to see, and feeding it through
// the update loop a chunk at a time costs a render per chunk to draw the
// intermediate states of a repaint nobody asked to watch.
func (r *reader) next() tea.Cmd {
	if r == nil {
		return nil
	}
	return func() tea.Msg {
		chunk, ok := <-r.ch
		if !ok {
			return closedMsg{err: r.error()}
		}
		for {
			select {
			case more, ok := <-r.ch:
				if !ok {
					// The stream ended mid-burst. Draw what arrived; the next
					// read reports the close.
					return outputMsg{data: chunk}
				}
				chunk = append(chunk, more...)
			default:
				return outputMsg{data: chunk}
			}
		}
	}
}

func (r *reader) stop() {
	if r != nil {
		r.once.Do(func() { close(r.done) })
	}
}

func (r *reader) setErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *reader) error() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
