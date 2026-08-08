//go:build !windows

package dockercache

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/creack/pty"
)

// watchResize keeps the child's pty the same size as the real terminal for as
// long as the build runs, so buildx's progress renderer rewraps with the
// window instead of staying at the size it started at. The returned function
// stops the subscription.
func watchResize(master *os.File) func() {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if s, err := pty.GetsizeFull(os.Stderr); err == nil {
				_ = pty.Setsize(master, s)
			}
		}
	}()
	return func() {
		signal.Stop(winch)
		close(winch)
	}
}
