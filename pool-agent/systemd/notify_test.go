//go:build linux

package systemd

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNotifyReadyWritesReadyToTheNotifySocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = conn.Close() }()
	t.Setenv(notifySocketEnv, path)

	if err := NotifyReady(); err != nil {
		t.Fatalf("notify: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Fatalf("notified %q, want READY=1", got)
	}
}

func TestNotifyReadyWithoutASocketDoesNothing(t *testing.T) {
	t.Setenv(notifySocketEnv, "")
	if err := NotifyReady(); err != nil {
		t.Fatalf("notify: %v", err)
	}
}
