// Package termpane draws a live terminal inside a Bubble Tea application.
//
// It is the multiplexer half of a terminal multiplexer and nothing else: give
// it a [Stream] — anything with a terminal on the other end that can be read,
// written and resized — and it maintains a virtual screen from the bytes that
// come back, encodes key presses on the way in, and renders the screen as
// lines you can put anywhere in your own layout.
//
// It knows nothing about where the terminal is. A local PTY, an SSH session and
// a websocket to a container are all the same Stream, which is the difference
// between this and a multiplexer built around spawning local shells.
//
// # Drawing it
//
// [Model.View] returns exactly the rows and columns it was sized to, already
// padded and truncated to the cell. Do not pass them through anything that
// wraps or re-styles whole lines: a terminal grid that gets re-wrapped shifts
// every row below the wrap and desyncs the hardware cursor from the screen the
// application thinks it is drawing on. Draw your own frame around the rows, and
// place the cursor with [Model.Cursor] at the origin you drew them at.
//
// # Owning the keyboard
//
// While a pane is focused, essentially every key belongs to the application on
// the other end — including the ones your own UI would like to use. Either
// reserve a prefix with [WithPrefix], which is what screen and tmux do and what
// this package implements for you, or intercept keys before delegating and send
// the rest with [Model.SendKey].
package termpane

import (
	"image/color"
	"io"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/discobox-ai/discobox/termpane/selection"
)

// Stream is a terminal to draw: its output to read, its input to write, and a
// way to tell the far end that the window it is drawing into changed size.
//
// Read is expected to block until there is output, and to return an error when
// the session ends. Close must unblock a pending Read.
type Stream interface {
	io.ReadWriteCloser

	// Resize tells the far end the terminal is now cols by rows cells.
	Resize(cols, rows int) error
}

// Model is one terminal pane. The zero value is not usable; build one with
// [New].
type Model struct {
	opts options

	emu    *vt.Emulator
	stream Stream
	reader *reader

	// The input forwarder's two halves of a handshake: done asks it to stop,
	// and exited says it has. See detach for why it is not simply killed.
	forwardDone chan struct{}
	forwardOut  chan struct{}

	cols, rows int

	attached bool
	err      error

	// prefixArmed is set between the prefix key and the key it qualifies.
	prefixArmed bool
	// prefixRepeat is set when the armed prefix was held open by a repeating
	// binding: the pane is mid-run, and only the repeating keys — still under
	// Ctrl — mean anything. Any other key ends the run and is the
	// application's, rather than consulting the whole binding table the way a
	// freshly pressed prefix does.
	prefixRepeat bool

	// scroll is how many lines back through the scrollback the view is, zero
	// being the live screen. See Scroll.
	scroll int

	// Selection state; see select.go. sel exists while attached, seized is
	// the host's mouse override, and selShot/selAlt are what the selection
	// read when it was made — the reference reconcileSelection clears against.
	sel     *selection.Model
	seized  bool
	selShot string
	selAlt  bool

	// Written by the emulator's callbacks, which run on whichever goroutine is
	// feeding it, and read when a frame is drawn.
	mu            sync.Mutex
	title         string
	altScreen     bool
	cursorVisible bool
	cursorStyle   tea.CursorShape
	cursorBlink   bool
	cursorColor   color.Color
	bells         int
	mouseModes    map[int]bool
	// clip is an OSC 52 copy the application made, waiting for the update loop
	// to turn it into a CopyMsg; hasClip distinguishes a copy of nothing from
	// no copy. See clipboard.go.
	clip    string
	hasClip bool
}

type options struct {
	scrollback int
	readOnly   bool
	prefix     string
	detach     string
	bindings   map[string]prefixBinding
	highlight  func(uv.Style) uv.Style
	wheelLines int
	// wordChars is nil for the selection package's default; see WithWordChars.
	wordChars *string
}

// prefixBinding is a key reserved behind the prefix and what it emits.
type prefixBinding struct {
	msg tea.Msg
	// repeat keeps the prefix open after this binding fires, as long as the key
	// that fired it was held with Ctrl. See WithRepeatingPrefixBinding.
	repeat bool
}

// Option configures a pane at construction.
type Option func(*options)

