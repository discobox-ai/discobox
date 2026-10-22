package ahphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/pkg/execstream/client"
)

// defaultCols and defaultRows size an attach no client has sized yet.
const (
	defaultCols = 120
	defaultRows = 30
)

// terminal is an AHP terminal over one exec's attach stream: a chat's screen,
// or a shell a client opened (ADR 26-10-17-354 §§3–4).
//
// A screen is attached while someone is subscribed to it and the discobox is
// running, and let go when nobody is: watching a discobox costs it nothing
// while nobody watches. A shell is attached from its creation until the
// client disposes of it.
type terminal struct {
	uri string
	s   *session
	// chat is a screen's chat, whose exec it shows; nil for a shell.
	chat *chat
	// execID is a shell's exec. A screen reads its chat's, which for the
	// primary is learned after the screen exists.
	execID string
	state  ahptypes.TerminalState
	subs   map[*conn]struct{}

	stream  Stream
	opening bool
	// gen counts the streams this terminal has had, so a reader still
	// draining one that was let go does not write into its successor.
	gen     int
	pending []byte
	cols    int
	rows    int
	// retryAt is when an attach that failed is tried again.
	retryAt time.Time
	// owner is the client whose createTerminal made a shell. Only its going
	// forgets the shell once it has exited.
	owner *conn
	// created is when the terminal was made, which an exec listing taken
	// before it does not reflect yet.
	created time.Time
	// noticed is whether the terminal has said its discobox is not running,
	// so it says so once per stop.
	noticed bool
}

func (t *terminal) shell() bool { return t.chat == nil }

func (t *terminal) exec() string {
	if t.chat != nil {
		return t.chat.execID
	}
	return t.execID
}

func (t *terminal) exited() bool {
	_, ok := t.state.Lifecycle.Value.(*ahptypes.TerminalExitedLifecycleState)
	return ok
}

// content is the terminal as a tool result: VS Code renders it as a terminal
// and subscribes to it to make it live.
func (t *terminal) content(title string) ahptypes.ToolResultContent {
	return ahptypes.ToolResultContent{Value: &ahptypes.ToolResultTerminalContent{
		Type: ahptypes.ToolResultContentTypeTerminal, Resource: t.uri, Title: title, IsPty: ptr(true),
	}}
}

func newTerminalState(title string, claim ahptypes.TerminalClaim) ahptypes.TerminalState {
	return ahptypes.TerminalState{
		Title:     title,
		Content:   []ahptypes.TerminalContentPart{},
		Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}},
		Claim:     claim,
		IsPty:     ptr(true),
	}
}

// newScreenLocked makes a chat a new screen. h.mu must be held.
func (h *Host) newScreenLocked(c *chat) *terminal {
	n := 0
	if c.screen != nil {
		n = c.screen.gen + 1
	}
	uri := fmt.Sprintf("%s%s/%s/%d", terminalScheme, c.s.key, chatKeyOf(c.uri), len(h.terminals)+n)
	for h.terminals[uri] != nil {
		uri += "x"
	}
	t := &terminal{
		uri: uri, s: c.s, chat: c, subs: map[*conn]struct{}{}, cols: defaultCols, rows: defaultRows, created: time.Now(),
		state: newTerminalState(c.state.Title, ahptypes.TerminalClaim{Value: &ahptypes.TerminalSessionClaim{
			Kind: ahptypes.TerminalClaimKindSession, Session: c.s.uri, Chat: c.uri,
		}}),
	}
	h.terminals[uri] = t
	return t
}

func chatKeyOf(uri string) string {
	for i := len(uri) - 1; i >= 0; i-- {
		if uri[i] == '/' {
			return uri[i+1:]
		}
	}
	return uri
}

