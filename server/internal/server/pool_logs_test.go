package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// stubPoolLog is a pool host log the test feeds a chunk at a time, so a
// followed stream can be observed arriving rather than only in full.
type stubPoolLog struct {
	projectID string
	poolID    string
	openOpts  sandbox.PoolLogOptions
	source    string

	output chan string

	mu     sync.Mutex
	closed bool
}

func newStubPoolLog(source string) *stubPoolLog {
	return &stubPoolLog{source: source, output: make(chan string, 8)}
}

func (l *stubPoolLog) Read(p []byte) (int, error) {
	chunk, ok := <-l.output
	if !ok {
		return 0, io.EOF
	}
	return copy(p, chunk), nil
}

func (l *stubPoolLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.output)
	}
	return nil
}

func newPoolLogsTestServer(t *testing.T, stubs *routerTestServices) *httptest.Server {
	t.Helper()
	router, err := NewRouter(services.Services{
		Projects:       stubs,
		HarnessConfigs: stubs,
		Sandboxes:      stubs,
		Providers:      stubs,
		Pools:          stubs,
		Jobs:           stubs,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func getPoolLogs(ctx context.Context, t *testing.T, server *httptest.Server, poolID, query string) *http.Response {
	t.Helper()
	url := server.URL + "/api/projects/" + testDefaultProjectID + "/pools/" + poolID + "/logs" + query
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := server.Client().Do(req) //nolint:bodyclose // Closed by the caller, which reads the streamed body first.
	if err != nil {
		t.Fatalf("get pool logs: %v", err)
	}
	return resp
}

// The route streams the driver's bytes through, names its source on the
// response, and passes the requested tail and follow down to the provider.
func TestPoolLogsRouteStreamsSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stubs := newRouterTestServices()
	log := newStubPoolLog("vz guest serial console")
	stubs.poolLog = log
	server := newPoolLogsTestServer(t, stubs)

	log.output <- "[    0.000000] Linux version 6.6\n"
	resp := getPoolLogs(ctx, t, server, "pool-1", "?tail=200&follow=true")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(poolLogSourceHeader); got != "vz guest serial console" {
		t.Fatalf("%s = %q", poolLogSourceHeader, got)
	}

	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if !strings.Contains(first, "Linux version 6.6") {
		t.Fatalf("first line = %q", first)
	}

	// The second chunk is written only after the first was read, so arriving at
	// all proves the response is flushed rather than buffered to completion.
	log.output <- "[    1.500000] docker: starting\n"
	second, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read second line: %v", err)
	}
	if !strings.Contains(second, "docker: starting") {
		t.Fatalf("second line = %q", second)
	}

	if log.openOpts.Tail != 200 || !log.openOpts.Follow {
		t.Fatalf("open options = %+v, want tail 200 and follow", log.openOpts)
	}
	if log.projectID != testDefaultProjectID || log.poolID != "pool-1" {
		t.Fatalf("logs opened for %q/%q", log.projectID, log.poolID)
	}
	_ = log.Close()
}

// A backend that keeps no host log is a settled answer about the provider, so
// it must reach the operator as a status with its reason, not as an empty
// stream that reads like a host with nothing to say.
func TestPoolLogsRouteReportsUnsupportedBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stubs := newRouterTestServices()
	stubs.poolLogErr = apperrors.NewStatusError(http.StatusNotImplemented,
		"pool host logs are not available from this backend: the Docker daemon at tcp://10.0.0.5:2375 is not this machine's")
	server := newPoolLogsTestServer(t, stubs)

	resp := getPoolLogs(ctx, t, server, "pool-1", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "not this machine's") {
		t.Fatalf("body = %q, want the driver's reason", body)
	}
}

// A tail that is not a positive number means "no bound" rather than an error:
// the flag is a convenience over a whole log, and refusing the request would
// withhold the log over a typo.
func TestPoolLogTailIgnoresUnusableValues(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{value: "", want: 0},
		{value: "abc", want: 0},
		{value: "0", want: 0},
		{value: "-5", want: 0},
		{value: "200", want: 200},
		{value: "999999999", want: maxPoolLogTail},
	} {
		if got := poolLogTail(tc.value); got != tc.want {
			t.Fatalf("poolLogTail(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// Closing the stream is what stops a driver's follow, so the handler must do it
// when the client goes away rather than leaving a journalctl running on the
// pool host for every abandoned request.
func TestPoolLogsRouteClosesStreamWhenClientLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stubs := newRouterTestServices()
	log := newStubPoolLog("docker daemon journal (systemd)")
	stubs.poolLog = log
	server := newPoolLogsTestServer(t, stubs)

	log.output <- "waiting\n"
	resp := getPoolLogs(ctx, t, server, "pool-1", "?follow=true")
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("read first line: %v", err)
	}
	cancel()

	deadline := time.After(5 * time.Second)
	for {
		log.mu.Lock()
		closed := log.closed
		log.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("stream was not closed after the client disconnected")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
