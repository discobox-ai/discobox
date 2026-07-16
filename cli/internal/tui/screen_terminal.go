package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// detachHint documents the screen-style prefix that returns to the list.
const detachHint = "^a d detach"

// terminalScreen embeds a live sandbox terminal in a bordered pane, tmux-style.
// Output bytes stream into an in-memory vt emulator whose rendered screen is
// drawn inside the pane; key presses are encoded and forwarded to the remote
// terminal. A screen-style prefix (Ctrl-A then d) detaches back to the list.
type terminalScreen struct {
	ctx     context.Context
	cancel  context.CancelFunc
	ds      DataSource
	keys    keyMap
	styles  styles
	sandbox Sandbox

	width  int
	height int

	emu     *vt.Emulator
	term    Terminal
	reader  *ttyReader
	opening bool
	ready   bool
	closed  bool

	prefixArmed bool
	errText     string
}

func newTerminalScreen(ctx context.Context, ds DataSource, keys keyMap, st styles, sb Sandbox) *terminalScreen {
	ctx, cancel := context.WithCancel(ctx)
	return &terminalScreen{ctx: ctx, cancel: cancel, ds: ds, keys: keys, styles: st, sandbox: sb}
}

func (s *terminalScreen) Init() tea.Cmd { return nil }

func (s *terminalScreen) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case resizeMsg:
		s.width, s.height = msg.width, msg.height
		cols, rows := s.paneCells()
		if s.term == nil && !s.opening && cols > 0 && rows > 0 {
			s.opening = true
			return s, s.openCmd(cols, rows)
		}
		if s.emu != nil && cols > 0 && rows > 0 {
			s.emu.Resize(cols, rows)
			_ = s.term.Resize(cols, rows)
		}
		return s, nil

	case ttyOpenedMsg:
		s.opening = false
		s.term = msg.terminal
		s.reader = msg.reader
		cols, rows := s.paneCells()
		if cols < 1 {
			cols = 1
		}
		if rows < 1 {
			rows = 1
		}
		s.emu = vt.NewEmulator(cols, rows)
		s.ready = true
		// Forward the emulator's encoded input — user keys/paste plus the app's
		// query auto-replies — to the remote PTY.
		s.startInputForwarder()
		return s, tea.Batch(
			readNext(s.reader),
			waitTerminalEvent(s.ctx, s.term),
			func() tea.Msg { return statusMsg{text: "connected"} },
		)

	case ttyOutputMsg:
		if s.emu != nil {
			_, _ = s.emu.Write(msg.data)
		}
		return s, readNext(s.reader)

	case ttyConnectionMsg:
		text := ""
		switch msg.event.State {
		case TerminalReconnecting:
			text = "reconnecting…"
		case TerminalReconnected:
			text = "reconnected"
		}
		return s, tea.Batch(
			func() tea.Msg { return statusMsg{text: text} },
			waitTerminalEvent(s.ctx, s.term),
		)

	case tea.PasteMsg:
		// Bracketed paste from the host terminal. The emulator wraps it in the
		// app's bracketed-paste markers when the app enabled that mode.
		if s.ready && s.emu != nil {
			s.emu.Paste(msg.Content)
		}
		return s, nil

	case ttyClosedMsg:
		s.closed = true
		if msg.err != nil {
			s.errText = msg.err.Error()
		}
		// The session ended on its own; go back to the list.
		return s, func() tea.Msg { return backMsg{} }

	case errMsg:
		// Attach failed before the pane could open; surface it (the root sets the
		// status line) and return to the list rather than trapping the user.
		s.opening = false
		return s, func() tea.Msg { return backMsg{} }

	case tea.KeyPressMsg:
		return s.handleKey(msg)
	}
	return s, nil
}

func (s *terminalScreen) handleKey(msg tea.KeyPressMsg) (screen, tea.Cmd) {
	// Before the terminal is attached, esc/q back out rather than trapping the user.
	if !s.ready {
		if key.Matches(msg, s.keys.Back, s.keys.Quit) {
			return s, func() tea.Msg { return backMsg{} }
		}
		return s, nil
	}

	if s.prefixArmed {
		s.prefixArmed = false
		switch msg.String() {
		case "d":
			return s, func() tea.Msg { return backMsg{} }
		case "ctrl+a":
			s.emu.SendText("\x01") // a literal Ctrl-A to the app
			return s, nil
		default:
			// Send the swallowed prefix, then the key itself.
			s.emu.SendText("\x01")
			s.sendKey(msg)
			return s, nil
		}
	}

	if msg.String() == "ctrl+a" {
		s.prefixArmed = true
		return s, nil
	}

	s.sendKey(msg)
	return s, nil
}

// sendKey forwards a key press to the emulator. Printable text (with no
// ctrl/alt) is sent verbatim so shifted characters — uppercase letters and
// symbols like ! or @ — survive: the emulator's key encoder keys off the base
// Code and drops anything carrying a Shift modifier. Everything else
// (arrows, enter, ctrl/alt combos) goes through SendKey, which encodes it per
// the application's current modes (cursor-key mode, keypad mode, alt prefixing).
func (s *terminalScreen) sendKey(msg tea.KeyPressMsg) {
	k := msg.Key()
	if k.Text != "" && k.Mod&tea.ModCtrl == 0 && k.Mod&tea.ModAlt == 0 {
		s.emu.SendText(k.Text)
		return
	}
	s.emu.SendKey(uv.KeyPressEvent(uv.Key(k)))
}

