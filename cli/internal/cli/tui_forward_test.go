package cli

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/obot-platform/discobox/execstream/frame"
)

// forwardTestServer answers the two requests a launcher forward makes: what the
// sandbox is listening on, and the tunnel onto one of those ports. The tunnel
// echoes, so a test can prove bytes went through the local port it was given.
func forwardTestServer(t *testing.T, sandboxPort int) *httptest.Server {
	t.Helper()
	ports := `[{"port":` + strconv.Itoa(sandboxPort) +
		`,"addresses":["127.0.0.1"],"protocol":"http","firstSeenAt":"2026-08-18T00:00:00Z"}]`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tcp/attach"):
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "done")
			conn.SetReadLimit(-1)
			stream := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
			for {
				read, err := frame.Read(stream)
				if err != nil {
					return
				}
				if read.Type == frame.Input {
					if err := frame.Write(stream, frame.Stdout, read.Payload); err != nil {
						return
					}
				}
			}
		case r.URL.Path == "/projects/project-1/sandboxes/sandbox-1":
			w.Header().Set("Content-Type", "application/json")
			sandbox := testSandboxJSON("sandbox-1", "alpha", "2026-08-18T00:00:00Z", "2026-08-18T00:00:01Z")
			// The listening ports ride on the runtime's agent status, which is
			// where the agent's own poller puts them (ADR 0046).
			sandbox = strings.Replace(sandbox, `"observedGeneration":1`,
				`"observedGeneration":1,"agentStatus":{"ports":`+ports+`}`, 1)
			_, _ = w.Write([]byte(sandbox))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// The launcher's forward is the same forwarder disco proxy runs: it follows
// what the sandbox announces, binds a local port for it, wakes the window, and
// carries bytes.
func TestLauncherForwardBindsAndCarriesBytes(t *testing.T) {
	// A port that is almost certainly free, so the binding lands on the
	// sandbox's own number and the assertion is about the mapping rather than
	// about what else this machine happens to be running.
	const sandboxPort = 47311
	server := forwardTestServer(t, sandboxPort)
	app := &App{serverURL: server.URL, noStart: true}
	client, err := app.apiClient()
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	source := &apiDataSource{app: app, client: client, projectID: "project-1"}

	forward, err := source.Forward(t.Context(), "sandbox-1")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer forward.Close()

	select {
	case <-forward.Events():
	case <-time.After(10 * time.Second):
		t.Fatal("the forward never reported a binding")
	}

	bindings := forward.Bindings()
	if len(bindings) != 1 || bindings[0].Port != sandboxPort {
		t.Fatalf("bindings = %#v, want one for %d", bindings, sandboxPort)
	}
	local := bindings[0].Local

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		t.Fatalf("dial the forwarded port: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "through"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "through" {
		t.Fatalf("echo = %q, want through", got)
	}

	// Closing releases the local port and the window's wake-up channel, which
	// is what lets the workspace's event pump stop rather than leak.
	if err := forward.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, ok := <-forward.Events(); ok {
		t.Fatal("a closed forward should close its event channel")
	}
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		t.Fatalf("local port %d was not released: %v", local, err)
	}
	_ = listener.Close()
}