// WithScrollback sets how many lines scrolled off the top are retained. The
// default is the emulator's own, which is 10,000 lines.
func WithScrollback(lines int) Option {
	return func(o *options) { o.scrollback = lines }
}

// WithReadOnly draws a stream nothing types at: keys, text and pastes are not
// forwarded, and a resize is applied to the emulator without being sent on.
//
// It is for a far end with no input side to reach — a process running on pipes
// rather than a PTY, whose output is watched rather than driven. Everything
// else about the pane is unchanged: it draws, it scrolls, and text in it can
// still be selected and copied.
//
// The emulator's own replies to the queries applications make are still
// collected and sent. They are not input in this sense, and an emulator whose
// replies go nowhere wedges on the first question it is asked.
func WithReadOnly() Option {
	return func(o *options) { o.readOnly = true }
}

// WithPrefix reserves the pane's two keys: the detach key, which emits
// [DetachMsg] instead of reaching the application, and a prefix that types
// either of them literally when the application needs one.
//
// That is screen's and tmux's arrangement, with the detach key promoted to a
// press of its own:
//
//   - detach              → [DetachMsg]
//   - prefix then detach  → the detach key, sent to the application
//   - prefix then prefix  → the prefix key, sent to the application
//   - prefix then any key → the prefix and then that key, so a mistyped prefix
//     costs nothing
//
// A key typed after the prefix matches whether or not Ctrl was still held — see
// [afterPrefix].
//
// A detach key the application would otherwise want — Ctrl-C is the obvious one
// — is exactly why the prefix is there. Both are Bubble Tea key names, as
// [tea.KeyPressMsg.String] reports them: "ctrl+a", "ctrl+c", "d".
func WithPrefix(prefix, detach string) Option {
	return func(o *options) { o.prefix, o.detach = prefix, detach }
}

// WithPrefixBinding reserves one more key behind the prefix, emitting msg
// instead of reaching the application.
//
// It is for the decisions that are the host's rather than the pane's — whether
// to pass the mouse through, what to do about a bell — which still need a key
// press the application never sees. Reserving them behind the prefix rather
// than on their own is what keeps them out of the application's way.
func WithPrefixBinding(key string, msg tea.Msg) Option {
	return bindPrefix(key, prefixBinding{msg: msg})
}

// WithRepeatingPrefixBinding is a binding that holds the prefix open while Ctrl
// is held, so the prefix followed by Ctrl-<key> Ctrl-<key> keeps firing without
// pressing the prefix again.
//
// It is for the bindings you use in runs — moving between panes, resizing them —
// where letting go of Ctrl is what says you are done. tmux spells this as a
// repeat flag on a timer; keying it to the modifier instead means a sequence
// ends when you stop holding it rather than when a clock runs out, which is
// both quicker and never ambiguous.
//
// While the run is open, only the repeating keys — still under Ctrl — are
// taken. Any other key ends the run and goes to the application, bound letters
// included: the prefix was held open for the run, not re-pressed, so the key
// after it is the first keystroke of whatever you type next rather than a
// second command.
func WithRepeatingPrefixBinding(key string, msg tea.Msg) Option {
	return bindPrefix(key, prefixBinding{msg: msg, repeat: true})
}

func bindPrefix(key string, binding prefixBinding) Option {
	return func(o *options) {
		if o.bindings == nil {
			o.bindings = map[string]prefixBinding{}
		}
		o.bindings[key] = binding
	}
}

// New builds a pane. It is not drawing anything until [Model.Attach] is given a
// stream, and it draws nothing at all until it has been sized.
func New(opts ...Option) *Model {
	m := &Model{cursorVisible: true}
	for _, opt := range opts {
		opt(&m.opts)
	}
	return m
}

// SetSize sets the pane's size in cells. It is the size of the terminal the
// application on the other end believes it has, so it is the content area you
// intend to draw — not counting any frame you put around it.
//
// Resizing an attached pane resizes the emulator and tells the far end. A
// failure to tell the far end is dropped: the size will be corrected by the
// next resize, and a stream that is really gone reports it through Read.
func (m *Model) SetSize(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	if cols == m.cols && rows == m.rows {
		return
	}
	m.cols, m.rows = cols, rows
	// Columns mean nothing across a resize — the emulator does not reflow —
	// so whatever was selected no longer names the same text.
	m.clearSelection()
	if m.emu != nil {
		m.emu.Resize(cols, rows)
	}
	// A read-only pane resizes its emulator and stops there: the far end has
	// no terminal whose size could be wrong.
	if m.stream != nil && !m.opts.readOnly {
		_ = m.stream.Resize(cols, rows)
	}
}

