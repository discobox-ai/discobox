package pooloutbound

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestRelayCarriesBytesWithoutBootstrapMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	upstream, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() { _ = ServeRelay(ctx, listener, UpstreamDialer("http://"+upstream.Addr().String())) }()
	got := make(chan string, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		data, _ := io.ReadAll(conn)
		got <- string(data)
		_, _ = io.WriteString(conn, "response")
	}()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// Even the old protocol's magic bytes are just traffic now.
	_, _ = io.WriteString(conn, "config\nhttp\nGET / HTTP/1.1\r\n\r\n")
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "response" {
		t.Fatalf("response = %q", response)
	}
	if data := <-got; data != "config\nhttp\nGET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("upstream received %q", data)
	}
}
