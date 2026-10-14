//go:build !windows

package tui

import (
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// resizeOnInit is the window with its terminal resized at the one moment
// Bubble Tea cannot hear it: after the size is read and before SIGWINCH is
// listened for, which is when Init runs. It records each frame it draws, on the
// runtime's own goroutine, so the test can read what the window meant to show.
type resizeOnInit struct {
	*Model
	resize func()
	drawn  *drawnFrame
}

func (r resizeOnInit) Init() tea.Cmd {
	r.resize()
	return r.Model.Init()
}

func (r resizeOnInit) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// This is the real runtime, so the window's timers and listeners are
	// run as the runtime runs them, not left as the test seams hand them
	// back (TestMain).
	switch msg := msg.(type) {
	case timerMsg:
		return r, tea.Tick(msg.after, msg.fire)
	case listenerMsg:
		return r, msg.run
	}
	_, cmd := r.Model.Update(msg)
	return r, cmd
}

func (r resizeOnInit) View() tea.View {
	view := r.Model.View()
	r.drawn.set(r.height, r.shimmer == 0, r.lastFrame)
	return view
}

// drawnFrame is the last frame the window drew, as plain rows, with the
// height it was drawn for and whether the glint was over by then.
type drawnFrame struct {
	mu     sync.Mutex
	height int
	still  bool
	rows   []string
}

func (d *drawnFrame) set(height int, still bool, frame string) {
	rows := strings.Split(ansi.Strip(frame), "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.height, d.still, d.rows = height, still, rows
}

func (d *drawnFrame) get() (height int, still bool, rows []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.height, d.still, d.rows
}

// A terminal that settles as the window opens must not leave the renderer
// drawing for the size it had before. When it did, the frame was two rows
// taller than the screen, the screen scrolled, and the composer's glint was
// painted two rows under the prompt and left there.
//
// The test process is not the pty's controlling process, so no SIGWINCH is
// delivered at all here: the new size is only learned by asking for it.
func TestWindowLearnsASizeChangedWhileOpening(t *testing.T) {
	const width, before, after = 120, 40, 38
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close(); _ = tty.Close() })
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: width, Rows: before}); err != nil {
		t.Fatal(err)
	}

	screen := &lockedScreen{emu: vt.NewEmulator(width, after)}
	go func() { _, _ = io.Copy(io.Discard, screen.emu) }()
	go func() { _, _ = io.Copy(screen, ptmx) }()

	m := New(t.Context(), newFakeSource())
	m.st = newStyles(true)
	drawn := &drawnFrame{}
	model := resizeOnInit{Model: m, drawn: drawn, resize: func() {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: width, Rows: after})
	}}
	p := tea.NewProgram(model,
		tea.WithContext(t.Context()),
		tea.WithInput(tty),
		tea.WithOutput(tty),
		tea.WithColorProfile(colorprofile.ANSI256),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithoutSignals(),
	)
	done := make(chan struct{})
	go func() { _, _ = p.Run(); close(done) }()
	t.Cleanup(func() { p.Quit(); <-done })

	// The window has caught up once it has drawn a still frame at the
	// terminal's size, and the terminal has caught up once its screen is that
	// frame. Both happen on clocks of their own, so they are waited for rather
	// than slept past.
	deadline := time.Now().Add(10 * time.Second)
	for {
		height, still, want := drawn.get()
		got := screen.lines()
		if height == after && still && slices.Equal(got[:after], want[:min(after, len(want))]) {
			return
		}
		if time.Now().After(deadline) {
			if height != after {
				t.Fatalf("window is %d rows, the terminal %d", height, after)
			}
			for i := range after {
				if i >= len(want) || got[i] != want[i] {
					t.Errorf("row %d on screen\n got: %q\nwant: %q", i, got[i], want[min(i, len(want)-1)])
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockedScreen is a terminal emulator fed from one goroutine and read from
// another.
type lockedScreen struct {
	mu  sync.Mutex
	emu *vt.Emulator
}

func (s *lockedScreen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emu.Write(p)
}

// lines is the screen as text, each row without its trailing blanks.
func (s *lockedScreen) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := strings.Split(s.emu.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}
