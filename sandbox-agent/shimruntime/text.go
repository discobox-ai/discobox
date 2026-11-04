package shimruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// ScreenText is a terminal's screen as a person looking at it would read it
// (ADR 0137 §1): rendered from the emulator's cells, so a region a program
// redrew a hundred times is there once, and carrying no escape sequences.
type ScreenText struct {
	Rows          int      `json:"rows"`
	Cols          int      `json:"cols"`
	CursorRow     int      `json:"cursorRow"`
	CursorCol     int      `json:"cursorCol"`
	CursorVisible bool     `json:"cursorVisible"`
	Title         string   `json:"title,omitempty"`
	AltScreen     bool     `json:"altScreen"`
	Lines         []string `json:"lines"`
	// Scrollback are the lines above the screen, oldest first. The alternate
	// screen has none.
	Scrollback []string   `json:"scrollback,omitempty"`
	OutputAt   *time.Time `json:"outputAt,omitempty"`
	// SentAt is when the input route last wrote to the terminal — send,
	// send-key, admin terminal input — and not an attach, whose frames carry
	// what the person's own terminal sends unasked (focus reports, query
	// answers): where a wait counting from the last input begins
	// (ADR 26-11-04-204 §4).
	SentAt *time.Time `json:"sentAt,omitempty"`
	// Exited is the shim's to fill: the runtime does not know the process.
	Exited bool `json:"exited"`
}

// ErrNoScreen is a terminal operation asked of an exec that has no TTY, and so
// no screen to read or keys to send.
var ErrNoScreen = errors.New("exec has no terminal")

// ScreenText renders the current screen, and up to scrollback lines above it.
func (r *Runtime) ScreenText(scrollback int) (ScreenText, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.screen == nil {
		return ScreenText{}, ErrNoScreen
	}
	var text ScreenText
	r.runScreenLocked(func(screen *screenBuffer) { text = screen.text(scrollback) })
	if !r.outputAt.IsZero() {
		at := r.outputAt
		text.OutputAt = &at
	}
	if !r.sentAt.IsZero() {
		at := r.sentAt
		text.SentAt = &at
	}
	return text, nil
}

func (s *screenBuffer) text(scrollback int) ScreenText {
	width, height := s.emu.Width(), s.emu.Height()
	pos := s.emu.CursorPosition()
	text := ScreenText{
		Rows:          height,
		Cols:          width,
		CursorRow:     pos.Y,
		CursorCol:     pos.X,
		CursorVisible: !s.modes.seen[25] || s.modes.set[25],
		Title:         s.title,
		AltScreen:     s.emu.IsAltScreen(),
		Lines:         make([]string, height),
	}
	for y := 0; y < height; y++ {
		line := make(uv.Line, width)
		for x := 0; x < width; x++ {
			if cell := s.emu.CellAt(x, y); cell != nil {
				line[x] = *cell
			}
		}
		text.Lines[y] = strings.TrimRight(line.String(), " ")
	}
	if !text.AltScreen && scrollback > 0 {
		sb := s.emu.Scrollback()
		start := max(sb.Len()-scrollback, 0)
		for i := start; i < sb.Len(); i++ {
			// A scrollback line is kept rendered; its text is that without the
			// escape sequences, the same as the line's cells would give.
			text.Scrollback = append(text.Scrollback, strings.TrimRight(ansi.Strip(sb.RenderedLine(i)), " "))
		}
	}
	return text
}

// InputPart is one piece of input written to a terminal: text, or one named
// key (ADR 0137 §2).
type InputPart struct {
	Text string `json:"text,omitempty"`
	Key  string `json:"key,omitempty"`
}

// EncodeInput turns input parts into the bytes to write to the terminal, as
// the program running in it asked to receive them: text as a bracketed paste
// when it enabled one, and cursor keys in application mode when it set that.
func (r *Runtime) EncodeInput(parts []InputPart) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.screen == nil {
		return nil, ErrNoScreen
	}
	bracketedPaste := r.screen.modes.set[2004]
	appCursor := r.screen.modes.set[1]
	var out []byte
	for i, part := range parts {
		switch {
		case part.Text != "" && part.Key != "":
			return nil, fmt.Errorf("input part %d names both text and a key", i)
		case part.Key != "":
			sequence, ok := keySequence(part.Key, appCursor)
			if !ok {
				return nil, fmt.Errorf("input part %d names unknown key %q", i, part.Key)
			}
			out = append(out, sequence...)
		case part.Text != "":
			if bracketedPaste {
				// A paste cannot be allowed to end itself early: an end
				// marker inside the text would turn the rest of it into typing.
				text := strings.ReplaceAll(part.Text, "\x1b[201~", "")
				out = append(out, "\x1b[200~"+text+"\x1b[201~"...)
			} else {
				out = append(out, part.Text...)
			}
		default:
			return nil, fmt.Errorf("input part %d is empty", i)
		}
	}
	return out, nil
}

