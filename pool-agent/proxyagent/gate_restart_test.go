package proxyagent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/x/shorttmp"
)

func TestTheGateWaitsForTheControlPlaneSocketToReturn(t *testing.T) {
	root := withTestRoot(t)
	socket := filepath.Join(shorttmp.Dir(t), "server.sock")
	endpoint := "unix://" + socket
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	// A server exit unlinks its socket. The replacement binds the same path.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envControlPlaneURL, endpoint)
	if err := WriteResolveContext(root, testProjectID, testPoolID, endpoint, "pool-token"); err != nil {
		t.Fatal(err)
	}
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	resolver.gateClient = gateHTTPClient()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	restarted := make(chan struct{})
	go func() {
		defer close(restarted)
		time.Sleep(50 * time.Millisecond)
		replacement, err := config.Listen(ctx, "unix", socket)
		if err != nil {
			t.Error(err)
			return
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "restarted")
		}), ReadHeaderTimeout: time.Second}
		defer server.Close()
		go func() { _ = server.Serve(replacement) }()
		<-ctx.Done()
	}()
	defer func() { cancel(); <-restarted }()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "https://"+GateHost+"/projects/default/sandboxes", nil)
	admitted, err := resolver.Gate(ctx, proxy.SecretGateRequest{ClientID: "sb-1", Request: req})
	if err != nil {
		t.Fatalf("gate failed during a brief server restart instead of waiting for the socket: %v", err)
	}
	defer admitted.Response.Body.Close()
	body, err := io.ReadAll(admitted.Response.Body)
	if err != nil || admitted.Response.StatusCode != http.StatusOK || string(body) != "restarted" {
		t.Fatalf("response = %d %q, %v; want 200 restarted", admitted.Response.StatusCode, body, err)
	}
}
