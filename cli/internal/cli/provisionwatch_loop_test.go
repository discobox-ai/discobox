package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// sandboxJSON is one GET /sandboxes/{id} body, with whatever runtime the test
// wants to hand back on this read.
func sandboxJSON(runtime string) string {
	return fmt.Sprintf(`{"id":"sbx_1","projectId":"project-1","createdByUserId":"user-1",`+
		`"displayName":"watched","config":{"name":"watched","image":""},"runtime":%s,`+
		`"createdAt":"2026-08-19T00:00:00Z","updatedAt":"2026-08-19T00:00:01Z"}`, runtime)
}

// progressRuntime is a runtime mid-provision, reporting a phase as of now.
func progressRuntime(phase string) string {
	return fmt.Sprintf(`{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0,`+
		`"provisionProgress":{"phase":%q},"provisionProgressAt":%q}`,
		phase, time.Now().UTC().Format(time.RFC3339Nano))
}

// runningRuntime is a discobox with nothing left to provision.
const runningRuntime = `{"state":"ready","desiredState":"present","runtimeState":"running","generation":1,"observedGeneration":1}`

// watchTestApp points an App at a server that answers sandbox reads from the
// script, one entry per read, holding on the last.
func watchTestApp(t *testing.T, runtimes ...string) (*App, func() int) {
	t.Helper()
	var mu sync.Mutex
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			// The client probes the endpoint before it will use it.
			return
		}
		if r.URL.Path != "/projects/project-1/sandboxes/sbx_1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		runtime := runtimes[min(reads, len(runtimes)-1)]
		reads++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(sandboxJSON(runtime)))
	}))
	t.Cleanup(server.Close)
	return &App{serverURL: server.URL}, func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}
}

// The watch follows the discobox through its phases and reports each one once.
// Repeating an unchanged line would make a caller that renders every report
// flicker, and would say nothing new.
func TestWatchProvisioningReportsEachPhaseOnce(t *testing.T) {
	app, _ := watchTestApp(t,
		progressRuntime("pulling_image"),
		progressRuntime("pulling_image"),
		progressRuntime("creating_container"),
		progressRuntime("creating_container"),
		progressRuntime("starting_container"),
	)

	ctx, cancel := context.WithCancel(t.Context())
	lines := make(chan string, 8)
	go func() {
		app.watchProvisioning(ctx, "project-1", "sbx_1", func(line string) { lines <- line })
		close(lines)
	}()

	want := []string{"pulling the discobox image", "creating the container", "starting the container"}
	for _, expected := range want {
		select {
		case got := <-lines:
			if got != expected {
				t.Fatalf("reported %q, want %q", got, expected)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %q", expected)
		}
	}
	cancel()
}

// An attach onto a discobox that is already up asks the server nothing at all.
// The first read waits an interval, and a connection lands well inside it, so
// narration costs a request only when there is a wait worth narrating.
func TestWatchProvisioningReadsNothingForAFastAttach(t *testing.T) {
	app, reads := watchTestApp(t, runningRuntime)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.watchProvisioning(ctx, "project-1", "sbx_1", func(line string) {
			t.Errorf("reported %q for a discobox with nothing to provision", line)
		})
	}()
	// The attach connects immediately, which is what takes the watch down.
	cancel()
	<-done

	if got := reads(); got != 0 {
		t.Fatalf("read the discobox %d times, want none for an attach that connected at once", got)
	}
}

// A discobox that is up says nothing even when the watch outlives the first
// interval: what remains then is inside it, and the caller keeps its own line.
func TestWatchProvisioningStaysSilentForAUsableSandbox(t *testing.T) {
	app, _ := watchTestApp(t, runningRuntime)

	ctx, cancel := context.WithTimeout(t.Context(), 3*provisionPollInterval)
	defer cancel()
	app.watchProvisioning(ctx, "project-1", "sbx_1", func(line string) {
		t.Errorf("reported %q for a discobox that is already up", line)
	})
}
