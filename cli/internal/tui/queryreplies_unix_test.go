//go:build !windows

package tui

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	xterm "golang.org/x/term"
)

// slowTerminal answers the queries a window sends the way a real terminal
// does, but late: Windows Terminal behind WSL takes tens of milliseconds to
// answer, longer than an attach that is refused at once takes to fail.
type slowTerminal struct {
	ptmx  *os.File
	delay time.Duration
	mu    sync.Mutex
	seen  bytes.Buffer
}

var slowTerminalReplies = []struct{ query, reply string }{
	{"\x1b[?2027$p", "\x1b[?2027;3$y"},
	{"\x1b[?2026$p", "\x1b[?2026;2$y"},
	{"\x1b[c", "\x1b[?61;4;6;7;14;21;22;23;24;28;32;42;52c"},
	{"\x1b[?u", "\x1b[?1u"},
}

func (s *slowTerminal) run() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.seen.Write(buf[:n])
			for _, q := range slowTerminalReplies {
				for c := strings.Count(string(buf[:n]), q.query); c > 0; c-- {
					reply := q.reply
					time.AfterFunc(s.delay, func() { _, _ = s.ptmx.Write([]byte(reply)) })
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// An attach that is refused as soon as the window opens — an archived
// discobox — must not leave the terminal's answers to the window's queries
// behind it, where the shell the command returns to reads them as typed input
// (`^[[?2027;3$y^[[?61;…c^[[?1u` at the prompt).
func TestARefusedAttachLeavesNoTerminalRepliesForTheShell(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close(); _ = tty.Close() })
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: 120, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	// The shell's end: what is still unread on the terminal once the command
	// returns. Opened apart from the window's, which the runtime may close.
	shell, err := os.OpenFile(tty.Name(), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shell.Close() })

	term := &slowTerminal{ptmx: ptmx, delay: 80 * time.Millisecond}
	go term.run()

	ds := newFakeSource(testSandboxes()...)
	ds.openExecErr = errors.New("sandbox is archived; unarchive it first")
	m := New(t.Context(), ds, WithAttach(testSandboxes()[1]))
	p := tea.NewProgram(realRuntime{m},
		tea.WithContext(t.Context()),
		tea.WithInput(tty),
		tea.WithOutput(tty),
		tea.WithColorProfile(colorprofile.ANSI256),
		tea.WithEnvironment([]string{"TERM=xterm-256color"}),
		tea.WithoutSignals(),
	)
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		p.Kill()
		t.Fatal("the window never closed on the refused attach")
	}
	if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "archived") {
		t.Fatalf("window ended with %v, want the refusal", m.exitErr)
	}

	// Every answer the terminal owes has arrived by now.
	time.Sleep(4 * term.delay)
	// The shell reads its line raw, as zsh's line editor does, so a reply
	// with no newline after it is input it reads.
	// Raw through x/term and a read that does not wait, rather than termios
	// by hand, whose request names differ between Linux and the BSDs.
	fd := int(shell.Fd())
	if _, err := xterm.MakeRaw(fd); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	left := make([]byte, 4096)
	n, _ := unix.Read(fd, left)
	if n > 0 {
		term.mu.Lock()
		sent := term.seen.String()
		term.mu.Unlock()
		t.Fatalf("the shell reads %q once the window is gone (window sent %q)", left[:n], sent)
	}
}
