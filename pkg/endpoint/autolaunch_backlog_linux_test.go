package endpoint

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// A server too starved even to accept fills its listener's backlog, and Linux
// then refuses a non-blocking connect at once with EAGAIN. Only a listening
// socket has a backlog, so that is a server too busy to answer, not an absent
// one.
func TestProbeReadsAFullBacklogAsABusyServer(t *testing.T) {
	socket := testSocketPath(t)
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: socket}); err != nil {
		t.Fatal(err)
	}
	// A backlog of none, and never an accept: the first connection queued
	// fills it.
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	var dialer net.Dialer
	for i := 0; ; i++ {
		conn, err := dialer.DialContext(t.Context(), "unix", socket)
		if errors.Is(err, syscall.EAGAIN) {
			break
		}
		if err != nil {
			t.Fatalf("fill the backlog: %v", err)
		}
		defer conn.Close()
		if i == 16 {
			t.Fatal("the backlog never filled")
		}
	}
	opts := LaunchOptions{Endpoint: "unix://" + socket, ProbeTimeout: time.Second}
	_, err = probeEndpoint(context.Background(), opts)
	if !errors.Is(err, errProbeUnanswered) || isProbeConnectionError(err) {
		t.Fatalf("probe of a full backlog = %v, want a server too busy to answer", err)
	}
}
