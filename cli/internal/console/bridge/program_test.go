package bridge

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// header follows b1's workspace header, as the app does when it shows the
// box, and is its first snapshot.
func header(t *testing.T, a *app) *Header {
	t.Helper()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 9, Topic: TopicHeader, Box: "b1"}})
	return nextHeader(a)
}

// nextHeader is the header's next snapshot.
func nextHeader(a *app) *Header {
	for {
		if snap := a.expect(TypeSnapshot).Snapshot; snap.Sub == 9 {
			return snap.Header
		}
	}
}

// agentOf is what a header says the box's agent is doing.
func agentOf(h *Header) string { return h.Agent + " " + h.AgentMsg }

// A box's header says what the core's rule chooses: the poll until its
// attached primary reports, the report while it is newer, read whichever way
// the output is cut and with either terminator, nothing once a prompt drops
// it, and the poll again once the session ends. A report said again is not a
// new header.
func TestThePrimarysHeaderIsTheCoresChoice(t *testing.T) {
	b1 := running("b1")
	b1.Agent = programstatus.Status{State: programstatus.Working, Msg: "polled"}
	b1.AgentObservedAt = time.Now().Add(-time.Hour)
	core := newFakeCore("home", b1)
	core.forward = newFakeForward()
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)
	if got := agentOf(header(t, a)); got != "working polled" {
		t.Fatalf("before an attach: %q", got)
	}
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 3, Box: "b1", Exec: console.ExecPrimary, Cols: 80, Rows: 24}})
	if s := a.expect(TypeStream).Stream; s.State != StreamAttached {
		t.Fatalf("stream %+v", s)
	}
	term := core.opening(console.ExecPrimary)
	report := "out\x1b]7501;state=blocked:kind=question:msg=" + b64("which branch?") + "\x1b\\more"
	for _, cut := range []int{1, 7, 20, len(report) - 6} {
		term.out <- []byte(report[:cut])
		term.out <- []byte(report[cut:])
		if got := agentOf(nextHeader(a)); got != "blocked which branch?" {
			t.Fatalf("cut at %d: %q", cut, got)
		}
		term.out <- []byte("\x1b]133;A\x07$ ")
		if got := agentOf(nextHeader(a)); got != " " {
			t.Fatalf("after the prompt: %q", got)
		}
	}
	term.out <- []byte("\x1b]7501;state=working\x07")
	term.out <- []byte("\x1b]7501;state=working\x07")
	if got := agentOf(nextHeader(a)); got != "working " {
		t.Fatalf("a report: %q", got)
	}
	close(term.out)
	if got := agentOf(nextHeader(a)); got != "working polled" {
		t.Fatalf("after the session ended: %q", got)
	}
}

// A shell's stream is read for nothing: only a box's primary speaks for it.
func TestOnlyThePrimarySpeaksForTheBox(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.forward = newFakeForward()
	a := dial(t, core)
	a.hello()
	header(t, a)
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 3, Box: "b1", Exec: "sh1", Cols: 80, Rows: 24}})
	if s := a.expect(TypeStream).Stream; s.State != StreamAttached {
		t.Fatalf("stream %+v", s)
	}
	core.opening("sh1").out <- []byte("\x1b]7501;state=working\x07")
	a.quiet(200 * time.Millisecond)
}

// A primary attached again speaks for the box: the stream it replaced says
// nothing more, its late output and its end included.
func TestAnEarlierPrimaryNoLongerSpeaks(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.forward = newFakeForward()
	a := dial(t, core)
	a.hello()
	header(t, a)
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 3, Box: "b1", Exec: console.ExecPrimary, Cols: 80, Rows: 24}})
	a.expect(TypeStream)
	first := core.opening(console.ExecPrimary)
	first.out <- []byte("\x1b]7501;state=working\x07")
	if got := agentOf(nextHeader(a)); got != "working " {
		t.Fatalf("first: %q", got)
	}
	a.send(Message{Type: TypeOpen, Open: &Open{Stream: 5, Box: "b1", Exec: console.ExecPrimary, Cols: 80, Rows: 24}})
	if got := agentOf(nextHeader(a)); got != " " {
		t.Fatalf("the second attach starts afresh: %q", got)
	}
	// The header was reset once the open had made the stream's terminal.
	second := core.opening(console.ExecPrimary)
	second.out <- []byte("\x1b]7501;state=blocked\x07")
	if got := agentOf(nextHeader(a)); got != "blocked " {
		t.Fatalf("second: %q", got)
	}
	first.out <- []byte("\x1b]7501;state=done\x07")
	close(first.out)
	// Its end is said as a stream's, and nothing more.
	if s := a.expect(TypeStream).Stream; s.Stream != 3 || s.State != StreamEnded {
		t.Fatalf("stream %+v", s)
	}
	a.quiet(300 * time.Millisecond)
}