// Size is the pane's current size in cells.
func (m *Model) Size() (cols, rows int) { return m.cols, m.rows }

// Attach starts drawing a stream, and returns the command that pumps it.
//
// The pane must be sized first: the size is what the far end is told, and a
// terminal opened at the wrong size draws itself wrong before anything can
// correct it.
func (m *Model) Attach(stream Stream) tea.Cmd {
	if m.cols == 0 || m.rows == 0 {
		m.cols, m.rows = 80, 24
	}
	// Whatever was attached before is closed with it; a pane draws one terminal.
	// A failure closing the old one is not this attach's problem to report.
	_ = m.detach()

	m.emu = vt.NewEmulator(m.cols, m.rows)
	if m.opts.scrollback > 0 {
		m.emu.SetScrollbackSize(m.opts.scrollback)
	}
	m.emu.SetCallbacks(m.callbacks())
	m.watchMouseModes()
	m.watchClipboard()
	m.emu.Focus()
	m.sel = selection.New(emuGrid{m})
	if m.opts.wordChars != nil {
		m.sel.SetWordChars(*m.opts.wordChars)
	}
	m.selShot = ""

	m.stream = stream
	m.reader = newReader(stream)
	m.attached, m.err = true, nil
	m.mu.Lock()
	m.mouseModes = map[int]bool{}
	// The title belongs to the stream, not to the pane: a new one announces
	// its own, and a far end with none should not be wearing the last one's.
	m.title = ""
	// A copy the last stream made and never had delivered dies with it.
	m.clip, m.hasClip = "", false
	m.mu.Unlock()
	if !m.opts.readOnly {
		_ = stream.Resize(m.cols, m.rows)
	}

	// Everything the emulator produces on its input side goes to the far end:
	// the keys and text sent below, and — the part that is easy to miss — the
	// automatic replies to the queries applications make about the terminal
	// they are running in. An emulator whose replies are never collected fills
	// its buffer and wedges the moment something asks it a question.
	m.forwardDone, m.forwardOut = make(chan struct{}), make(chan struct{})
	startForwarding(m.emu, stream, m.forwardDone, m.forwardOut)

	return m.reader.next()
}

// callbacks keep the state the pane draws chrome and a cursor from. They run on
// the goroutine writing to the emulator, which is not the one rendering, so
// everything they touch is behind the mutex.
func (m *Model) callbacks() vt.Callbacks {
	return vt.Callbacks{
		Title: func(title string) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.title = title
		},
		AltScreen: func(alt bool) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.altScreen = alt
		},
		CursorVisibility: func(visible bool) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.cursorVisible = visible
		},
		CursorStyle: func(style vt.CursorStyle, blink bool) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.cursorStyle, m.cursorBlink = cursorShape(style), blink
		},
		CursorColor: func(c color.Color) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.cursorColor = c
		},
		Bell: func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.bells++
		},
	}
}

func cursorShape(style vt.CursorStyle) tea.CursorShape {
	switch style {
	case vt.CursorUnderline:
		return tea.CursorUnderline
	case vt.CursorBar:
		return tea.CursorBar
	default:
		return tea.CursorBlock
	}
}

// inputChunk is the most input taken off the emulator at once.
const inputChunk = 32 * 1024

// inputBacklog is how many chunks may be waiting for the far end before the
// pane pushes back on whoever is producing them. Two megabytes is far more than
// typing produces and more than any paste anyone means, so in practice the
// backlog absorbs the whole of what is sent and the host never waits; past it,
// waiting is the only honest answer, since the alternative is either dropping
// input or growing without bound.
const inputBacklog = 64

