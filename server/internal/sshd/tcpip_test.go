package sshd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// fakeTCPAttachAgent stands in for the sandbox-agent's /tcp/attach endpoint.
// It echoes Input frames back as Stdout, and refuses the upgrade whenever
// host/port match refuseHost/refusePort — standing in for a dial failure on
// the real endpoint.
type fakeTCPAttachAgent struct {
	refuseHost, refusePort string
	gotHost, gotPort       string
}

func (f *fakeTCPAttachAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.gotHost = r.URL.Query().Get("host")
	f.gotPort = r.URL.Query().Get("port")
	if f.gotHost == f.refuseHost && f.gotPort == f.refusePort {
		http.Error(w, "connection refused", http.StatusBadGateway)
		return
	}
	wsConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	netConn := websocket.NetConn(r.Context(), wsConn, websocket.MessageBinary)
	defer netConn.Close()
	for {
		f2, err := frame.Read(netConn)
		if err != nil {
			return
		}
		if f2.Type == frame.Input {
			if werr := frame.Write(netConn, frame.Stdout, f2.Payload); werr != nil {
				return
			}
		}
	}
}

func TestDirectTCPIPTunnelRoundTrip(t *testing.T) {
	agent := &fakeTCPAttachAgent{refuseHost: "refused.example", refusePort: "9999"}
	agentServer := httptest.NewServer(agent)
	defer agentServer.Close()

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
	defer client.Close()

	conn, err := client.Dial("tcp", "example.com:4321")
	if err != nil {
		t.Fatalf("direct-tcpip dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := readFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want %q", buf, "ping")
	}

	if agent.gotHost != "example.com" || agent.gotPort != "4321" {
		t.Fatalf("agent saw host=%q port=%q, want example.com/4321", agent.gotHost, agent.gotPort)
	}
}

func TestDirectTCPIPRejectedBeforeAcceptOnDialFailure(t *testing.T) {
	agent := &fakeTCPAttachAgent{refuseHost: "refused.example", refusePort: "9999"}
	agentServer := httptest.NewServer(agent)
	defer agentServer.Close()

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
	defer client.Close()

	if _, err := client.Dial("tcp", "refused.example:9999"); err == nil {
		t.Fatalf("expected the direct-tcpip channel to be rejected")
	}
}

func TestDirectTCPIPMalformedPayloadRejected(t *testing.T) {
	h := newTestHarness(t)
	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")
	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)

	client, err := h.dial(sandbox.ID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// A direct-tcpip payload too short to unmarshal must be rejected, not
	// crash the connection handler.
	if _, _, err := client.OpenChannel("direct-tcpip", []byte{0x00}); err == nil {
		t.Fatalf("expected a malformed direct-tcpip payload to be rejected")
	}
}

func readFull(conn interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
