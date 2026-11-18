package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/config"
)

// Unconfigured, nothing is bound and no token is written: profiling is opt-in.
func TestStartPprofUnsetBindsNothing(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), config.PprofTokenFileName)
	bound, stop, err := startPprof(context.Background(), "", tokenFile)
	if err != nil {
		t.Fatalf("startPprof(\"\") error = %v", err)
	}
	defer stop()
	if bound != nil {
		t.Fatalf("startPprof(\"\") bound %s, want nothing", bound)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file stat = %v, want it absent", err)
	}
}

// Configured, the listener serves the standard endpoints on loopback to a
// request carrying the token, and stopping it closes the port and removes the
// token.
func TestStartPprofServesProfilesUntilStopped(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "state", config.PprofTokenFileName)
	bound, stop, err := startPprof(context.Background(), "127.0.0.1:0", tokenFile)
	if err != nil {
		t.Fatalf("startPprof() error = %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	if tcp, ok := bound.(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		t.Fatalf("bound %v, want a loopback TCP address", bound)
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", info.Mode().Perm())
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil || len(token) < 32 {
		t.Fatalf("read token = %q, %v; want a long random token", token, err)
	}

	base := "http://" + bound.String()
	client := &http.Client{Timeout: 10 * time.Second}
	get := func(path, password string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		if password != "" {
			req.SetBasicAuth("pprof", password)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	for _, password := range []string{"", "wrong", string(token) + "x"} {
		if code, _ := get("/debug/pprof/heap?debug=1", password); code != http.StatusUnauthorized {
			t.Fatalf("GET heap with password %q = %d, want 401", password, code)
		}
	}
	for _, tc := range []struct{ path, want string }{
		{"/debug/pprof/", "goroutine"},
		{"/debug/pprof/goroutine?debug=1", "goroutine profile:"},
		{"/debug/pprof/heap?debug=1", "heap profile:"},
		{"/debug/pprof/cmdline", ""},
		{"/debug/pprof/profile?seconds=1", ""},
		{"/debug/pprof/trace?seconds=1", ""},
	} {
		code, body := get(tc.path, string(token))
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d %s, want 200", tc.path, code, body)
		}
		if body == "" || !strings.Contains(body, tc.want) {
			t.Fatalf("GET %s body does not contain %q", tc.path, tc.want)
		}
	}

	stop()
	stopped = true
	var dialer net.Dialer
	if conn, err := dialer.DialContext(context.Background(), "tcp", bound.String()); err == nil {
		_ = conn.Close()
		t.Fatalf("%s still accepts connections after stop", bound)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file stat after stop = %v, want it removed", err)
	}
}

// Each start writes a new token, so one taken from an earlier run is no use.
func TestStartPprofRotatesTheToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), config.PprofTokenFileName)
	if err := os.WriteFile(tokenFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stop, err := startPprof(context.Background(), "127.0.0.1:0", tokenFile)
	if err != nil {
		t.Fatalf("startPprof() error = %v", err)
	}
	defer stop()
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(token) == "stale" {
		t.Fatalf("token file still holds the earlier token")
	}
	if info, err := os.Stat(tokenFile); err == nil && runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", info.Mode().Perm())
	}
}

// An address the operator asked for and cannot have fails startup, and leaves
// no token behind.
func TestStartPprofFailsWhenTheAddressIsTaken(t *testing.T) {
	var listenConfig net.ListenConfig
	occupied, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy tcp port: %v", err)
	}
	defer occupied.Close()
	tokenFile := filepath.Join(t.TempDir(), config.PprofTokenFileName)
	if _, stop, err := startPprof(context.Background(), occupied.Addr().String(), tokenFile); err == nil {
		stop()
		t.Fatalf("startPprof(%s) error = nil, want a bind error", occupied.Addr())
	} else if !strings.Contains(err.Error(), "pprofListen") {
		t.Fatalf("startPprof() error = %v, want it to name pprofListen", err)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file stat = %v, want none written", err)
	}
}

// The API router, which every API transport serves, has no profiling routes.
func TestRouterDoesNotServePprof(t *testing.T) {
	router := newStubRouterForTest()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile?seconds=1", "/debug/pprof/heap"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("API router answered GET %s with 200; profiling must stay off the API", path)
		}
	}
}
