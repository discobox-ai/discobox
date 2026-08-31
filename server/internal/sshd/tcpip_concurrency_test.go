package sshd

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// What a forwarded connection is actually asked to do -- many at once, in bulk,
// and half-closed by whichever end finishes first -- had no coverage at all
// until a VS Code Remote-SSH session that would not load sent us measuring the
// live thing by hand. These are the questions that took a day to answer.

// tunnelAgent stands in for the sandbox-agent's /tcp/attach for the traffic
// shapes a single echo does not reach: a payload of any size, and a target that
// finishes sending before the client does.
type tunnelAgent struct {
	// reply is written as Stdout frames as soon as the tunnel opens, before
	// anything arrives, the way a server that speaks first does.
	reply []byte
	// closeAfterReply half-closes the far end once reply is written, leaving
	// the tunnel able to receive.
	closeAfterReply bool
	// echo writes back what it receives.
	echo bool

	mu       sync.Mutex
	opened   int
	received []byte
}

func (a *tunnelAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	netConn := websocket.NetConn(r.Context(), wsConn, websocket.MessageBinary)
	defer netConn.Close()

	a.mu.Lock()
	a.opened++
	a.mu.Unlock()

	if len(a.reply) > 0 {
		for offset := 0; offset < len(a.reply); offset += 32 * 1024 {
			end := min(offset+32*1024, len(a.reply))
			if err := frame.Write(netConn, frame.Stdout, a.reply[offset:end]); err != nil {
				return
			}
		}
	}
	if a.closeAfterReply {
		if err := frame.Write(netConn, frame.CloseOutput, nil); err != nil {
			return
		}
	}
	for {
		read, err := frame.Read(netConn)
		if err != nil {
			return
		}
		switch read.Type {
		case frame.Input:
			a.mu.Lock()
			a.received = append(a.received, read.Payload...)
			a.mu.Unlock()
			if a.echo {
				if err := frame.Write(netConn, frame.Stdout, read.Payload); err != nil {
					return
				}
			}
		case frame.CloseInput:
			if !a.closeAfterReply {
				return
			}
		}
	}
}

// readAllWithin reads to EOF, failing rather than hanging. An SSH channel
// ignores SetReadDeadline, so a regression that never delivers the far end's
// EOF blocks until the whole test binary times out -- which is how the fix
// these tests cover was confirmed, and not how a test should report it.
func readAllWithin(t *testing.T, r io.Reader, within time.Duration) []byte {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- result{data, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("read: %v", res.err)
		}
		return res.data
	case <-time.After(within):
		t.Fatalf("no EOF within %s: the far end finished sending and the channel never said so", within)
		return nil
	}
}

// waitWithin fails if the work is still running after within, for the same
// reason.
func waitWithin(t *testing.T, wg *sync.WaitGroup, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("forwarded connections still unfinished after %s", within)
	}
}

// tunnelHarness wires an SSH client to a server whose sandbox attaches land on
// agent, and returns the client.
func tunnelHarness(t *testing.T, agent http.Handler) *ssh.Client {
	t.Helper()
	agentServer := httptest.NewServer(agent)
	t.Cleanup(agentServer.Close)

	h := newTestHarness(t)
	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")
	h.sandboxes.acquireResult = func() (*services.HTTPClientLease, *model.Sandbox, error) {
		lease := &transport.HTTPClientLease{Client: agentServer.Client(), BaseURL: agentServer.URL}
		return lease, &model.Sandbox{ID: sandbox.ID, ProjectID: acme, PoolID: "pool-1"}, nil
	}
	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)
	client, err := h.dial(sandbox.ID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// One SSH connection carries many forwarded connections at once -- a browser,
// an editor and a language server all at the same time -- and no channel may
// wait on another.
func TestDirectTCPIPChannelsAreConcurrent(t *testing.T) {
	agent := &tunnelAgent{echo: true}
	client := tunnelHarness(t, agent)

	const channels = 16
	var wg sync.WaitGroup
	errs := make(chan error, channels)
	for i := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := client.Dial("tcp", fmt.Sprintf("target-%d.example:80", i))
			if err != nil {
				errs <- fmt.Errorf("channel %d dial: %w", i, err)
				return
			}
			defer conn.Close()
			want := fmt.Appendf(nil, "hello from %d", i)
			if _, err := conn.Write(want); err != nil {
				errs <- fmt.Errorf("channel %d write: %w", i, err)
				return
			}
			got := make([]byte, len(want))
			_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
			if _, err := readFull(conn, got); err != nil {
				errs <- fmt.Errorf("channel %d read: %w", i, err)
				return
			}
			if !bytes.Equal(got, want) {
				errs <- fmt.Errorf("channel %d echoed %q, want %q", i, got, want)
			}
		}()
	}
	waitWithin(t, &wg, 30*time.Second)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if agent.opened != channels {
		t.Fatalf("the agent saw %d tunnels, want %d", agent.opened, channels)
	}
}

// A forwarded connection carries a file, a page, a language server's index --
// far more than fits in one frame or one SSH window.
func TestDirectTCPIPCarriesBulkData(t *testing.T) {
	payload := make([]byte, 4<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	agent := &tunnelAgent{reply: payload, closeAfterReply: true}
	client := tunnelHarness(t, agent)

	conn, err := client.Dial("tcp", "bulk.example:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	got := readAllWithin(t, conn, 60*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("received %d bytes of %d, and they %s", len(got), len(payload),
			map[bool]string{true: "match as far as they go", false: "differ"}[bytes.HasPrefix(payload, got)])
	}
}

// The half that finishes first ends alone. A target that is done sending says
// so, and the client keeps sending -- which is how a request that streams to
// EOF gets its response, and what tearing the channel down instead would cut
// off (ADR 0024 §4).
func TestDirectTCPIPRemoteHalfCloseLeavesTheOtherHalfOpen(t *testing.T) {
	agent := &tunnelAgent{reply: []byte("response first"), closeAfterReply: true}
	client := tunnelHarness(t, agent)

	conn, err := client.Dial("tcp", "half.example:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// The far end's data arrives, then its EOF.
	got := readAllWithin(t, conn, 30*time.Second)
	if string(got) != "response first" {
		t.Fatalf("read %q, want %q", got, "response first")
	}

	// And this side can still send, which is the whole point.
	if _, err := conn.Write([]byte("still writing")); err != nil {
		t.Fatalf("write after the far end's EOF: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		agent.mu.Lock()
		received := string(agent.received)
		agent.mu.Unlock()
		if received == "still writing" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent received %q after the half-close, want %q", received, "still writing")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
