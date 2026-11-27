package server

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// A draining front accepts nothing more, and carries an upgraded stream it
// holds, an exec's or a terminal's, until the stream ends; only then does its
// drain end (ADR 26-11-26-536 §3).
func TestADrainingFrontCarriesItsStreamsToTheirEnd(t *testing.T) {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	front, err := NewFront(slog.New(slog.DiscardHandler), listener, Identity{ProjectID: "project-1", PoolID: "pool-1"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	front.Start()
	t.Cleanup(func() { _ = front.server.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A stream the way the sandbox proxy relays one: hijacked, and held by
	// its handler for as long as it runs.
	go func() {
		_ = front.serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, buffered, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: exec\r\n\r\n")
			for {
				line, err := buffered.ReadString('\n')
				if err != nil {
					return
				}
				_, _ = io.WriteString(conn, line)
			}
		}))
	}()

	var dialer net.Dialer
	stream, err := dialer.DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(stream, "GET /exec HTTP/1.1\r\nHost: pool\r\nConnection: Upgrade\r\nUpgrade: exec\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stream)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", resp.StatusCode)
	}
	echo := func(line string) {
		t.Helper()
		if _, err := io.WriteString(stream, line+"\n"); err != nil {
			t.Fatalf("write to the stream: %v", err)
		}
		got, err := reader.ReadString('\n')
		if err != nil || got != line+"\n" {
			t.Fatalf("the stream echoed %q, %v; want %q", got, err, line)
		}
	}
	echo("before")

	front.StopAccepting()
	drained := make(chan error, 1)
	go func() { drained <- front.Drain(ctx) }()
	refused, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if conn, err := dialer.DialContext(refused, "tcp", listener.Addr().String()); err == nil {
		_ = conn.Close()
		t.Fatal("a draining front's listener still accepted a connection")
	}
	echo("during the drain")
	select {
	case err := <-drained:
		t.Fatalf("the drain ended with a stream still open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	_ = stream.Close()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain = %v, want nil once the stream ended", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not end with the stream")
	}
}
