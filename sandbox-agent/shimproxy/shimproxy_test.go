package shimproxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// socketDir returns a temporary directory short enough to hold a Unix socket.
//
// A socket path cannot exceed 108 bytes, and t.TempDir() spends that twice: it
// roots at $TMPDIR — inside the workspace under `nix develop`, /var/folders/<...>
// on macOS — and then appends the test's own name, which in this package runs
// past fifty characters on its own.
func socketDir(t *testing.T) string {
	t.Helper()
	root := ""
	if runtime.GOOS != "windows" {
		root = "/tmp"
	}
	dir, err := os.MkdirTemp(root, "shimproxy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type halfCloseProxyHarness struct {
	client   *net.TCPConn
	reader   *bufio.Reader
	proxyErr <-chan error
	shimErr  <-chan error
}

func newHalfCloseProxyHarness(t *testing.T, shim func(*net.UnixConn, *bufio.Reader) error) halfCloseProxyHarness {
	t.Helper()
	socketPath := filepath.Join(socketDir(t), "shim.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	shimErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			shimErr <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			shimErr <- err
			return
		}
		_ = req.Body.Close()
		if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test-exec\r\n\r\n"); err != nil {
			shimErr <- err
			return
		}
		shimErr <- shim(conn, reader)
	}()

	proxyErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyErr <- AttachHTTPUpgrade(r.Context(), w, socketPath, "test-exec", true)
	}))
	t.Cleanup(server.Close)

	address := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.DialTCP("tcp", nil, mustTCPAddr(t, address))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/attach", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "test-exec")
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("proxy status = %s", resp.Status)
	}
	return halfCloseProxyHarness{
		client:   conn,
		reader:   reader,
		proxyErr: proxyErr,
		shimErr:  shimErr,
	}
}

func mustTCPAddr(t *testing.T, address string) *net.TCPAddr {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestAttachHTTPUpgradePreservesShimOutputAfterClientHalfClose(t *testing.T) {
	shimReceived := make(chan []byte, 1)
	harness := newHalfCloseProxyHarness(t, func(conn *net.UnixConn, reader *bufio.Reader) error {
		payload, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		shimReceived <- payload
		if _, err := conn.Write([]byte("from-shim")); err != nil {
			return err
		}
		return conn.CloseWrite()
	})

	if _, err := harness.client.Write([]byte("from-client")); err != nil {
		t.Fatal(err)
	}
	if err := harness.client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(harness.reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "from-shim" {
		t.Fatalf("output after client half-close = %q, want from-shim", output)
	}
	if got := string(<-shimReceived); got != "from-client" {
		t.Fatalf("shim input = %q, want from-client", got)
	}
	waitHalfCloseResult(t, "proxy", harness.proxyErr)
	waitHalfCloseResult(t, "shim", harness.shimErr)
}

func TestAttachHTTPUpgradePreservesClientInputAfterShimHalfClose(t *testing.T) {
	shimReceived := make(chan []byte, 1)
	harness := newHalfCloseProxyHarness(t, func(conn *net.UnixConn, reader *bufio.Reader) error {
		if _, err := conn.Write([]byte("from-shim")); err != nil {
			return err
		}
		if err := conn.CloseWrite(); err != nil {
			return err
		}
		payload, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		shimReceived <- payload
		return nil
	})

	output, err := io.ReadAll(harness.reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "from-shim" {
		t.Fatalf("output before shim half-close = %q, want from-shim", output)
	}
	if _, err := harness.client.Write([]byte("from-client")); err != nil {
		t.Fatalf("write after shim half-close: %v", err)
	}
	if err := harness.client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := string(<-shimReceived); got != "from-client" {
		t.Fatalf("shim input after its half-close = %q, want from-client", got)
	}
	waitHalfCloseResult(t, "proxy", harness.proxyErr)
	waitHalfCloseResult(t, "shim", harness.shimErr)
}

func waitHalfCloseResult(t *testing.T, name string, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s error: %v", name, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not finish after both directions half-closed", name)
	}
}
