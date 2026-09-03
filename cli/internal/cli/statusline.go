package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// statusLine is one line of "what is happening right now" on a stream that is
// about to be used for something else.
//
// On a terminal it rewrites in place, so a wait that passes through six phases
// costs one line rather than six, and clearing it leaves the stream exactly as
// it was found — which matters because the next thing written to it is usually
// a full-screen terminal session.
//
// Off a terminal it appends each distinct line instead. A run in CI has no
// cursor to move and every reason to keep the record of what the wait was
// spent on.
//
// On a terminal it also carries a spinner and the window's colors. Every wait
// long enough to narrate is long enough to be mistaken for a hang, and the
// phase text alone does not answer that: a server migrating a large database
// reports "migrating the database" once and then says nothing for a minute. The
// spinner is the part that keeps moving. The colors are the window's rather than
// this file's because a single command draws both — `discobox attach` launches a
// server, narrates that on this line, then narrates the discobox it is waiting
// for on another — and two accents for the same idea reads as two different
// things happening.
type statusLine struct {
	out io.Writer
	tty bool
	// color is whether the stream will show any, decided once. A terminal
	// without it still gets the spinner: the glyph is the liveness signal, and
	// the paint is decoration on top of it.
	color bool

	mu sync.Mutex
	// shown is whether a line is currently drawn and needs erasing.
	shown bool
	// done makes a late set a no-op. The narration runs in its own goroutine
	// and is canceled rather than joined, so a report can arrive after the
	// caller has taken the stream back; writing it then would leave text on a
	// line the caller believes it cleared.
	done bool
	// text is what the line currently says, kept so the spinner can redraw it
	// between reports rather than only on the change of one.
	text string
	// frame is where the spinner has got to.
	frame int
	// ticking is whether the spinner goroutine is running. It starts on the
	// first line drawn rather than at construction, so a wait that turns out to
	// be short enough never to draw costs no goroutine at all.
	ticking bool
	// suspended is the line held down while something else has the stream: a
	// question that draws its own screen on it. The wait carries on and the
	// reports keep landing — they are what the line says when it comes back —
	// but nothing is drawn until it does. See suspend.
	suspended bool
	stop      chan struct{}
}

// The spinner is the braille cycle, in the window's own gold, and the text is
// the amber the window paints its busy line in (`tui.colWarn`). Held as
// 256-color indices for the same reason the window holds them that way.
const (
	statusSpinnerColor = "220"
	statusTextColor    = "214"
	// spinnerInterval is fast enough to read as motion and slow enough that the
	// line is not the most eye-catching thing on a screen it shares with a
	// command's own output.
	statusSpinnerInterval = 90 * time.Millisecond
)

var statusSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func newStatusLine(out io.Writer) *statusLine {
	tty := isTerminalStream(out)
	return &statusLine{
		out:   out,
		tty:   tty,
		color: tty && streamHasColor(out),
		stop:  make(chan struct{}),
	}
}

// streamHasColor reports whether this stream will show color. NO_COLOR is
// honored here rather than checked for directly: colorprofile reads it, per
// no-color.org, along with everything else that decides a profile.
//
// Detected against the stream the line is drawn on rather than against stdout,
// because a status line goes to stderr and the two can be redirected apart.
func streamHasColor(out io.Writer) bool {
	return colorprofile.Detect(out, os.Environ()) > colorprofile.ASCII
}

// set replaces what the line says.
func (l *statusLine) set(text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	text = oneLine(text, l.textWidth())
	if text == "" {
		return
	}
	if !l.tty {
		fmt.Fprintln(l.out, text)
		return
	}
	l.text = text
	// A report that arrives while something else has the stream is remembered
	// rather than drawn: it is what the line will say when it comes back.
	if l.suspended {
		return
	}
	l.draw()
	l.startSpinner()
}

// textWidth is how much room the message has, which on a terminal is the row
// less what the spinner and its separating space occupy. Passing the whole
// width would let a line that exactly fits the row spill onto the next one once
// the spinner was prepended, and \r\x1b[K cannot erase a row the cursor has
// already left.
func (l *statusLine) textWidth() int {
	width := terminalWidth(l.out)
	if width <= 0 || !l.tty {
		return width
	}
	return width - runeLen(statusSpinnerFrames[0]) - 1
}