// ensureAttachedLocked attaches a terminal that should be and is not: a screen
// someone is watching, of a discobox that is running, whose exec is known. A
// discobox that is not running is never started for it — observing never
// wakes (ADR 26-10-17-354 §5) — and the terminal says why it is still.
func (h *Host) ensureAttachedLocked(t *terminal) {
	if t.stream != nil || t.opening || t.exited() || !t.s.hasBox {
		return
	}
	if !t.shell() && len(t.subs) == 0 {
		return
	}
	if !t.s.running() {
		if !t.noticed {
			t.noticed = true
			h.dispatch(t.uri, ahptypes.StateAction{Value: &ahptypes.TerminalDataAction{
				Type: ahptypes.ActionTypeTerminalData, Data: fmt.Sprintf("\r\n\x1b[2m[discobox %s is %s]\x1b[0m\r\n", t.s.box.Name, t.s.box.State),
			}}, nil)
		}
		return
	}
	execID := t.exec()
	if execID == "" || time.Now().Before(t.retryAt) {
		return
	}
	t.opening = true
	gen := t.gen
	go h.attach(t, gen, t.s.box.ID, execID, t.cols, t.rows)
}

// openTimeout bounds how long opening a stream may take.
const openTimeout = time.Minute

// openContext is the context to open a stream under: the host's, which the
// stream lives as long as, cut short only while it is opening — by
// openTimeout, or by parent ending (the request that asked for it). opened
// says the open has returned and whether it succeeded; it answers false when
// the open lost a race with either deadline, which has already ended the
// stream. release ends the context: a stream's reader calls it when the
// stream ends, so a context does not outlive what it was opened for.
func (h *Host) openContext(parent context.Context) (ctx context.Context, opened func(ok bool) bool, release func()) {
	ctx, cancel := context.WithCancel(h.ctx)
	timer := time.AfterFunc(openTimeout, cancel)
	stop := context.AfterFunc(parent, cancel)
	return ctx, func(ok bool) bool {
		timed := !timer.Stop()
		parented := !stop()
		if !ok || timed || parented {
			cancel()
			return false
		}
		return true
	}, cancel
}

func (h *Host) attach(t *terminal, gen int, sandboxID, execID string, cols, rows int) {
	ctx, opened, release := h.openContext(h.ctx)
	stream, err := h.cfg.Boxes.OpenExec(ctx, sandboxID, execID, cols, rows)
	if err == nil && !opened(true) {
		_ = stream.Close()
		err = fmt.Errorf("the attach did not open within %s", openTimeout)
	} else if err != nil {
		opened(false)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.opening = false
	if err != nil {
		h.cfg.Logf("ahp: attach %s/%s: %v", sandboxID, execID, err)
		t.retryAt = time.Now().Add(10 * time.Second)
		return
	}
	if t.gen != gen || h.terminals[t.uri] != t || (!t.shell() && len(t.subs) == 0) {
		go func() { _ = stream.Close(); release() }()
		return
	}
	h.adoptLocked(t, stream, release)
}

// adoptLocked makes stream the terminal's. The attach replays the screen, so
// what the terminal held before is cleared rather than drawn twice.
func (h *Host) adoptLocked(t *terminal, stream Stream, release func()) {
	t.gen++
	t.stream = stream
	t.pending = nil
	t.noticed = false
	if len(t.state.Content) > 0 {
		h.dispatch(t.uri, ahptypes.StateAction{Value: &ahptypes.TerminalClearedAction{Type: ahptypes.ActionTypeTerminalCleared}}, nil)
	}
	go h.read(t, stream, t.gen, release)
}

// read sends a stream's output to the terminal's subscribers until it ends.
func (h *Host) read(t *terminal, stream Stream, gen int, release func()) {
	defer func() {
		_ = stream.Close()
		release()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			h.mu.Lock()
			if t.gen != gen {
				h.mu.Unlock()
				return
			}
			var data string
			data, t.pending = utf8Prefix(append(t.pending, buf[:n]...))
			if data != "" {
				h.dispatch(t.uri, ahptypes.StateAction{Value: &ahptypes.TerminalDataAction{Type: ahptypes.ActionTypeTerminalData, Data: data}}, nil)
			}
			h.mu.Unlock()
		}
		if err != nil {
			h.ended(t, gen, err)
			return
		}
	}
}

