package systemd

import (
	"fmt"
	"net"
	"os"
)

// notifySocketEnv is where systemd says a Type=notify unit's process reports
// its state (sd_notify(3)).
const notifySocketEnv = "NOTIFY_SOCKET"

// NotifyReady tells systemd this process has started, as a Type=notify unit's
// main process does once what the units ordered after it read is in place. A
// process systemd does not supervise that way is given no socket, and this
// does nothing.
func NotifyReady() error {
	socket := os.Getenv(notifySocketEnv)
	if socket == "" {
		return nil
	}
	// An abstract socket is named with a leading '@' in the environment and a
	// leading NUL on the wire.
	if socket[0] == '@' {
		socket = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("dial %s: %w", notifySocketEnv, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		return fmt.Errorf("write %s: %w", notifySocketEnv, err)
	}
	return nil
}
