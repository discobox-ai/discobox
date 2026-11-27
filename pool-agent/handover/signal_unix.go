//go:build unix

package handover

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// DrainRequested is the drain units apply asks this process for (SIGUSR2),
// taken only once this process holds its draining mark at markPath: a
// process that could not mark itself draining would let the one taking over
// sweep what it still holds. A signal whose mark cannot be taken is refused
// (refused is told why) and this process serves on, as if it had not come;
// units apply then finds it never began to drain. The drain is closed when
// the mark is held, which it is until Release or this process exits.
func DrainRequested(ctx context.Context, markPath string, refused func(error)) *Drain {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR2)
	drain := &Drain{requested: make(chan struct{})}
	go func() {
		// The signal stays caught for as long as the process runs: a later
		// one (a retried signal, an apply finishing a handover) is ignored,
		// never left to its default action, which would end the drain.
		defer signal.Stop(signals)
		taken := false
		for {
			select {
			case <-signals:
			case <-ctx.Done():
				return
			}
			if taken {
				continue
			}
			wait, cancel := context.WithTimeout(ctx, drainMarkWait)
			mark, err := Mark(wait, markPath)
			cancel()
			if err != nil {
				refused(err)
				continue
			}
			drain.mark = mark
			taken = true
			close(drain.requested)
		}
	}()
	return drain
}

// The environment systemd passes sockets in (sd_listen_fds(3)).
const (
	envListenPID     = "LISTEN_PID"
	envListenFDs     = "LISTEN_FDS"
	envListenFDNames = "LISTEN_FDNAMES"
	listenFDsStart   = 3
)

// Listeners are the listening sockets systemd passed this process, by the
// FileDescriptorName= of the socket unit each came from, taken once: the
// environment naming them is cleared, so a child this process starts is not
// handed them as well. A process systemd passed none gets none.
func Listeners() (map[string]net.Listener, error) {
	defer func() {
		_ = os.Unsetenv(envListenPID)
		_ = os.Unsetenv(envListenFDs)
		_ = os.Unsetenv(envListenFDNames)
	}()
	pid, err := strconv.Atoi(os.Getenv(envListenPID))
	if err != nil || pid != os.Getpid() {
		return map[string]net.Listener{}, nil
	}
	count, err := strconv.Atoi(os.Getenv(envListenFDs))
	if err != nil || count <= 0 {
		return map[string]net.Listener{}, nil
	}
	names := strings.Split(os.Getenv(envListenFDNames), ":")
	listeners := make(map[string]net.Listener, count)
	for i := range count {
		fd := listenFDsStart + i
		unix.CloseOnExec(fd)
		name := ""
		if i < len(names) {
			name = names[i]
		}
		file := os.NewFile(uintptr(fd), name)
		listener, err := net.FileListener(file)
		_ = file.Close()
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("socket %q systemd passed: %w", name, err)
		}
		if name == "" {
			_ = listener.Close()
			continue
		}
		listeners[name] = listener
	}
	return listeners, nil
}
