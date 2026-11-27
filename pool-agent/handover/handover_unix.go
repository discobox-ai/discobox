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

// DrainRequested is closed when this process is told to drain: systemd's
// `kill --signal=SIGUSR2`, which poolunits sends to the old slot once the new
// one is up. It is never closed if ctx ends first.
func DrainRequested(ctx context.Context) <-chan struct{} {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR2)
	drain := make(chan struct{})
	go func() {
		defer signal.Stop(signals)
		select {
		case <-signals:
			close(drain)
		case <-ctx.Done():
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