// keySequence is the byte sequence a terminal sends for a named key.
func keySequence(name string, appCursor bool) (string, bool) {
	cursor := func(final string) string {
		if appCursor {
			return "\x1bO" + final
		}
		return "\x1b[" + final
	}
	switch name {
	case "Enter":
		return "\r", true
	case "Space":
		// A key, never a paste: a program in bracketed paste reads a pasted
		// space as text, not as the press that toggles or pages.
		return " ", true
	case "Tab":
		return "\t", true
	case "Escape":
		return "\x1b", true
	case "Backspace":
		return "\x7f", true
	case "Delete":
		return "\x1b[3~", true
	case "Up":
		return cursor("A"), true
	case "Down":
		return cursor("B"), true
	case "Right":
		return cursor("C"), true
	case "Left":
		return cursor("D"), true
	case "Home":
		return cursor("H"), true
	case "End":
		return cursor("F"), true
	case "PageUp":
		return "\x1b[5~", true
	case "PageDown":
		return "\x1b[6~", true
	}
	if letter, ok := strings.CutPrefix(name, "C-"); ok && len(letter) == 1 && letter[0] >= 'a' && letter[0] <= 'z' {
		return string(rune(letter[0] - 'a' + 1)), true
	}
	return "", false
}

// Wait reasons a terminal reports (ADR 0137 §3), and the program's report
// (ADR 26-11-04-204 §5).
const (
	WaitQuiet   = "quiet"
	WaitExit    = "exit"
	WaitTimeout = "timeout"
	WaitProgram = "program"
)

// noteOutputLocked records that the program just wrote output, and wakes
// anything waiting on the terminal. Called with r.mu held.
func (r *Runtime) noteOutputLocked() {
	r.outputAt = time.Now().UTC()
	r.signalActivityLocked()
}

// NoteInput records that the terminal was just written to. Input restarts a
// quiet period like output does: a program that was typed to has not yet had
// its chance to answer.
func (r *Runtime) NoteInput() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputAt = time.Now().UTC()
	r.signalActivityLocked()
}

// NoteSent records that the input route is about to write to the terminal,
// for a wait to count from (ADR 26-11-04-204 §4). It is stamped before the
// write, so whatever the input causes is reported after it.
func (r *Runtime) NoteSent() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sentAt = time.Now().UTC()
}

func (r *Runtime) signalActivityLocked() {
	if r.activitySignal != nil {
		close(r.activitySignal)
	}
	r.activitySignal = make(chan struct{})
}

// ProgramMatch is a program status a wait ends on: a state, and for blocked
// optionally the kind. An empty Kind matches any kind.
type ProgramMatch struct {
	State programstatus.State `json:"state"`
	Kind  programstatus.Kind  `json:"kind,omitempty"`
}

// Holds reports whether status is one m matches.
func (m ProgramMatch) Holds(status programstatus.Status) bool {
	return status.State == m.State && (m.Kind == "" || status.Kind == m.Kind)
}

// Await is what a terminal wait asks of the runtime: what to wait for, and
// the point it counts from.
type Await struct {
	// Since is the point the wait counts from: quiet is measured from no
	// earlier than it, and only a report after it counts. Zero is the call.
	Since time.Time
	// Quiet is how long the terminal must go without output or input; zero
	// waits on no quiet.
	Quiet time.Duration
	// Program is the program statuses to end on, matched against the
	// effective status (ADR 26-10-16-424 §3) after a report past Since.
	Program []ProgramMatch
	// Done ends the wait as an exit when it closes; nil waits on no exit.
	Done <-chan struct{}
}

// AwaitResult is what a wait ended on: its reason, and for a program status
// the status that held.
type AwaitResult struct {
	Reason  string
	Program *programstatus.Status
}

// Await blocks until one of what w names holds, done closes, or ctx ends,
// and says which (ADR 0137 §3, ADR 26-11-04-204 §§4-5).
//
// Quiet starts no earlier than w.Since, or the call when it is zero: output
// from before it says nothing about whether the program has answered what it
// was just sent. A program status counts only from a report after the same
// point, so a state that held before the input does not end the wait.
//
// Every report arrives as output, which wakes this: Observe signals under the
// lock it feeds the emulator under, so the status read after the wake has the
// report in it.
func (r *Runtime) Await(ctx context.Context, w Await) AwaitResult {
	since := w.Since
	if since.IsZero() {
		since = time.Now().UTC()
	}
	for {
		r.mu.Lock()
		if r.activitySignal == nil {
			r.activitySignal = make(chan struct{})
		}
		signal := r.activitySignal
		last := since
		for _, at := range []time.Time{r.outputAt, r.inputAt} {
			if at.After(last) {
				last = at
			}
		}
		var held *programstatus.Status
		if len(w.Program) > 0 && r.screen != nil {
			r.runScreenLocked(func(screen *screenBuffer) {
				status, ok := screen.programStatusNow()
				if !ok || !status.ReportedAt.After(since) {
					return
				}
				for _, match := range w.Program {
					if match.Holds(status) {
						held = &status
						return
					}
				}
			})
		}
		r.mu.Unlock()
		if held != nil {
			return AwaitResult{Reason: WaitProgram, Program: held}
		}

		var timer *time.Timer
		var quietTimer <-chan time.Time
		if w.Quiet > 0 {
			remaining := time.Until(last.Add(w.Quiet))
			if remaining <= 0 {
				return AwaitResult{Reason: WaitQuiet}
			}
			timer = time.NewTimer(remaining)
			quietTimer = timer.C
		}
		reason := ""
		select {
		case <-w.Done:
			reason = WaitExit
		case <-ctx.Done():
			reason = WaitTimeout
		case <-quietTimer:
			reason = WaitQuiet
		case <-signal:
		}
		if timer != nil {
			timer.Stop()
		}
		if reason != "" {
			return AwaitResult{Reason: reason}
		}
	}
}