// ended is a stream that stopped with err. The attach ends the way a plain
// attach does when its exec exits — io.EOF for a clean exit, an ExitError
// carrying any other code — and that makes the terminal exited for good. An
// attach lost to anything else, a server that went away, is attached again
// once it can be.
func (h *Host) ended(t *terminal, gen int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t.gen != gen {
		return
	}
	t.stream = nil
	t.pending = nil
	var code int64
	var exit client.ExitError
	switch {
	case errors.Is(err, io.EOF):
	case errors.As(err, &exit):
		code = int64(exit.Code)
	default:
		h.cfg.Logf("ahp: attach to %s ended: %v", t.uri, err)
		t.retryAt = time.Now().Add(h.cfg.Poll)
		return
	}
	h.dispatch(t.uri, ahptypes.StateAction{Value: &ahptypes.TerminalExitedAction{Type: ahptypes.ActionTypeTerminalExited, ExitCode: &code}}, nil)
	if t.shell() {
		h.terminalsChangedLocked()
	}
}

// releaseLocked lets go of a screen nobody is watching.
func (h *Host) releaseLocked(t *terminal) {
	if !t.shell() && len(t.subs) == 0 {
		h.closeStreamLocked(t)
	}
}

func (h *Host) closeStreamLocked(t *terminal) {
	if t.stream == nil {
		return
	}
	t.gen++
	stream := t.stream
	t.stream = nil
	go func() { _ = stream.Close() }()
}

// handleTerminalAction is a client's action on a terminal; h.mu is not held.
// Typing into one whose discobox is stopped starts it (ADR 26-10-17-354 §5);
// what was typed meanwhile is not kept, since nothing is there to read it.
// Input is written here, on the client's own reader, so keystrokes reach the
// discobox in the order they were typed.
func (h *Host) handleTerminalAction(t *terminal, act ahptypes.StateAction, origin *ahptypes.ActionOrigin) {
	if a, ok := act.Value.(*ahptypes.TerminalInputAction); ok {
		h.mu.Lock()
		stream := t.stream
		stopped := t.s.hasBox && !t.s.running() && t.s.box.State != BoxArchived
		h.mu.Unlock()
		switch {
		case stream != nil:
			if _, err := stream.Write([]byte(a.Data)); err != nil {
				h.cfg.Logf("ahp: type into %s: %v", t.uri, err)
			}
		case stopped:
			go func() {
				if err := h.start(h.ctx, t.s); err != nil {
					h.cfg.Logf("ahp: start %s: %v", t.s.key, err)
				}
			}()
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dispatch(t.uri, act, origin)
	switch a := act.Value.(type) {
	case *ahptypes.TerminalResizedAction:
		t.cols, t.rows = int(a.Cols), int(a.Rows)
		if stream := t.stream; stream != nil {
			cols, rows := t.cols, t.rows
			go func() { _ = stream.Resize(cols, rows) }()
		}
	default:
		if t.shell() {
			h.terminalsChangedLocked()
		}
	}
}

// terminalsChangedLocked publishes the root's terminal catalog: the shells
// clients have open through this host. A screen is reached through its chat
// and is not listed.
func (h *Host) terminalsChangedLocked() {
	infos := []ahptypes.TerminalInfo{}
	for _, t := range h.terminals {
		if !t.shell() {
			continue
		}
		infos = append(infos, ahptypes.TerminalInfo{Resource: t.uri, Title: t.state.Title, Claim: t.state.Claim, Lifecycle: t.state.Lifecycle})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Resource < infos[j].Resource })
	h.dispatch(ahptypes.RootResourceURI, ahptypes.StateAction{Value: &ahptypes.RootTerminalsChangedAction{
		Type: ahptypes.ActionTypeRootTerminalsChanged, Terminals: infos,
	}}, nil)
}