// startInputForwarder copies the emulator's input side (the bytes produced by
// SendKey/SendText/Paste and by auto-replies to terminal queries) to the remote
// terminal. It exits when the emulator is closed or the write fails.
func (s *terminalScreen) startInputForwarder() {
	emu, term := s.emu, s.term
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := emu.Read(buf)
			if n > 0 {
				if _, werr := term.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// View draws the pane frame by hand. Passing the terminal grid through a
// lipgloss bordered box lets lipgloss re-wrap any line as wide as the box, which
// silently shifts rows and desyncs the hardware cursor. Instead every inner row
// is truncated and padded to exactly innerW cells (ANSI-aware) so the frame is
// fixed and the cursor offsets below are exact.
func (s *terminalScreen) View(width, height int) string {
	s.width, s.height = width, height
	innerW := width - 2
	innerH := height - 2
	if innerW < 1 || innerH < 2 {
		return ""
	}

	rows := make([]string, 0, innerH)
	rows = append(rows, s.titleBar(innerW))
	rows = append(rows, s.bodyRows(innerW, innerH-1)...)

	var b strings.Builder
	b.WriteString(s.borderLine("╭", "╮", innerW))
	side := s.styles.paneBorder.Render("│")
	for _, row := range rows {
		b.WriteByte('\n')
		b.WriteString(side)
		b.WriteString(row)
		b.WriteString(side)
	}
	b.WriteByte('\n')
	b.WriteString(s.borderLine("╰", "╯", innerW))
	return b.String()
}

// bodyRows returns exactly n content rows, each fitted to width cells.
func (s *terminalScreen) bodyRows(width, n int) []string {
	var lines []string
	switch {
	case s.errText != "":
		lines = []string{s.styles.statusError.Render(s.errText)}
	case !s.ready:
		lines = []string{s.styles.status.Render("attaching…")}
	default:
		lines = strings.Split(s.emu.Render(), "\n")
	}
	out := make([]string, n)
	for i := range out {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		out[i] = fitCells(line, width)
	}
	return out
}

func (s *terminalScreen) titleBar(innerW int) string {
	name := s.styles.paneTitle.Render(displayName(s.sandbox))
	hint := s.styles.status.Render(detachHint)
	gap := innerW - ansi.StringWidth(name) - ansi.StringWidth(hint)
	if gap < 1 {
		return fitCells(name, innerW)
	}
	return fitCells(name+strings.Repeat(" ", gap)+hint, innerW)
}

func (s *terminalScreen) borderLine(left, right string, innerW int) string {
	return s.styles.paneBorder.Render(left + strings.Repeat("─", innerW) + right)
}

func (s *terminalScreen) cursor(originX, originY int) *tea.Cursor {
	if !s.ready || s.emu == nil {
		return nil
	}
	pos := s.emu.CursorPosition()
	// The frame is: top border (1 row), title (1 row), then the grid. The cursor
	// sits one column in from the left border.
	x := originX + 1 + pos.X
	y := originY + 2 + pos.Y
	return tea.NewCursor(x, y)
}

func (s *terminalScreen) title() string {
	return fmt.Sprintf("terminal · %s", displayName(s.sandbox))
}

func (s *terminalScreen) helpBindings() []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithHelp("^a d", "detach")),
		key.NewBinding(key.WithHelp("keys", "sent to sandbox")),
	}
}

func (s *terminalScreen) fullHelpBindings() [][]key.Binding {
	return [][]key.Binding{s.helpBindings()}
}

// paneCells returns the emulator size implied by the current pane dimensions:
// the content area minus the border and the one-line title bar.
func (s *terminalScreen) paneCells() (cols, rows int) {
	cols = s.width - 2
	rows = s.height - 2 - 1
	if cols < 0 {
		cols = 0
	}
	if rows < 0 {
		rows = 0
	}
	return cols, rows
}

func (s *terminalScreen) openCmd(cols, rows int) tea.Cmd {
	ds, ctx, id := s.ds, s.ctx, s.sandbox.ID
	return func() tea.Msg {
		term, err := ds.OpenTerminal(ctx, id, cols, rows)
		if err != nil {
			return errMsg{context: "attach", err: err}
		}
		return ttyOpenedMsg{sandboxID: id, terminal: term, reader: newTTYReader(term)}
	}
}

func (s *terminalScreen) close() {
	s.cancel()
	if s.reader != nil {
		s.reader.stop()
	}
	if s.emu != nil {
		// Closing the emulator unblocks the input forwarder's Read.
		_ = s.emu.Close()
	}
	if s.term != nil {
		_ = s.term.Close()
	}
	s.closed = true
}

func waitTerminalEvent(ctx context.Context, terminal Terminal) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case event := <-terminal.Events():
			return ttyConnectionMsg{event: event}
		}
	}
}

// ttyReader pumps terminal output off the read goroutine into a channel the UI
// drains one message at a time, keeping blocking reads out of Update.
type ttyReader struct {
	ch   chan []byte
	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	err  error
}

func newTTYReader(t Terminal) *ttyReader {
	r := &ttyReader{ch: make(chan []byte, 64), done: make(chan struct{})}
	go r.loop(t)
	return r
}

func (r *ttyReader) loop(t Terminal) {
	defer close(r.ch)
	buf := make([]byte, 32*1024)
	for {
		n, err := t.Read(buf)
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
			r.setErr(err)
			return
		}
	}
}

func (r *ttyReader) stop() { r.once.Do(func() { close(r.done) }) }

func (r *ttyReader) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *ttyReader) error() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// readNext waits for the next output chunk, or reports closure when the stream
// ends.
func readNext(r *ttyReader) tea.Cmd {
	return func() tea.Msg {
		chunk, ok := <-r.ch
		if !ok {
			return ttyClosedMsg{err: r.error()}
		}
		return ttyOutputMsg{data: chunk}
	}
}
