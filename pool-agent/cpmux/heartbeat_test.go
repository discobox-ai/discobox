package cpmux

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// idleCutConn models an intermediary that closes a quiet direction. Reads on
// both sides must see traffic before the deadline, even with no open streams.
type idleCutConn struct {
	net.Conn
	idle time.Duration
}

func (c idleCutConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func TestHeartbeatsKeepAnIdleChannelOpen(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	host, err := Client(idleCutConn{a, 45 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	guest, err := Server(idleCutConn{b, 45 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	// Production's 30-second heartbeat must beat the intermediary's 45-second
	// cutoff repeatedly, without application traffic to keep it alive.
	time.Sleep(65 * time.Second)
	if host.Closed() || guest.Closed() {
		t.Fatal("idle channel closed despite heartbeats")
	}
	result := make(chan error, 1)
	go func() {
		conn, err := guest.Dial(context.Background(), TargetControlPlane)
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		_, err = io.WriteString(conn, "after idle")
		result <- err
	}()
	conn, target, err := host.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	data, err := io.ReadAll(conn)
	if err != nil || target != TargetControlPlane || string(data) != "after idle" {
		t.Fatalf("after idle: target=%q data=%q error=%v", target, data, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
