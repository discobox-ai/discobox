package wslc

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// TestBridgeHalfCloseE2E verifies both half-close directions across the wslc
// bridge, because every socket this provider uses is relayed through it and a
// half-close bug there is silent until it deadlocks a request:
//
//   - host -> guest: after CloseWrite, the guest must see EOF on its read side
//     and the target must still be able to send its reply back.
//
//   - guest -> host: when the target closes, the host's Read must return EOF
//     rather than blocking forever. This is what a one-shot HTTP/1.0-style
//     request (write, then io.ReadAll) depends on.
//
//     $env:DISCOBOX_WSLC_E2E="1"; go test -run TestBridgeHalfClose -v ./providers/wslc/
func TestBridgeHalfCloseE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	driver, err := NewDriver(DriverConfig{CPUCount: 2, MemoryMiB: 2048})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	const poolID = "halfclose-e2e"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	session, err := driver.session(poolID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	t.Run("CloseWriteStillReceivesResponse", func(t *testing.T) {
		conn, err := session.DialGuestUnix(guestDockerSocket)
		if err != nil {
			t.Fatalf("DialGuestUnix: %v", err)
		}
		defer func() { _ = conn.Close() }()

		// HTTP/1.0 with no Connection: keep-alive - dockerd answers and then
		// closes, so the response is delimited only by EOF.
		if _, err := io.WriteString(conn, "GET /_ping HTTP/1.0\r\n\r\n"); err != nil {
			t.Fatalf("write request: %v", err)
		}
		closer, ok := conn.(interface{ CloseWrite() error })
		if !ok {
			t.Fatal("guest connection does not implement CloseWrite; half-close cannot be signaled")
		}
		if err := closer.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}

		// The half-close must not discard the reply still in flight, and the
		// read must terminate at EOF instead of hanging.
		done := make(chan struct{})
		var body []byte
		var readErr error
		go func() {
			defer close(done)
			body, readErr = io.ReadAll(conn)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("read after CloseWrite hung: the target's EOF never reached the host")
		}
		if readErr != nil {
			t.Fatalf("read response: %v", readErr)
		}
		if !strings.Contains(string(body), "200 OK") {
			t.Fatalf("response after half-close = %q, want a 200", truncate(string(body)))
		}
	})

	t.Run("TargetCloseSurfacesEOF", func(t *testing.T) {
		conn, err := session.DialGuestUnix(guestDockerSocket)
		if err != nil {
			t.Fatalf("DialGuestUnix: %v", err)
		}
		defer func() { _ = conn.Close() }()

		if _, err := io.WriteString(conn, "GET /_ping HTTP/1.0\r\n\r\n"); err != nil {
			t.Fatalf("write request: %v", err)
		}
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("drain body: %v", err)
		}

		// dockerd closed after the HTTP/1.0 response; the next read must report
		// EOF promptly rather than blocking on a connection nobody will write to.
		done := make(chan error, 1)
		go func() {
			_, err := reader.Read(make([]byte, 1))
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("read after target close returned data, want EOF")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("read did not surface the target's close: half-close is not propagated host-ward")
		}
	})

	// The Docker client rides this same conn, so a keep-alive request sequence
	// must survive on one connection without a half-close confusing it.
	t.Run("KeepAliveRequestsOnOneConn", func(t *testing.T) {
		lease, err := driver.AcquireDockerClient(ctx, poolID)
		if err != nil {
			t.Fatalf("AcquireDockerClient: %v", err)
		}
		defer lease.Release()
		for i := 0; i < 3; i++ {
			if _, err := lease.Client.Ping(ctx, client.PingOptions{}); err != nil {
				t.Fatalf("ping %d over reused bridge conns: %v", i, err)
			}
		}
	})

}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
