package bridge

import (
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// What a box's agent is doing, as the workspace header says it (ADR
// 26-10-16-424): what the program in its primary terminal reports of itself
// over OSC 7501, read off the stream the bridge relays, or the listing's poll,
// whichever the core's rule says (console.Sandbox.AgentHeard), the choice the
// TUI makes for what it says on its terminal's tab and title. It goes to the app in the box's header (console.Header.Agent),
// which is read again whenever what the primary says changes; the app draws
// it and decides nothing (289 §2). Its emulator never sees a sequence it does not
// know, so this is the one place the desktop's stream is read for it.
//
// The sequences are read by the parser every other reader of this protocol
// goes through (charmbracelet/x/ansi, under termpane and the shim), and taken
// by the protocol's own rules (programstatus.Set.Observe).

// heard is what a box's primary stream has said of its program since the app
// attached it.
type heard struct {
	// stream is the primary stream attached last: an earlier one's late
	// output or end is not this one's.
	stream uint32
	set    programstatus.Set
	ended  bool
	// changed is closed when what the primary has said changes, and replaced:
	// what a header following the box waits on.
	changed chan struct{}
}

// programWatch reads one attached primary stream's output for its program's
// reports and its shell's prompts.
type programWatch struct {
	s      *Server
	box    string
	stream uint32
	parser *ansi.Parser
}

// watchProgram starts reading box's primary stream, which the app has just
// attached: what an earlier attach heard is forgotten, as a TUI pane's is.
func (s *Server) watchProgram(box string, stream uint32) *programWatch {
	w := &programWatch{s: s, box: box, stream: stream, parser: ansi.NewParser()}
	w.parser.SetHandler(ansi.Handler{HandleOsc: func(cmd int, data []byte) {
		if cmd != programstatus.Code && cmd != programstatus.PromptCode {
			return
		}
		w.hear(func(h *heard) bool { return h.set.Observe(data, time.Now().UTC(), 0) != programstatus.SeenNothing })
	}})
	s.hear(box, func(h *heard) bool {
		h.stream, h.set, h.ended = stream, programstatus.Set{}, false
		return true
	})
	return w
}

// hear changes what the box's primary has said, if this is still its
// primary stream.
func (w *programWatch) hear(change func(*heard) bool) {
	w.s.hear(w.box, func(h *heard) bool { return h.stream == w.stream && change(h) })
}

// feed reads one read of the stream's output.
func (w *programWatch) feed(out []byte) {
	for _, b := range out {
		w.parser.Advance(b)
	}
}

// end is the stream ending, or detached: the poll speaks for the box again.
func (w *programWatch) end() {
	w.hear(func(h *heard) bool {
		h.ended = true
		return true
	})
}

// hear changes what box's primary has said, and wakes the headers following
// the box when change says it changed anything. What is kept is one small
// record a box whose primary the app attached, for as long as the bridge runs.
func (s *Server) hear(box string, change func(*heard) bool) {
	s.heardMu.Lock()
	defer s.heardMu.Unlock()
	h := s.heardOf(box)
	if change(h) {
		close(h.changed)
		h.changed = make(chan struct{})
	}
}

// heardOf is box's record, made when there is none. heardMu is held.
func (s *Server) heardOf(box string) *heard {
	h := s.heard[box]
	if h == nil {
		h = &heard{changed: make(chan struct{})}
		s.heard[box] = h
	}
	return h
}

// header is box's header as the core says it (console.NewHeader), its agent
// from what the box's primary has said, and what is closed when that
// changes. The record is read under heardMu: a primary's output goes on
// changing it.
func (s *Server) header(key string, box console.Sandbox, forwarded map[console.PortKey]int) (console.Header, <-chan struct{}) {
	s.heardMu.Lock()
	defer s.heardMu.Unlock()
	h := s.heardOf(key)
	return console.NewHeader(box, forwarded, &h.set, h.ended), h.changed
}

// primaryOf is whether an exec a stream attaches is its box's primary, the
// one terminal whose program speaks for the box.
func primaryOf(exec string) bool { return exec == console.ExecPrimary }
