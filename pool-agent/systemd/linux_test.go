//go:build linux

package systemd

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

// A unit of a machine's systemd says it is ready on the socket systemd gave
// it, which is what lets the proxy's unit be ordered after what the agent
// writes.
func TestNotifyReadyTellsTheSystemdItIsAUnitOf(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "notify")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	t.Setenv(notifySocketEnv, socket)

	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := listener.Read(buf)
	if err != nil {
		t.Fatalf("read the notification: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Errorf("notification = %q, want READY=1", got)
	}
}

// A pool container's agent is PID 1 under no systemd, and has no one to tell.
func TestNotifyReadyWithNoSystemdAboveDoesNothing(t *testing.T) {
	t.Setenv(notifySocketEnv, "")
	if err := NotifyReady(); err != nil {
		t.Errorf("NotifyReady with no socket: %v", err)
	}
}

// A socket that is named and not there is an error, not a silent start: the
// units ordered after the agent would wait for a word that never comes.
func TestNotifyReadyReportsASocketItCannotReach(t *testing.T) {
	t.Setenv(notifySocketEnv, filepath.Join(t.TempDir(), "absent"))
	if err := NotifyReady(); err == nil {
		t.Error("NotifyReady reached a socket that does not exist")
	}
}
