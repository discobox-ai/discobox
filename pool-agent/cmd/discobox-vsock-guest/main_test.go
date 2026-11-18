package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	guestvsock "github.com/discobox-ai/discobox/pkg/vsock"
)

func TestLifecycleAcceptsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- serveLifecycle(ctx, listener, func(context.Context) error {
			calls.Add(1)
			return nil
		})
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+listener.Addr().String()+"/shutdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown status = %d", response.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("shutdown calls = %d, want 1", calls.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve lifecycle: %v", err)
	}
}

func TestDockerProxyServesFramedConnectionsWhenTheKernelAsks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The Docker socket stand-in echoes a line back.
	// A short directory: t.TempDir() carries the test's name, which takes a
	// Unix socket's path past its limit on CI's runners.
	dir, err := os.MkdirTemp("", "vg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	docker, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	go func() {
		for {
			conn, err := docker.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadString('\n')
				_, _ = io.WriteString(conn, line)
			}()
		}
	}()

	for _, tc := range []struct {
		name    string
		cmdline string
		framed  bool
	}{
		{"framed", "console=hvc0 root=/dev/vda ro " + guestvsock.FramingKernelParameter + " panic=5", true},
		{"plain", "console=hvc0 root=/dev/vda ro panic=5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listener := frameIfAsked(inner, tc.cmdline)
			defer listener.Close()
			go func() { _ = serveUnixProxy(ctx, listener, socket) }()

			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", inner.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if tc.framed {
				if conn, err = guestvsock.FrameDialed(conn); err != nil {
					t.Fatal(err)
				}
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.WriteString(conn, "ping\n"); err != nil {
				t.Fatal(err)
			}
			// The stand-in closes its side once it has answered, as dockerd
			// does when an attached container exits: the host reads the
			// answer and then EOF, never a connection left hanging.
			got, err := io.ReadAll(conn)
			if err != nil || string(got) != "ping\n" {
				t.Fatalf("read to EOF = %q, %v; want %q then EOF", got, err, "ping\n")
			}
		})
	}
}
