package bridge

import (
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// earlyLimit is how much the app may send on one stream before it is
// attached, each frame counted with its head: a paste typed into a pane
// still opening. Past it, frames are dropped; the app sends its pane's size
// again once the stream says it is attached.
const earlyLimit = 1 << 20

// streams is every terminal stream the app has allocated: kept frames for
// one not yet attached, the terminal of one that is, and the IDs of the ones
// that ended, whose late frames are dropped.
type streams struct {
	w    *Writer
	send func(Message)

	mu      sync.Mutex
	table   map[uint32]*stream
	retired map[uint32]bool
}

type stream struct {
	mu sync.Mutex
	// term is nil until the stream is attached.
	term console.Terminal
	// tab is the session the stream shows, as the core made it a tab: what
	// its ending does is the tab's (console.Tab.End).
	tab console.Tab
	// early is what the app sent before then, in order.
	early      []Frame
	earlyBytes int
	// closed is a stream the app closed before it was attached: the terminal
	// that attaches it later is closed at once.
	closed bool
	done   chan struct{}
	// watch reads a box's primary for what its program says (program.go);
	// nil on every other stream.
	watch *programWatch
	// ended is called once, when the stream is let go however that happens:
	// a files changed session, which is its tab's own, ended in its box
	// (Server.endFiles). Nil on every other stream.
	ended func()
}

func newStreams(w *Writer, send func(Message)) *streams {
	return &streams{w: w, send: send, table: map[uint32]*stream{}, retired: map[uint32]bool{}}
}

// get is a stream by ID, made pending if the app has not mentioned it yet;
// nil for one that has ended.
func (s *streams) get(id uint32) *stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == 0 || s.retired[id] {
		return nil
	}
	st := s.table[id]
	if st == nil {
		st = &stream{done: make(chan struct{})}
		s.table[id] = st
	}
	return st
}

// frame takes one frame the app sent on a stream: applied to an attached
// terminal, kept for one not attached yet, dropped for one that has ended.
// CloseInput is dropped: a box's terminal has no end of input apart from its
// end (console.Terminal), and the app ends a session with the end command.
func (s *streams) frame(f Frame) {
	switch f.Kind {
	case frame.Input, frame.Resize, frame.Repaint:
	default:
		return
	}
	st := s.get(f.Stream)
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if f.Kind == frame.Input {
		// The person at it, typing or pressing: a files changed tab they
		// quit is done with (console.Tab.End).
		st.tab.Typed = true
	}
	if st.term == nil {
		size := 9 + len(f.Payload)
		if st.closed || st.earlyBytes+size > earlyLimit {
			return
		}
		st.early = append(st.early, f)
		st.earlyBytes += size
		return
	}
	apply(st.term, f)
}

func apply(term console.Terminal, f Frame) {
	switch f.Kind {
	case frame.Input:
		_, _ = term.Write(f.Payload)
	case frame.Resize:
		var size frame.ResizePayload
		if json.Unmarshal(f.Payload, &size) == nil {
			_ = term.Resize(int(size.Cols), int(size.Rows))
		}
	case frame.Repaint:
		_ = term.Repaint()
	}
}

// attach gives a stream its terminal, showing tab: what was kept for it is
// applied in order, then its output and its connection's state go to the app
// until it ends or is closed, its output read by watch when it is a box's
// primary, and ended called once it is let go. A stream that ended or was
// closed while it opened takes no terminal, and false says the terminal was
// closed.
func (s *streams) attach(id uint32, term console.Terminal, tab console.Tab, watch *programWatch, ended func()) bool {
	st := s.get(id)
	if st == nil {
		_ = term.Close()
		return false
	}
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		s.retire(id)
		_ = term.Close()
		return false
	}
	// What was typed before it attached was typed at it.
	typed := st.tab.Typed
	st.term, st.tab, st.watch, st.ended = term, tab, watch, ended
	st.tab.Typed = st.tab.Typed || typed
	for _, f := range st.early {
		apply(term, f)
	}
	st.early, st.earlyBytes = nil, 0
	st.mu.Unlock()
	s.send(Message{Type: TypeStream, Stream: &Stream{Stream: id, State: StreamAttached}})
	go s.pump(id, st)
	go s.watch(id, st)
	return true
}

