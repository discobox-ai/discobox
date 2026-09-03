//go:build unix

package tui

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// The window on a real terminal, driven by the real renderer.
//
// Everything else in this package drives the model directly, which is where the
// window's own decisions are — but not where these two land. Both are about
// rows the window prints on the screen it was started from, and what it costs
// to leave one there: nothing above the window can be erased once the terminal
// has scrolled it away.
//
//   - The renderer keeps the latest frame and writes whichever is current when
//     its clock fires, so a frame held briefly may never be written at all. The
//     erasing frame is exactly that, and a slow terminal is what drops it, so
//     one case runs the clock slow enough to make that a certainty rather than
//     a chance.
//   - A frame taller than the screen scrolls its own top rows into the
//     scrollback as it is printed, so another case runs on a screen short
//     enough that the frame the window used to draw would not have fit.
//
// The mark is put back by hand because go test's output is not a terminal, so
// the window would draw no color and drop it — and it is twelve of the rows
// this is about.
func TestThePromptIsErasedFromTheScreenTheWindowLeavesBehind(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows int
		fps  int
	}{
		{"a terminal with room", 30, 60},
		{"a terminal too slow to draw every frame", 30, 4},
		{"the screen given back and taken again", 30, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := newTerminal(t, 141, tc.rows)
			// The shell the window was started from, so the rows it prints have
			// something above them, the way they do on a real screen.
			term.print("darren@host:~/src/disco2$ ./build/discobox")

			// A window with something to report under it, which is the frame
			// growing by a row while it is on screen.
			updates := make(chan string, 1)
			ds := newFakeSource(testSandboxes()...)
			ds.workspace = SourceWorkspace{Directory: "/src/disco2", Repository: true, Carries: true}
			m := New(t.Context(), ds, WithInitialization("staging", updates))
			m.logo = newLogo(true)
			program := tea.NewProgram(m,
				tea.WithInput(term.tty), tea.WithOutput(term.tty), tea.WithFPS(tc.fps))
			done := make(chan struct{})
			go func() {
				defer close(done)
				if _, err := program.Run(); err != nil {
					t.Error(err)
				}
			}()

			term.wait(t, "the opening prompt", func() bool {
				return strings.Contains(term.screen(), "discoboxes you already have")
			})
			updates <- "pulling images"
			term.wait(t, "the line under the window", func() bool {
				return strings.Contains(term.screen(), "pulling images")
			})

			if tc.name == "the screen given back and taken again" {
				// Enter on a dirty working tree asks about it in a modal,
				// which stands in place of the window: the screen is taken for
				// the modal, given back to the prompt when it is answered, and
				// taken again for the terminal that follows.
				term.send("\r")
				term.wait(t, "the modal", term.altScreen)
				term.send("\r")
				term.wait(t, "the prompt back", func() bool { return !term.altScreen() })
			} else {
				term.send("\t")
			}
			term.wait(t, "the window to take the screen", term.altScreen)

			program.Quit()
			<-done
			term.wait(t, "the window to give the screen back", func() bool { return !term.altScreen() })

			// What the window printed is off the screen it printed it on, and
			// out of the scrollback above it: the command that started it is
			// all that is left up there.
			if left := term.printedRows(); len(left) > 0 {
				t.Errorf("the window left %d of its rows on the screen it was started from:\n%s",
					len(left), strings.Join(left, "\n"))
			}
		})
	}
}

// terminal is a pty with an emulator reading it, which is both how the test
// sees what the window drew and how the window gets the answers a terminal
// gives — the cursor reports the erasing frame is acknowledged with among them.
type terminal struct {
	ptmx *os.File
	tty  *os.File
	emu  *vt.Emulator
	mu   sync.Mutex
}

func newTerminal(t *testing.T, cols, rows int) *terminal {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("open a terminal: %v", err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	if err := pty.Setsize(tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		t.Fatalf("size the terminal: %v", err)
	}

	term := &terminal{ptmx: ptmx, tty: tty, emu: vt.NewEmulator(cols, rows)}
	term.emu.SetScrollbackSize(200)
	// What the emulator has to say — its answers to the window's questions —
	// goes back the way a terminal's would, as input.
	go func() { _, _ = io.Copy(ptmx, term.emu) }()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				term.mu.Lock()
				_, _ = term.emu.Write(buf[:n])
				term.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return term
}

// print puts a line on the screen from the far side, as whatever was running in
// the terminal before the window opened did.
func (term *terminal) print(line string) {
	_, _ = io.WriteString(term.tty, line+"\r\n")
}

// send types at the window.
func (term *terminal) send(keys string) {
	_, _ = io.WriteString(term.ptmx, keys)
}

func (term *terminal) screen() string {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.emu.Render()
}

func (term *terminal) altScreen() bool {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.emu.IsAltScreen()
}

// printedRows is what the window left behind on the screen it was started from,
// scrollback included: rows carrying its frame, its mark, or the box around
// either.
func (term *terminal) printedRows() []string {
	term.mu.Lock()
	defer term.mu.Unlock()
	rows := strings.Split(term.emu.Render(), "\n")
	scrollback := term.emu.Scrollback()
	for i := range scrollback.Len() {
		rows = append(rows, scrollback.Line(i).Render())
	}
	var left []string
	for _, row := range rows {
		if strings.ContainsAny(row, "│╭╮╰╯▗▖▍▋▌") {
			left = append(left, strings.TrimRight(row, " "))
		}
	}
	return left
}

func (term *terminal) wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; the screen:\n%s", what, term.screen())
}