// draw writes the current line. The caller holds the mutex.
//
// Erase before writing rather than after: a shorter line must not leave the
// tail of a longer one behind it.
func (l *statusLine) draw() {
	fmt.Fprintf(l.out, "\r\x1b[K%s", l.render())
	l.shown = true
}

// erase takes the row back down, leaving the cursor at the start of it. The
// caller holds the mutex.
func (l *statusLine) erase() {
	if !l.shown {
		return
	}
	fmt.Fprint(l.out, "\r\x1b[K")
	l.shown = false
}

// print writes a line that stays, above the one being rewritten in place: the
// row this owns is erased, the text is written where the scrollback keeps it,
// and the status line is drawn again underneath.
//
// It is how anything else says something on this stream while a wait is being
// narrated. Writing to the stream directly instead writes into the row this
// line owns, and the result is the report with the spinner glued to the front
// of it — "⠧ preparing sourcesource x: /home/ada/src/x".
func (l *statusLine) print(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	text := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	l.erase()
	fmt.Fprint(l.out, text)
	if l.done || l.suspended || !l.tty || l.text == "" {
		return
	}
	l.draw()
	l.startSpinner()
}

// suspend hands the stream to something that draws on it itself — a question
// with its own screen — and returns what takes it back. The wait is not over,
// so this is not clear: reports keep arriving while the line is down, and what
// the last one said is what comes back.
//
// The returned func is safe to call more than once, and a line already
// suspended or already cleared returns one that does nothing.
func (l *statusLine) suspend() func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done || l.suspended {
		return func() {}
	}
	l.suspended = true
	l.erase()
	return sync.OnceFunc(l.resume)
}

// resume draws the line again after a suspend, saying whatever the last report
// left. A line cleared while it was down stays down: clearing is final.
func (l *statusLine) resume() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.suspended = false
	if l.done || !l.tty || l.text == "" {
		return
	}
	l.draw()
	l.startSpinner()
}

// render is the styled line, or the plain text where there is nothing to style
// it with. The caller holds the mutex.
func (l *statusLine) render() string {
	spinner := statusSpinnerFrames[l.frame%len(statusSpinnerFrames)]
	if !l.color {
		return spinner + " " + l.text
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(statusSpinnerColor)).Render(spinner) +
		" " + lipgloss.NewStyle().Foreground(lipgloss.Color(statusTextColor)).Render(l.text)
}

// startSpinner begins advancing the glyph, once. The caller holds the mutex.
func (l *statusLine) startSpinner() {
	if l.ticking {
		return
	}
	l.ticking = true
	go l.spin()
}

// spin redraws the line on a beat until it is cleared. It writes nothing of its
// own: everything it draws is the text the last report left, so a spinner that
// outlives its caller by a tick cannot invent a line.
func (l *statusLine) spin() {
	ticker := time.NewTicker(statusSpinnerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.mu.Lock()
			if l.done || l.suspended || !l.shown {
				// The line is down. This goroutine lets go of the spinner so
				// that whoever puts the line back can start one again; while
				// it is down there is nothing to animate.
				l.ticking = false
				l.mu.Unlock()
				return
			}
			l.frame++
			l.draw()
			l.mu.Unlock()
		}
	}
}

// oneLine makes text that occupies exactly one terminal row.
//
// What arrives here is not always one line. A failure reported by a pool host
// is quoted verbatim, and those carry embedded newlines; a pull that names an
// image, a size and a layer count is simply long. Either one breaks a line that
// is rewritten in place, and in the same way: \r\x1b[K erases the row the
// cursor is on, so every row the text spilled onto is left on the screen for
// good — under whatever the caller drew next, which is the point at which the
// caller believed it had a clean stream.
//
// Folding whitespace rather than cutting at the first newline, because the
// interesting half of "connect to guest port 3002:\n  <detail>" is the half
// after the break.
func oneLine(text string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	if width <= 0 {
		return text
	}
	// One column short of the width: writing the last cell of a row leaves the
	// cursor there or on the next row depending on the terminal, and a status
	// line cannot afford to be unsure which row it is on.
	limit := width - 1
	if limit < 8 {
		limit = 8
	}
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

// clear takes the line back down and stops the status line from drawing again.
// It is safe to call more than once, and safe to call while a report is racing
// it.
func (l *statusLine) clear() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	// Closed under the mutex the spinner takes before it draws, so the goroutine
	// cannot be part way through a draw that lands after the erase below.
	close(l.stop)
	l.erase()
}