// pump sends the terminal's output on its stream, to be read for what its
// program says when it is a box's primary (program.go), and its end: what the
// ending does to the workspace (console.Tab.End), then one Exit frame
// carrying why, if it failed.
func (s *streams) pump(id uint32, st *stream) {
	buf := make([]byte, 32*1024)
	for {
		n, err := st.term.Read(buf)
		if n > 0 {
			_ = s.w.Write(frame.Stdout, id, append([]byte(nil), buf[:n]...))
			if st.watch != nil {
				st.watch.feed(buf[:n])
			}
		}
		if err != nil {
			select {
			case <-st.done:
				// Closed by the app, which knows.
				return
			default:
			}
			msg := ""
			var why error
			if !errors.Is(err, io.EOF) {
				msg, why = err.Error(), err
			}
			st.mu.Lock()
			end := wireEnd(st.tab.End(why))
			status := st.tab.Status
			st.mu.Unlock()
			s.send(Message{Type: TypeStream, Stream: &Stream{Stream: id, State: StreamEnded, Why: msg, End: end, Status: status}})
			_ = s.w.Write(frame.Exit, id, []byte(msg))
			s.end(id)
			return
		}
	}
}

// watch says when the stream's connection drops and comes back. Events is
// never closed while the terminal is open, so this ends with the stream.
func (s *streams) watch(id uint32, st *stream) {
	for {
		select {
		case <-st.done:
			return
		case ev := <-st.term.Events():
			state := StreamReconnected
			if ev.State == console.TerminalReconnecting {
				state = StreamReconnecting
			}
			s.send(Message{Type: TypeStream, Stream: &Stream{Stream: id, State: state}})
		}
	}
}

// repaint asks an attached stream's terminal for its screen again, and is
// whether there was one.
func (s *streams) repaint(id uint32) bool {
	s.mu.Lock()
	st := s.table[id]
	s.mu.Unlock()
	if st == nil {
		return false
	}
	st.mu.Lock()
	term := st.term
	st.mu.Unlock()
	if term == nil {
		return false
	}
	_ = term.Repaint()
	return true
}

// fail says a stream opened on tab could not be attached, and forgets it with
// what was kept for it; what that does to the workspace is the tab's
// (console.Tab.End): a primary that cannot be reached ends it.
func (s *streams) fail(id uint32, why error, tab console.Tab) {
	s.retire(id)
	s.send(Message{Type: TypeStream, Stream: &Stream{Stream: id, State: StreamFailed, Why: why.Error(), End: wireEnd(tab.End(why))}})
}

// close is the app closing a stream: an attached one's terminal is closed,
// and one still opening is closed as it attaches.
func (s *streams) close(id uint32) {
	st := s.get(id)
	if st == nil {
		return
	}
	st.mu.Lock()
	attached := st.term != nil
	st.closed = true
	st.early, st.earlyBytes = nil, 0
	st.mu.Unlock()
	if attached {
		s.end(id)
	}
}

// end closes an attached stream's terminal and forgets it.
func (s *streams) end(id uint32) {
	s.mu.Lock()
	st := s.table[id]
	delete(s.table, id)
	s.retired[id] = true
	s.mu.Unlock()
	if st == nil {
		return
	}
	st.mu.Lock()
	term, ended := st.term, st.ended
	st.ended = nil
	select {
	case <-st.done:
	default:
		close(st.done)
	}
	st.mu.Unlock()
	if term != nil {
		_ = term.Close()
	}
	if st.watch != nil {
		st.watch.end()
	}
	if ended != nil {
		ended()
	}
}

func (s *streams) retire(id uint32) {
	s.mu.Lock()
	delete(s.table, id)
	s.retired[id] = true
	s.mu.Unlock()
}

// closeAll closes every attached terminal, when the bridge ends.
func (s *streams) closeAll() {
	s.mu.Lock()
	ids := make([]uint32, 0, len(s.table))
	for id := range s.table {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.end(id)
	}
}
