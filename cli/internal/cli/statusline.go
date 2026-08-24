package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
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
type statusLine struct {
	out io.Writer
	tty bool

	mu sync.Mutex
	// shown is whether a line is currently drawn and needs erasing.
	shown bool
	// done makes a late set a no-op. The narration runs in its own goroutine
	// and is canceled rather than joined, so a report can arrive after the
	// caller has taken the stream back; writing it then would leave text on a
	// line the caller believes it cleared.
	done bool
}

func newStatusLine(out io.Writer) *statusLine {
	return &statusLine{out: out, tty: isTerminalStream(out)}
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
	text = oneLine(text, terminalWidth(l.out))
	if text == "" {
		return
	}
	if !l.tty {
		fmt.Fprintln(l.out, text)
		return
	}
	// Erase before writing rather than after: a shorter line must not leave the
	// tail of a longer one behind it.
	fmt.Fprintf(l.out, "\r\x1b[K%s", text)
	l.shown = true
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
	l.done = true
	if l.shown {
		fmt.Fprint(l.out, "\r\x1b[K")
		l.shown = false
	}
}
