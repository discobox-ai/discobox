package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The profile is fetched from the pool's route and written whole to the file,
// and the command says how to open it.
func TestFetchPoolPprofWritesTheProfile(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("profile bytes"))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	output := filepath.Join(t.TempDir(), "proxy-profile.pb.gz")
	var stdout, stderr bytes.Buffer
	if err := app.fetchPoolPprof(context.Background(), "proj_1", "pool_1", "proxy", "profile", 5, output, &stdout, &stderr); err != nil {
		t.Fatalf("fetch pool pprof: %v", err)
	}
	if gotPath != "/projects/proj_1/pools/pool_1/pprof/proxy/profile" || gotQuery != "seconds=5" {
		t.Fatalf("request = %s?%s", gotPath, gotQuery)
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "profile bytes" {
		t.Fatalf("file = %q, %v", data, err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "go tool pprof "+output) {
		t.Fatalf("stdout = %q, stderr = %q; want only how to open it, on stderr", stdout.String(), stderr.String())
	}
}

// A refusal, such as a pool with profiling off, reaches the operator as the
// error, and leaves no file behind.
func TestFetchPoolPprofSurfacesTheRefusal(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"profiling is off for pool pool_1"}`))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	dir := t.TempDir()
	err := app.fetchPoolPprof(context.Background(), "proj_1", "pool_1", "agent", "heap", 0, filepath.Join(dir, "agent-heap.pb.gz"), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "profiling is off for pool pool_1") {
		t.Fatalf("error = %v, want the server's reason", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused profile left %d files behind", len(entries))
	}
}

func TestDefaultPoolPprofFile(t *testing.T) {
	for _, tc := range []struct{ process, profile, want string }{
		{"agent", "profile", "agent-profile.pb.gz"},
		{"proxy", "heap", "proxy-heap.pb.gz"},
		{"mediator", "trace", "mediator.trace"},
	} {
		if got := defaultPoolPprofFile(tc.process, tc.profile); got != tc.want {
			t.Errorf("defaultPoolPprofFile(%q, %q) = %q, want %q", tc.process, tc.profile, got, tc.want)
		}
	}
}

// A server from before the route answers its router's plain 404, which is the
// server predating it, not a refusal of the pool's.
func TestFetchPoolPprofNamesAServerThatPredatesIt(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	err := app.fetchPoolPprof(context.Background(), "proj_1", "pool_1", "agent", "heap", 0, filepath.Join(t.TempDir(), "agent-heap.pb.gz"), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "server predates") {
		t.Fatalf("error = %v, want the server named as predating the route", err)
	}
}