// startForwarding connects the emulator's input side to the stream.
//
// It is two goroutines rather than one because a write to the far end can take
// arbitrarily long — a stalled pty, a reconnecting transport — and the pipe
// behind the emulator is synchronous: whoever last called Paste or SendKey is
// held until the bytes are read. That caller is the host's update goroutine, so
// a slow stream reached directly from the drain would freeze the window for as
// long as the far end took. The drain keeps the pipe moving; the writer waits.
func startForwarding(emu *vt.Emulator, stream Stream, done, exited chan struct{}) {
	backlog := make(chan []byte, inputBacklog)
	dead := make(chan struct{})
	go writeInput(stream, backlog, dead, done)
	go drainInput(emu, backlog, dead, done, exited)
}

// drainInput copies the emulator's input side into the backlog.
//
// It is the pipe's only reader, and it goes on reading for as long as the pane
// is attached — including after the far end is gone, when what it reads is
// dropped. A pipe with no reader blocks its writers forever, and its writers are
// Paste, SendKey and the emulator's own replies to the far end's queries: every
// one of them runs on the host's update goroutine. A drain that gave up on a
// dead stream would take the window down with the pane.
func drainInput(emu *vt.Emulator, backlog chan<- []byte, dead, done <-chan struct{}, exited chan<- struct{}) {
	defer close(exited)
	buf := make([]byte, inputChunk)
	for {
		n, err := emu.Read(buf)
		// A read that returned because detach woke it carries the wake-up byte
		// and nothing worth sending, so it is checked before the write rather
		// than after: the far end must not be handed a byte nobody typed.
		select {
		case <-done:
			return
		default:
		}
		if n > 0 {
			// The buffer is reused, so the writer is handed its own copy.
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			// Once the far end is dead the backlog fills and stays full, at
			// which point this only ever takes the dead case: reading and
			// dropping, which is the point.
			select {
			case backlog <- chunk:
			case <-dead:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// writeInput sends the backlog on to the far end. A failed write ends it —
// there is nothing left to send input to — and closing dead tells the drain to
// stop queueing for it. That the session is over is the reader's to report; a
// pane does not learn it twice.
func writeInput(stream Stream, backlog <-chan []byte, dead chan<- struct{}, done <-chan struct{}) {
	defer close(dead)
	for {
		select {
		case chunk := <-backlog:
			if _, err := stream.Write(chunk); err != nil {
				return
			}
		case <-done:
			return
		}
	}
}

// Update feeds the pane. Delegate every message to it while it is focused: the
// output it is drawing arrives as messages of this package's own, and dropping
// them stops the pane dead.
func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	switch msg := msg.(type) {
	case outputMsg:
		if m.emu != nil {
			_, _ = m.emu.Write(msg.data)
		}
		// A selection whose text this output overwrote is cleared before it
		// can highlight something nobody selected.
		m.reconcileSelection()
		// New output pins the view back to the live screen. A pane scrolled
		// away from what is happening in it, with no way to notice, is a pane
		// that looks hung.
		m.scroll = 0
		// Output is also where an application's own copy arrives; the write
		// above is what parsed it. See clipboard.go.
		return m, tea.Batch(m.takeClipboard(), m.reader.next())

	case closedMsg:
		m.attached = false
		m.err = msg.err
		return m, func() tea.Msg { return ClosedMsg{Err: msg.err} }

	case tea.PasteMsg:
		// Bracketed paste from the host terminal. The emulator wraps it in the
		// application's own paste markers when it has asked for them, and sends
		// it plain when it has not.
		if m.emu != nil && !m.opts.readOnly {
			m.emu.Paste(stripPasteMarkers(msg.Content))
		}
		return m, nil

	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

// pasteMarkers begin and end a bracketed paste, in the 7-bit form everything
// writes and the 8-bit form some far ends still parse.
var pasteMarkers = []string{"\x1b[200~", "\x1b[201~", "\x9b200~", "\x9b201~"}

// stripPasteMarkers takes the paste markers out of text about to be pasted.
//
// The end marker is the one that matters. Text carrying it closes the paste
// early on the far end, and everything after it arrives as though it had been
// typed: a clipboard ending "\x1b[201~rm -rf /\r" would run. Filtering it is
// what every terminal emulator does, and a pane is one. The start marker goes
// with it, a paste that appears to begin twice being nothing the far end asked
// for.
//
// Nothing else is touched. A paste is the user's own text, and a terminal that
// second-guesses it is a terminal that mangles what people paste.
func stripPasteMarkers(text string) string {
	for _, marker := range pasteMarkers {
		text = strings.ReplaceAll(text, marker, "")
	}
	return text
}

// handleKey applies the reserved prefix, if there is one, and sends the rest.
func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	if m.emu == nil {
		return nil
	}
	if cmd, taken := m.copyChord(msg); taken {
		return cmd
	}
	name := msg.String()

	if m.prefixArmed {
		inRun := m.prefixRepeat
		m.prefixArmed, m.prefixRepeat = false, false
		if inRun {
			// Mid-run the prefix is open only as a courtesy to the run: the
			// repeating keys, still under Ctrl, continue it. Anything else —
			// a bound letter included — ends the run and belongs to the
			// application, or a run of moves followed by a word would fire a
			// binding per letter.
			for key, bound := range m.opts.bindings {
				if bound.repeat && msg.Mod&tea.ModCtrl != 0 && afterPrefix(name, key) {
					m.prefixArmed, m.prefixRepeat = true, true
					return func() tea.Msg { return bound.msg }
				}
			}
			m.SendKey(msg)
			return nil
		}
		switch {
		case name == m.opts.prefix:
			// The prefix twice is how you type one. This one is matched
			// exactly, where everything after the prefix tolerates a held
			// Ctrl: the bare form is a letter a binding can have, and with a
			// leader like Ctrl-A that letter is one a host will want.
			m.SendKey(prefixKey(m.opts.prefix))
			return nil
		case afterPrefix(name, m.opts.detach):
			// The detach key typed after the prefix is that key, sent on: it is
			// how the application gets the one keystroke the pane has taken —
			// its own interrupt, most of the time.
			m.SendKey(prefixKey(m.opts.detach))
			return nil
		}
		for key, bound := range m.opts.bindings {
			if !afterPrefix(name, key) {
				continue
			}
			// A repeating binding leaves the prefix armed while Ctrl is still
			// down, so a run of them is one chord rather than one per step.
			if bound.repeat && msg.Mod&tea.ModCtrl != 0 {
				m.prefixArmed, m.prefixRepeat = true, true
			}
			return func() tea.Msg { return bound.msg }
		}
		// A prefix that qualified nothing was a keystroke like any other, so it
		// is delivered along with the key that followed it.
		m.SendKey(prefixKey(m.opts.prefix))
		m.SendKey(msg)
		return nil
	}
	if m.opts.detach != "" && name == m.opts.detach {
		return func() tea.Msg { return DetachMsg{} }
	}
	if m.opts.prefix != "" && name == m.opts.prefix {
		m.prefixArmed = true
		return nil
	}

	m.SendKey(msg)
	return nil
}

// afterPrefix matches a key typed after the prefix, ignoring whether Ctrl was
// still held.
//
// Holding Ctrl through the pair is what everyone does — the prefix is a Ctrl
// chord, and letting go precisely between the two keystrokes is a skill nobody
// should have to acquire. GNU screen has bound both forms of most of its
// commands for decades for exactly this reason; tmux binds only the bare form
// and the control variant silently does nothing.
//
// It applies only after the prefix. The detach key on its own is matched
// exactly, because there the bare letter is one you type constantly.
//
// The cost is that the un-controlled forms of the reserved keys are spoken for:
// with Ctrl-C as detach, prefix then "c" sends an interrupt rather than the
// letter. That is screen's bargain too, and prefix-then-a-plain-letter is not
// something anyone types on purpose.
func afterPrefix(name, want string) bool {
	if want == "" {
		return false
	}
	return name == want ||
		strings.TrimPrefix(name, "ctrl+") == strings.TrimPrefix(want, "ctrl+")
}

// prefixKey rebuilds a reserved key as a key press, for the cases where it has
// to be delivered after all: the one that armed the prefix was swallowed, and
// the one that followed it may have been typed in either form.
func prefixKey(prefix string) tea.KeyPressMsg {
	key := tea.Key{}
	if code, mod, ok := parseKeyName(prefix); ok {
		key.Code, key.Mod = code, mod
		if mod == 0 {
			key.Text = string(code)
		}
	}
	return tea.KeyPressMsg(key)
}

// parseKeyName understands the key names this package's prefix is spelled with:
// a bare character, or ctrl+ and alt+ in front of one.
func parseKeyName(name string) (rune, tea.KeyMod, bool) {
	var mod tea.KeyMod
	for {
		switch {
		case strings.HasPrefix(name, "ctrl+"):
			mod |= tea.ModCtrl
			name = strings.TrimPrefix(name, "ctrl+")
		case strings.HasPrefix(name, "alt+"):
			mod |= tea.ModAlt
			name = strings.TrimPrefix(name, "alt+")
		default:
			runes := []rune(name)
			if len(runes) != 1 {
				return 0, 0, false
			}
			return runes[0], mod, true
		}
	}
}

// PrefixArmed reports whether the prefix has been pressed and is waiting for the
// key it qualifies.
//
// It is exported for hosts that move focus between panes: a repeating binding
// leaves the pane it fired in armed, and a host that moves focus on one has to
// carry the sequence to the pane it moved to, or the run stops the moment it
// does anything.
func (m *Model) PrefixArmed() bool { return m.prefixArmed }

// SetPrefixArmed opens or closes the prefix sequence. Opening it opens the
// run form — only the repeating keys, still under Ctrl, are taken; anything
// else goes to the application — because carrying a mid-run sequence to the
// pane focus moved to is what the setter exists for, and a run must mean the
// same thing on the pane it lands on as on the one it left.
func (m *Model) SetPrefixArmed(armed bool) { m.prefixArmed, m.prefixRepeat = armed, armed }

// SendKey forwards one key press to the terminal, encoded the way the
// application on the other end has asked for it — cursor-key mode, keypad mode
// and alt prefixing are all the emulator's business rather than the caller's.
//
// Printable text with no ctrl or alt is sent as text rather than as a key,
// because the key encoder works from the unshifted code: routed as a key, an
// uppercase letter arrives lowercase and "!" arrives as "1".
func (m *Model) SendKey(msg tea.KeyPressMsg) {
	if m.emu == nil || m.opts.readOnly {
		return
	}
	key := unshiftBackspace(msg.Key())
	if key.Text != "" && key.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
		m.emu.SendText(key.Text)
		return
	}
	// The emulator drops the modified special keys; see keys.go.
	if seq := modifiedKeySeq(key); seq != "" {
		m.emu.SendText(seq)
		return
	}
	m.emu.SendKey(uv.KeyPressEvent(uv.Key(key)))
}

// SendText writes text to the terminal as though it had been typed.
func (m *Model) SendText(text string) {
	if m.emu != nil && !m.opts.readOnly {
		m.emu.SendText(text)
	}
}

// View renders the screen as exactly the rows the pane was sized to, each
// exactly as wide as it was sized to. See the package comment on what not to do
// with them.
//
// Scrolled back, the rows above the screen come from the scrollback, so what is
// drawn is one continuous run of history however far up it starts.
func (m *Model) View() []string {
	out := make([]string, m.rows)
	var lines []string
	if m.emu != nil {
		lines = strings.Split(m.emu.Render(), "\n")
	}
	selected := m.emu != nil && m.sel != nil && m.sel.Active()
	for i := range out {
		row := m.lineAt(i-m.scroll, lines)
		if selected {
			// A row the selection touches is re-rendered from its cells with
			// the highlight applied; every other row keeps the fast path.
			if highlighted, ok := m.highlightRow(i); ok {
				row = highlighted
			}
		}
		out[i] = fitCells(row, m.cols)
	}
	return out
}

// lineAt is the row that belongs at a position on the visible screen, counting
// negative positions back into the scrollback.
func (m *Model) lineAt(at int, screen []string) string {
	if at < 0 {
		history := m.ScrollbackLen()
		if index := history + at; index >= 0 && m.emu != nil {
			return m.emu.Scrollback().Line(index).Render()
		}
		return ""
	}
	if at < len(screen) {
		return screen[at]
	}
	return ""
}

// ScrollbackLen is how many lines have scrolled off the top and been kept.
func (m *Model) ScrollbackLen() int {
	if m.emu == nil {
		return 0
	}
	return m.emu.ScrollbackLen()
}

// Scroll moves the view back through the scrollback, positive being upward, and
// reports where it ended up. It is bounded by what has been kept, so it stops at
// the top of the history rather than running off into blank rows.
//
// The view returns to the live screen on its own the moment anything new
// arrives; see Update.
func (m *Model) Scroll(delta int) int {
	m.scroll = min(max(m.scroll+delta, 0), m.ScrollbackLen())
	return m.scroll
}

// ScrollOffset is how far back the view is, zero being the live screen.
func (m *Model) ScrollOffset() int { return m.scroll }

// fitCells truncates or pads a rendered line to exactly width cells, measuring
// display cells rather than bytes and leaving any style the line opened closed
// behind it, so nothing bleeds into whatever is drawn next to it.
func fitCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, "") + ansi.ResetStyle
	if w := ansi.StringWidth(s); w < width {
		s += strings.Repeat(" ", width-w)
	}
	return s
}

// Cursor is where the hardware cursor belongs, given the screen position the
// pane's first row was drawn at. It is nil when there is nothing to place one
// on, and when the application has hidden it.
func (m *Model) Cursor(originX, originY int) *tea.Cursor {
	if m.emu == nil || !m.attached {
		return nil
	}
	m.mu.Lock()
	visible, shape, blink, col := m.cursorVisible, m.cursorStyle, m.cursorBlink, m.cursorColor
	m.mu.Unlock()
	if !visible {
		return nil
	}
	pos := m.emu.CursorPosition()
	if pos.X < 0 || pos.Y < 0 || pos.X >= m.cols || pos.Y >= m.rows {
		return nil
	}
	cursor := tea.NewCursor(originX+pos.X, originY+pos.Y)
	cursor.Shape, cursor.Blink, cursor.Color = shape, blink, col
	return cursor
}

// Title is the title the application has set, if it has set one.
func (m *Model) Title() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.title
}

