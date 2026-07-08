package shim

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/shimproxy"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"
)

const testProtocol = "discobox-agent-terminal"

// TestReplayThroughStack reproduces the end-to-end attach path minus auth: a real
// shim, the sandbox-agent AttachHTTPUpgrade handler, an httputil.ReverseProxy in
// front (as the worker/control-plane hops), and an http.Client attach like the
// CLI. It asserts the whole recorded history survives every hop.
func TestReplayThroughStack(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	socketPath := dir + "/shim.sock"
	go func() {
		_ = Run(ctx, Config{
			TerminalID:  "agt_test",
			AgentID:     "test",
			Command:     []string{"sh", "-c", "seq 1 2000; sleep 8"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: dir + "/runtime.json",
			LogDir:      dir + "/logs",
			Rows:        24,
			Cols:        80,
		})
	}()

	// Origin: the sandbox-agent attach handler.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replay := r.URL.Query().Get("replay") == "true"
		_ = shimproxy.AttachHTTPUpgrade(r.Context(), w, socketPath, testProtocol, replay)
	}))
	defer origin.Close()

	// Two reverse proxy hops like worker-agent + control-plane.
	originURL, _ := url.Parse(origin.URL)
	mid := httptest.NewServer(httputil.NewSingleHostReverseProxy(originURL))
	defer mid.Close()
	midURL, _ := url.Parse(mid.URL)
	front := httptest.NewServer(httputil.NewSingleHostReverseProxy(midURL))
	defer front.Close()

	// Produce and observe the whole blob first so replay lands after it is recorded.
	plainConn, plainReader := attachTerminal(t, ctx, socketPath, false)
	defer plainConn.Close()
	if _, err := shimproxy.StartJSON[terminal.Terminal](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	want := expectedSeqOutput(2000)
	if live := readOutputBytes(t, plainReader, len(want)); live != want {
		t.Fatalf("plain output mismatch: got %d bytes want %d", len(live), len(want))
	}

	// Attach with replay through the proxy via an http.Client, like the CLI.
	got := clientReplayAttach(t, ctx, front.URL+"/attach?replay=true", len(want))
	if got != want {
		t.Fatalf("replay through stack dropped history:\n got[:80]=%q\nwant[:80]=%q\n(len got=%d want=%d)",
			truncate(got, 80), truncate(want, 80), len(got), len(want))
	}
}

func clientReplayAttach(t *testing.T, ctx context.Context, attachURL string, n int) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, attachURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", testProtocol)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status: %s", resp.Status)
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("upgraded body is not writable")
	}
	defer conn.Close()
	// Mimic the CLI: it writes an initial resize and issues a separate /start
	// request before it begins reading output, so the shim floods replay history
	// while the client is not yet draining.
	_ = frame.Write(conn, frame.Resize, mustResize(t))
	time.Sleep(300 * time.Millisecond)
	// Signal readiness so the shim streams history without waiting out the
	// ready-timeout fallback.
	_ = frame.Write(conn, frame.Ready, nil)
	var out []byte
	for len(out) < n {
		f, err := frame.Read(conn)
		if err != nil {
			t.Fatalf("read frame after %d bytes: %v", len(out), err)
		}
		if f.Type == frame.Output {
			out = append(out, f.Payload...)
		}
	}
	return string(out)
}

func mustResize(t *testing.T) []byte {
	t.Helper()
	payload, err := frame.EncodeResize(80, 24)
	if err != nil {
		t.Fatalf("encode resize: %v", err)
	}
	return payload
}
