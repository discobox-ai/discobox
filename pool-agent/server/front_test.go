package server

import (
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpchannel"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// Before the agent has a runtime its front answers the control-plane channel
// alone, to a server token carrying the channel's scope; afterwards the
// agent's router answers everything else, and the front the channel still
// (ADR 26-10-30-875 §2).
func TestTheFrontServesTheChannelBeforeTheAgentIsReady(t *testing.T) {
	const projectID, poolID = "project-1", "pool-1"
	publicKey, sign := testPoolTokenSigner(t)
	channel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	var listen net.ListenConfig
	listener, err := listen.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{ProjectID: projectID, PoolID: poolID}
	front, err := StartFront(slog.New(slog.DiscardHandler), listener, identity, publicKey, channel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = front.Shutdown(t.Context()) })
	base := "http://" + listener.Addr().String()

	get := func(path, token string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	channelPath := cpchannel.Path(projectID, poolID)

	if got := get(channelPath, sign(projectID, poolID, "", ScopePoolControlPlaneChannel)); got != http.StatusTeapot {
		t.Fatalf("channel with its scope = %d, want the channel's answer", got)
	}
	if got := get(channelPath, sign(projectID, poolID, "", ScopeSandboxRead)); got != http.StatusForbidden {
		t.Fatalf("channel without its scope = %d, want 403", got)
	}
	if got := get(channelPath, ""); got != http.StatusUnauthorized {
		t.Fatalf("channel with no token = %d, want 401", got)
	}
	if got := get("/healthz", ""); got != http.StatusServiceUnavailable {
		t.Fatalf("healthz before the agent is ready = %d, want 503", got)
	}

	router, err := NewRouter(Config{
		Identity:              identity,
		Runtime:               sandboxruntime.NewMemorySandboxRuntime(),
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = front.serve(t.Context(), router) }()
	deadline := time.Now().Add(5 * time.Second)
	for get("/healthz", "") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("the agent's router never took over the front")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := get(channelPath, sign(projectID, poolID, "", ScopePoolControlPlaneChannel)); got != http.StatusTeapot {
		t.Fatalf("channel once ready = %d, want the channel's answer", got)
	}
}
