package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/obot-platform/discobox/execstream/frame"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	services "github.com/obot-platform/discobox/server/internal/services"
)

// stubPoolConsole is a console whose "TTY" is a pipe the test drives: what the
// handler writes lands in written, and what the test wants the console to emit
// is fed through output.
type stubPoolConsole struct {
	projectID string
	poolID    string
	openOpts  sandbox.ConsoleOptions

	output chan []byte

	mu       sync.Mutex
	written  []byte
	resizes  [][2]int
	closed   bool
	exitCode int
	exitErr  error
}

func newStubPoolConsole() *stubPoolConsole {
	return &stubPoolConsole{output: make(chan []byte, 8)}
}

func (c *stubPoolConsole) Read(p []byte) (int, error) {
	chunk, ok := <-c.output
	if !ok {
		return 0, io.EOF
	}
	return copy(p, chunk), nil
}

func (c *stubPoolConsole) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, p...)
	return len(p), nil
}

func (c *stubPoolConsole) Resize(_ context.Context, rows, cols int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resizes = append(c.resizes, [2]int{rows, cols})
	return nil
}

func (c *stubPoolConsole) Wait(context.Context) (int, error) { return c.exitCode, c.exitErr }

func (c *stubPoolConsole) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.output)
	}
	return nil
}

func (c *stubPoolConsole) snapshot() (string, [][2]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.written), append([][2]int(nil), c.resizes...)
}

func newPoolConsoleTestServer(t *testing.T, stubs *routerTestServices) *httptest.Server {
	t.Helper()
	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
		Events:         stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func dialPoolConsole(ctx context.Context, t *testing.T, server *httptest.Server, poolID, query string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/projects/" + testDefaultProjectID + "/pools/" + poolID + "/console" + query
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: server.Client()})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial console: %v", err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "done") })
	return conn
}

// The console is a byte pipe with resize: input frames reach the console's TTY,
// its output comes back as stdout frames, and the requested size travels on the
// open so the first prompt is already drawn at the caller's size.
func TestPoolConsoleStreamsFramesBothWays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stubs := newRouterTestServices()
	console := newStubPoolConsole()
	stubs.console = console
	server := newPoolConsoleTestServer(t, stubs)

	conn := dialPoolConsole(ctx, t, server, "pool-1", "?rows=40&cols=100")
	netConn := websocket.NetConn(ctx, conn, websocket.MessageBinary)

	if err := frame.Write(netConn, frame.Input, []byte("uname -a\n")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	resize, err := frame.EncodeResize(120, 50)
	if err != nil {
		t.Fatalf("encode resize: %v", err)
	}
	if err := frame.Write(netConn, frame.Resize, resize); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	console.output <- []byte("Linux pool-host\n")

	got, err := frame.Read(netConn)
	if err != nil {
		t.Fatalf("read output frame: %v", err)
	}
	if got.Type != frame.Stdout || string(got.Payload) != "Linux pool-host\n" {
		t.Fatalf("output frame = %d %q", got.Type, got.Payload)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		written, resizes := console.snapshot()
		if written == "uname -a\n" && len(resizes) == 1 && resizes[0] == [2]int{50, 120} {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console input = %q, resizes = %v", written, resizes)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if console.poolID != "pool-1" || console.projectID != testDefaultProjectID {
		t.Fatalf("console opened for %q/%q", console.projectID, console.poolID)
	}
	if console.openOpts.Rows != 40 || console.openOpts.Cols != 100 {
		t.Fatalf("console open options = %#v", console.openOpts)
	}
}

// A shell that exits ends the session with its status, rather than dropping the
// websocket and leaving the client to guess.
func TestPoolConsoleReportsShellExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stubs := newRouterTestServices()
	console := newStubPoolConsole()
	console.exitCode = 3
	stubs.console = console
	server := newPoolConsoleTestServer(t, stubs)

	conn := dialPoolConsole(ctx, t, server, "pool-1", "")
	netConn := websocket.NetConn(ctx, conn, websocket.MessageBinary)

	_ = console.Close()

	got, err := frame.Read(netConn)
	if err != nil {
		t.Fatalf("read exit frame: %v", err)
	}
	if got.Type != frame.Exit {
		t.Fatalf("frame type = %d, want exit", got.Type)
	}
	exit, err := frame.DecodeExit(got.Payload)
	if err != nil {
		t.Fatalf("decode exit: %v", err)
	}
	if exit.Status != "exited" || exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("exit payload = %#v", exit)
	}
}

// The console is refused before the upgrade, so an operator whose pool host is
// unreachable gets the reason instead of a websocket that closes on its own.
func TestPoolConsoleRejectsBeforeUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stubs := newRouterTestServices()
	stubs.consoleErr = apperrors.NewStatusError(http.StatusNotFound, "pool not found")
	server := newPoolConsoleTestServer(t, stubs)

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/projects/" + testDefaultProjectID + "/pools/missing/console"
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: server.Client()})
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "done")
		t.Fatal("dial console succeeded, want rejection")
	}
	if resp == nil {
		t.Fatalf("dial console error without response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "pool not found") {
		t.Fatalf("body = %q", body)
	}
}
