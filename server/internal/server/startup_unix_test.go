//go:build !windows

package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/health"
)

// Only the local IPC endpoint may answer the question: a peer on TCP reaches
// the same handler and must not be able to choose a weaker default for this
// machine.
func TestDefaultProviderChoiceIsTakenOnlyOverTheLocalSocket(t *testing.T) {
	handler := newStartupHandler("starting services")
	go func() {
		_, _ = handler.awaitChoice(t.Context(), health.Choice{Provider: "libkrun", Reason: health.ReasonKVMUnavailable, Alternatives: []string{"docker"}})
	}()
	server := &http.Server{Handler: handler, ConnContext: markLocalIPC, ReadHeaderTimeout: 5 * time.Second}
	t.Cleanup(func() { _ = server.Close() })

	socket := testSocketPath(t)
	unixListener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	tcpListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(unixListener) }()
	go func() { _ = server.Serve(tcpListener) }()
	for deadline := time.Now().Add(5 * time.Second); !probeStartup(t, handler).NeedsChoice(); {
		if time.Now().After(deadline) {
			t.Fatal("startup never held")
		}
		time.Sleep(10 * time.Millisecond)
	}

	post := func(client *http.Client, url string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+health.SetupDefaultProviderPath, strings.NewReader(`{"provider":"docker"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(http.DefaultClient, "http://"+tcpListener.Addr().String()); code != http.StatusServiceUnavailable {
		t.Fatalf("a choice over TCP answered %d, want 503", code)
	}
	unixClient := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	if code := post(unixClient, "http://local"); code != http.StatusAccepted {
		t.Fatalf("a choice over the Unix socket answered %d, want 202", code)
	}
}
