package desktop

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The fake voxl is this test binary, run again behind a wrapper script with
// fakeVoxlEnv set: Streamer gives voxl an environment of its own, so the
// switch has to ride in the script rather than be inherited.
const fakeVoxlEnv = "DISCOBOX_TEST_FAKE_VOXL"

func TestMain(m *testing.M) {
	if os.Getenv(fakeVoxlEnv) == "1" {
		os.Exit(fakeVoxl(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeVoxl is enough of voxl to be started, health-checked and streamed
// through: it serves /healthz, /stats to whoever has its token, and on /ws, given voxl's token and no browser
// origin, answers the upgrade and echoes what it is sent. With
// FAKE_VOXL_FAILS=n it exits on its first n starts the way voxl does when a
// cold display refuses its capture, counting starts in FAKE_VOXL_COUNTER.
func fakeVoxl(args []string) int {
	host, port := "", ""
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--addr="); ok {
			host = value
		}
		if value, ok := strings.CutPrefix(arg, "--port="); ok {
			port = value
		}
	}
	if counter := os.Getenv("FAKE_VOXL_COUNTER"); counter != "" {
		data, _ := os.ReadFile(counter)
		starts, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		starts++
		_ = os.WriteFile(counter, []byte(strconv.Itoa(starts)), 0o600)
		fails, _ := strconv.Atoi(os.Getenv("FAKE_VOXL_FAILS"))
		if starts <= fails {
			fmt.Println("video init failed: MIT-SHM attach refused")
			return 1
		}
	}
	token := os.Getenv("VOXL_PASSWORD")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "a browser origin reached voxl", http.StatusForbidden)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
	})
	fmt.Println("serving")
	server := &http.Server{Addr: net.JoinHostPort(host, port), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		fmt.Println(err)
		return 1
	}
	return 0
}

// streamFixture is a viewer whose display answers like an X server that is up
// and whose voxl is the fake above.
type streamFixture struct {
	server  *Server
	origin  *httptest.Server
	counter string
}

