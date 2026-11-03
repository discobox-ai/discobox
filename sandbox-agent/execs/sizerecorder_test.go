//go:build !windows

package execs

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
)

// runSizeRecorder is the program the repaint test runs on the PTY: it says
// hello, then appends the terminal's size to path on every SIGWINCH and never
// prints again.
//
// It is this test binary rather than a shell trap because a shell can lose the
// signal. bash drops a SIGWINCH that arrives while it is still running the
// trap for the one before — and the size the trap records is on disk before
// the trap has returned, so a test that answers each record with the next
// resize lands in exactly that window. A Go program cannot lose the last one:
// a signal that arrives while a size is being recorded leaves another
// notification queued, and the size is read after it, so the last line is
// always the size the terminal was last given.
func runSizeRecorder(path string) int {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	fmt.Print("hello")
	for range winch {
		size, err := pty.GetsizeFull(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := appendSize(path, size); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

func appendSize(path string, size *pty.Winsize) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// One write per record, so a reader polling the file sees whole lines.
	_, err = fmt.Fprintf(file, "%d %d\n", size.Rows, size.Cols)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
