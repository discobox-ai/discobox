package shimproxy

import (
	"bufio"
	"bytes"
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

	"github.com/coder/websocket"
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

// testPingInterval and testPingTimeout are a keepalive fast enough that a
// test's pause outlasts many unanswered pings in seconds rather than the
// minutes the real timing takes.
const (
	testPingInterval = 300 * time.Millisecond
	testPingTimeout  = 200 * time.Millisecond
)

// testKeepalivePause outlasts the test keepalive many times, and the five
// seconds the websocket gives a ping or pong to take its writer and be
// written, which a paused client holds behind a pending output write.
const testKeepalivePause = 8 * time.Second

// websocketAttach serves one websocket attach, with the test keepalive, of a shim
// that runs shim once the attach is upgraded, and dials it. proxyErr has the
// attach's result once it ends.
func websocketAttach(t *testing.T, shim func(*net.UnixConn, *bufio.Reader)) (ws *websocket.Conn, proxyErr <-chan error) {
	t.Helper()
	requireUnixHalfClose(t)
	socketPath := filepath.Join(shorttmp.Dir(t), "shim.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_ = req.Body.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test-exec\r\n\r\n")
		shim(conn, reader)
	}()

	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result <- AttachWebSocket(r.Context(), w, r, socketPath, "test-exec", false, testPingInterval, testPingTimeout)
	}))
	t.Cleanup(server.Close)

	ws, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(-1)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws, result
}

// A websocket attach whose client pauses reading, long enough for the agent's
// output write to back up past a keepalive ping, must hold the exec back the
// way a pipe does and deliver every byte once the client reads again, not
// end the attach because the ping went unanswered (#1408).
func TestAttachWebSocketHoldsAPausedClient(t *testing.T) {
	const outputSize = 8 << 20
	ws, _ := websocketAttach(t, func(conn *net.UnixConn, _ *bufio.Reader) {
		_, _ = conn.Write(bytes.Repeat([]byte("x"), outputSize))
		_, _ = io.WriteString(conn, "END")
		_ = conn.CloseWrite()
	})

	// The client's consumer pauses: nothing reads the socket. Its own
	// keepalive goes on pinging the agent, as the CLI's does, so the agent
	// has pongs to answer behind its pending output.
	pingClient(t, ws, testKeepalivePause)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	output, err := io.ReadAll(websocket.NetConn(ctx, ws, websocket.MessageBinary))
	if len(output) != outputSize+len("END") || !bytes.HasSuffix(output, []byte("END")) {
		t.Fatalf("read %d bytes (err %v) after a %s pause, want %d ending in END: the agent ended the attach",
			len(output), err, testKeepalivePause, outputSize+len("END"))
	}
}

// pingClient pings the agent from the client every testPingInterval for d,
// as the CLI's keepalive does, without reading: the pongs are left unread.
func pingClient(t *testing.T, ws *websocket.Conn, d time.Duration) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		ctx, cancel := context.WithTimeout(t.Context(), testPingInterval)
		_ = ws.Ping(ctx)
		cancel()
	}
}

// An attach whose shim is not reading its input, with the client's input
// backed up behind it, leaves the agent not reading the client, so the
// client's pongs go unread: that holds the attach back too, and is not a
// dead client.
func TestAttachWebSocketHoldsAnInputBackedUpBehindTheShim(t *testing.T) {
	const inputSize = 8 << 20
	ws, _ := websocketAttach(t, func(conn *net.UnixConn, reader *bufio.Reader) {
		// The command reads no input until the pause is over.
		time.Sleep(testKeepalivePause)
		n, _ := io.Copy(io.Discard, io.LimitReader(reader, inputSize))
		_, _ = fmt.Fprintf(conn, "read %d", n)
		_ = conn.CloseWrite()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	go func() { _, _ = client.Write(bytes.Repeat([]byte("i"), inputSize)) }()

	output, err := io.ReadAll(client)
	if want := fmt.Sprintf("read %d", inputSize); string(output) != want {
		t.Fatalf("got %q (err %v), want %q: the agent ended the attach while its shim was not reading", output, err, want)
	}
}

// A client that stops answering while its attach is idle (a dead peer: no
// output is pending and the agent is waiting on the client) is found by the
// keepalive, which ends the attach and closes the shim's end.
func TestAttachWebSocketEndsAnIdleAttachWhoseClientIsGone(t *testing.T) {
	shimEOF := make(chan struct{})
	_, proxyErr := websocketAttach(t, func(_ *net.UnixConn, reader *bufio.Reader) {
		// A command that is running and quiet; the attach is idle until the
		// agent hangs up.
		_, _ = io.Copy(io.Discard, reader)
		close(shimEOF)
	})
	// The client never reads again, so it answers no ping: to the agent it
	// is gone.
	deadline := time.After(testPingInterval + testPingTimeout + 5*time.Second)
	select {
	case err := <-proxyErr:
		if err != nil {
			t.Fatalf("attach ended with %v", err)
		}
	case <-deadline:
		t.Fatal("the attach of a client that answers no ping was not ended")
	}
	select {
	case <-shimEOF:
	case <-time.After(5 * time.Second):
		t.Fatal("the attach ended without closing the shim's end")
	}
}