// AltScreen reports whether the application is on the alternate screen, which
// is how you tell a full-screen program from a shell prompt.
func (m *Model) AltScreen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.altScreen
}

// Bells is how many times the application has rung the bell. It is a count
// rather than a callback so a host can notice one without being interrupted on
// somebody else's goroutine.
func (m *Model) Bells() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bells
}

// Attached reports whether a stream is being drawn.
func (m *Model) Attached() bool { return m.attached }

// Err is why the session ended, if it ended badly.
func (m *Model) Err() error { return m.err }

// Close ends the session and releases everything behind it. It is safe to call
// on a pane that was never attached, and safe to call twice.
func (m *Model) Close() error { return m.detach() }

func (m *Model) detach() error {
	if m.reader != nil {
		m.reader.stop()
		m.reader = nil
	}
	var err error
	if m.emu != nil {
		m.stopForwarder(m.emu)
		err = m.emu.Close()
		m.emu = nil
	}
	if m.stream != nil {
		if cerr := m.stream.Close(); err == nil {
			err = cerr
		}
		m.stream = nil
	}
	m.attached = false
	m.prefixArmed, m.prefixRepeat = false, false
	m.forwardDone, m.forwardOut = nil, nil
	m.sel, m.selShot = nil, ""
	return err
}

// stopForwarder gets the input drain out of the emulator before the emulator is
// closed under it. The writer it feeds leaves on the same signal.
//
// Closing the emulator is what would unblock its Read, and it is what the
// obvious version of this does — but Emulator.Read and Emulator.Close both
// touch the emulator's closed flag with no synchronization between them, so
// closing while a read is in flight is a data race. (The unblocking itself is
// safe: it goes through an io.Pipe. It is the flag that races.)
//
// So the drain is woken instead of interrupted: a byte written to the
// emulator's input pipe — the same pipe its Read is waiting on — returns that
// read, the drain sees the done signal and leaves without forwarding it, and
// only then is the emulator closed, with nothing inside Read to race.
//
// The write itself is on a goroutine because an io.Pipe write blocks until it
// is read: if the drain has already gone, nothing will read it, and the
// close below is what releases it.
func (m *Model) stopForwarder(emu *vt.Emulator) {
	if m.forwardDone == nil {
		return
	}
	close(m.forwardDone)
	go func() { _, _ = emu.InputPipe().Write([]byte{0}) }()
	<-m.forwardOut
}
