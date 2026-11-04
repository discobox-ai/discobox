package shimproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/x/shorttmp"
)

type halfCloseProxyHarness struct {
	client   *net.TCPConn
	reader   *bufio.Reader
	proxyErr <-chan error
	shimErr  <-chan error
}

// halfCloseDeadline bounds every read in a half-close test, so a half-close
// that never arrives fails the test in seconds instead of hanging it until
// go test's timeout. Every failure there reports the shim's and the proxy's
// results (ends), so a shim that timed out waiting for EOF reads as that and
// not as output the proxy dropped.
const halfCloseDeadline = 10 * time.Second

// requireUnixHalfClose skips a test that waits for a Unix socket peer's
// half-close. On Windows a read pending on a socket while its peer shuts it
// down can stall instead of returning EOF (go.dev/issue/49352; Go's own
// TestCloseWrite sleeps around it), which hung these tests until go test's
// timeout (#473). That is not Windows being out of scope: a Windows build's
// Supervisor runs the same shim on a Unix socket, so an attach's half-close
// there is untested and known to stall, and a Windows attach that relies on
// it would not tear down.
func requireUnixHalfClose(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("untested on Windows: a read pending on a Windows socket can miss its peer's half-close (go.dev/issue/49352)")
	}
}

func newHalfCloseProxyHarness(t *testing.T, shim func(*net.UnixConn, *bufio.Reader) error) halfCloseProxyHarness {
	t.Helper()
	requireUnixHalfClose(t)
	socketPath := filepath.Join(shorttmp.Dir(t), "shim.sock")
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
		_ = conn.SetDeadline(time.Now().Add(halfCloseDeadline))
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
		// The fake shim takes one attach. Anything else reaching this
		// listener, such as the sandbox-agent's port probe (REVIEW.md), must
		// not spend it.
		if r.Method != http.MethodPost || r.URL.Path != "/attach" {
			http.NotFound(w, r)
			return
		}
		proxyErr <- AttachHTTPUpgrade(r.Context(), w, socketPath, "test-exec", true)
	}))
	t.Cleanup(server.Close)

	address := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.DialTCP("tcp", nil, mustTCPAddr(t, address))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(halfCloseDeadline)); err != nil {
		t.Fatal(err)
	}
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
		shimReceived <- payload
		if err != nil {
			return err
		}
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
		t.Fatalf("read after client half-close: %v (%s)", err, harness.ends())
	}
	if string(output) != "from-shim" {
		t.Fatalf("output after client half-close = %q, want from-shim (%s)", output, harness.ends())
	}
	if got := string(<-shimReceived); got != "from-client" {
		t.Fatalf("shim input = %q, want from-client (%s)", got, harness.ends())
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
		shimReceived <- payload
		return err
	})

	output, err := io.ReadAll(harness.reader)
	if err != nil {
		t.Fatalf("read before shim half-close: %v (%s)", err, harness.ends())
	}
	if string(output) != "from-shim" {
		t.Fatalf("output before shim half-close = %q, want from-shim (%s)", output, harness.ends())
	}
	if _, err := harness.client.Write([]byte("from-client")); err != nil {
		t.Fatalf("write after shim half-close: %v (%s)", err, harness.ends())
	}
	if err := harness.client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if got := string(<-shimReceived); got != "from-client" {
		t.Fatalf("shim input after its half-close = %q, want from-client (%s)", got, harness.ends())
	}
	waitHalfCloseResult(t, "proxy", harness.proxyErr)
	waitHalfCloseResult(t, "shim", harness.shimErr)
}

// ends says how the shim and the proxy have finished so far, without waiting,
// for a failure message. Only a failing test calls it: it consumes what it
// reads.
func (h halfCloseProxyHarness) ends() string {
	return "shim: " + peekResult(h.shimErr) + ", proxy: " + peekResult(h.proxyErr)
}

func peekResult(result <-chan error) string {
	select {
	case err := <-result:
		if err != nil {
			return err.Error()
		}
		return "finished"
	default:
		return "still running"
	}
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

// A shim may accept a connection but stall before headers or in its JSON body.
// Both must return so autostop can eventually honor its working ceiling.
func TestStatusBoundsTheWholeExchange(t *testing.T) {
	for _, body := range []string{"", "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n{"} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			socket := filepath.Join(shorttmp.Dir(t), "status.sock")
			listenConfig := net.ListenConfig{}
			listener, err := listenConfig.Listen(context.Background(), "unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			release := make(chan struct{})
			defer close(release)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = http.ReadRequest(bufio.NewReader(conn))
				_, _ = io.WriteString(conn, body)
				<-release
			}()
			result := make(chan error, 1)
			go func() { _, err := StatusJSON[map[string]any](context.Background(), socket); result <- err }()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("stalled status succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("status exchange exceeded its timeout")
			}
		})
	}
}
