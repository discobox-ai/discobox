package docker

import (
	"context"
	"fmt"
	"strings"
)

// Explaining a daemon this process cannot reach.
//
// The failure that brought this about is the common first run on a fresh
// Docker install: the user is not in the group that owns the socket, so their
// shell reaches the daemon and the server they start does not. What the server
// said was "initialize app: permission denied while trying to connect to the
// docker API at unix:///var/run/docker.sock", which names neither the group
// that would grant it nor the command that adds it.

// explainDaemonUnreachable adds what this machine can say about a daemon the
// client could not reach.
//
// The client's message is all that is left of the refusal — moby's client
// builds it with fmt.Errorf and no %w (client/request.go), so the syscall does
// not survive to be matched on — so this asks the socket itself instead of
// reading that sentence: dialing it reproduces the refusal with the error
// intact, and the socket's own owner and mode say who was entitled to it.
//
// The original error is always the start of what comes back. It is what the
// daemon and the client actually said, and a hint that turns out not to apply
// must not replace it.
func explainDaemonUnreachable(ctx context.Context, host string, err error) error {
	if err == nil {
		return nil
	}
	hint := socketAccessHint(ctx, unixSocketPath(host))
	if hint == "" {
		return err
	}
	return fmt.Errorf("%w (%s)", err, hint)
}

// unixSocketPath is the filesystem path a Docker host names, or "" for a host
// that is not a Unix socket — a TCP daemon, or a Windows named pipe.
func unixSocketPath(host string) string {
	path, ok := strings.CutPrefix(strings.TrimSpace(host), "unix://")
	if !ok {
		return ""
	}
	return path
}