func newStreamFixture(t *testing.T, fails int, allowedHosts ...string) *streamFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the desktop runs on Linux")
	}
	// The display's xrandr, on the PATH it is looked up on, answering a query
	// the way the dummy driver does.
	bin := t.TempDir()
	writeScript(t, filepath.Join(bin, "xrandr"), "#!/bin/sh\n"+
		"echo 'Screen 0: minimum 320 x 200, current 2560 x 1408, maximum 8192 x 8192'\n"+
		"echo 'DUMMY0 connected primary 2560x1408+0+0 0mm x 0mm'\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	voxlDir := t.TempDir()
	counter := filepath.Join(t.TempDir(), "starts")
	writeScript(t, filepath.Join(voxlDir, "voxl"), fmt.Sprintf("#!/bin/sh\n%s=1 FAKE_VOXL_FAILS=%d FAKE_VOXL_COUNTER=%s exec %s \"$@\"\n",
		fakeVoxlEnv, fails, counter, self))

	server, err := New(slog.New(slog.DiscardHandler), Config{
		FeedbackDir:  t.TempDir(),
		Display:      ":91",
		ScaleEnvDir:  filepath.Join(t.TempDir(), "scale-env"),
		VoxlDir:      voxlDir,
		StreamAddr:   freeLoopbackAddr(t),
		AllowedHosts: allowedHosts,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(server.streamer.Close)
	origin := httptest.NewServer(server)
	t.Cleanup(origin.Close)
	return &streamFixture{server: server, origin: origin, counter: counter}
}

// starts is how many times the fake voxl has been started.
func (f *streamFixture) starts(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

// upgrade asks for the stream the way a browser on the page does, with the
// given Origin (none when empty), and returns the status and the connection.
func (f *streamFixture) upgrade(t *testing.T, origin string) (int, net.Conn, *bufio.Reader) {
	t.Helper()
	host := strings.TrimPrefix(f.origin.URL, "http://")
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(t.Context(), "tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	request := "GET /api/stream HTTP/1.1\r\nHost: " + host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"
	if origin != "" {
		request += "Origin: " + origin + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read the upgrade's response: %v", err)
	}
	// The body of a 101 is the connection itself, which the caller reads; any
	// other answer's body is read and closed here.
	if response.StatusCode == http.StatusSwitchingProtocols {
		return response.StatusCode, conn, reader
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	t.Logf("upgrade answered %d: %s", response.StatusCode, body)
	return response.StatusCode, conn, reader
}

// The stream is the page's own origin all the way to voxl: the upgrade the
// page makes reaches voxl's websocket, carrying the token only the viewer
// knows and not the browser's origin, and what is sent comes back through it.
func TestThePagesStreamReachesVoxl(t *testing.T) {
	f := newStreamFixture(t, 0)
	status, conn, reader := f.upgrade(t, f.origin.URL)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", status)
	}
	if _, err := io.WriteString(conn, "hello voxl\n"); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil || line != "hello voxl\n" {
		t.Fatalf("echo = %q, %v", line, err)
	}
	if got := f.starts(t); got != 1 {
		t.Fatalf("voxl started %d times, want 1", got)
	}

	// A second viewer shares the running voxl rather than starting another.
	if status, _, _ := f.upgrade(t, f.origin.URL); status != http.StatusSwitchingProtocols {
		t.Fatalf("second viewer: status = %d", status)
	}
	if got := f.starts(t); got != 1 {
		t.Fatalf("a second viewer started voxl again: %d starts", got)
	}
}

// A display that has only just come up can refuse voxl's capture; the next
// start, a moment later, is not refused. The viewer retries it rather than
// failing the page.
func TestAColdRefusalIsRetried(t *testing.T) {
	f := newStreamFixture(t, 2)
	if status, _, _ := f.upgrade(t, f.origin.URL); status != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101 after retries", status)
	}
	if got := f.starts(t); got != 3 {
		t.Fatalf("voxl started %d times, want 3", got)
	}
}

// And retried a bounded number of times: a voxl that cannot start is a
// failure the page shows, not a loop.
func TestAStreamThatCannotStartGivesUp(t *testing.T) {
	f := newStreamFixture(t, 99)
	if status, _, _ := f.upgrade(t, f.origin.URL); status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if got := f.starts(t); got != streamStartAttempts {
		t.Fatalf("voxl started %d times, want %d", got, streamStartAttempts)
	}
}

// voxl exits when its X server goes away. The next stream request starts it
// again; nothing has to restart the viewer.
func TestAStreamThatExitedIsStartedAgain(t *testing.T) {
	f := newStreamFixture(t, 0)
	if _, err := f.server.streamer.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.server.streamer.mu.Lock()
	process := f.server.streamer.current
	f.server.streamer.mu.Unlock()
	if err := process.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-process.done

	if status, _, _ := f.upgrade(t, f.origin.URL); status != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", status)
	}
	if got := f.starts(t); got != 2 {
		t.Fatalf("voxl started %d times, want 2", got)
	}
}

// Reaching the stream's port is not a request to stream, and neither is
// reaching the viewer: only an upgrade starts voxl.
func TestOnlyAStreamRequestStartsVoxl(t *testing.T) {
	f := newStreamFixture(t, 0)
	for _, path := range []string{"/", "/api/session", "/api/stream"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.origin.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if got := f.starts(t); got != 0 {
		t.Fatalf("voxl started %d times without a stream being asked for", got)
	}
}

// A page on another site cannot open the stream, which would let it type on
// the desktop and read its clipboard; nor can it reach the rest of the
// viewer. And voxl is never started for it.
func TestAForeignOriginIsRefused(t *testing.T) {
	f := newStreamFixture(t, 0)
	if status, _, _ := f.upgrade(t, "http://evil.example"); status != http.StatusForbidden {
		t.Fatalf("upgrade from a foreign origin: status = %d, want 403", status)
	}
	// Same host, another port, is another origin.
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(f.origin.URL, "http://"))
	if status, _, _ := f.upgrade(t, "http://"+net.JoinHostPort(host, "1")); status != http.StatusForbidden {
		t.Fatalf("upgrade from another port's origin: status = %d, want 403", status)
	}
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, f.origin.URL+"/api/feedback", strings.NewReader("{}"))
	request.Header.Set("Origin", "https://evil.example")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a write from a foreign origin: status = %d, want 403", response.StatusCode)
	}
	if got := f.starts(t); got != 0 {
		t.Fatalf("voxl was started for a refused request: %d starts", got)
	}
}

// A domain pointed at 127.0.0.1 is same-origin with itself, so the Origin rule
// alone lets it through; the Host it arrives with does not.
func TestAForeignHostIsRefused(t *testing.T) {
	f := newStreamFixture(t, 0, "desktop.example")
	for host, want := range map[string]int{
		"evil.example":         http.StatusForbidden,
		"evil.example:6900":    http.StatusForbidden,
		"localhost:6900":       http.StatusOK,
		"127.0.0.1:43123":      http.StatusOK,
		"[::1]:6900":           http.StatusOK,
		"desktop.example":      http.StatusOK,
		"DESKTOP.example:8443": http.StatusOK,
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.origin.URL+"/api/session", nil)
		request.Host = host
		request.Header.Set("Origin", "http://"+host)
		response, err := http.DefaultClient.Do(request)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want {
			t.Errorf("Host %s: status = %d, want %d", host, response.StatusCode, want)
		}
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}
