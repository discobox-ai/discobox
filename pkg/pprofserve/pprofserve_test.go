package pprofserve

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Unconfigured, nothing is bound and no token is written: profiling is opt-in.
func TestStartUnsetBindsNothing(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "pprof.token")
	bound, stop, err := Start(context.Background(), Config{Setting: "pprofListen", Addr: "", TokenFile: tokenFile})
	if err != nil {
		t.Fatalf("Start(\"\") error = %v", err)
	}
	defer stop()
	if bound != nil {
		t.Fatalf("Start(\"\") bound %s, want nothing", bound)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file stat = %v, want it absent", err)
	}
}

// Configured, the listener serves the standard endpoints on loopback to a
// request carrying the token, and stopping it closes the port and removes the
// token.
func TestStartServesProfilesUntilStopped(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "state", "pprof.token")
	bound, stop, err := Start(context.Background(), Config{Setting: "pprofListen", Addr: "127.0.0.1:0", TokenFile: tokenFile})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
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
func TestStartRotatesTheToken(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "pprof.token")
	if err := os.WriteFile(tokenFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stop, err := Start(context.Background(), Config{Setting: "pprofListen", Addr: "127.0.0.1:0", TokenFile: tokenFile})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
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
func TestStartFailsWhenTheAddressIsTaken(t *testing.T) {
	var listenConfig net.ListenConfig
	occupied, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy tcp port: %v", err)
	}
	defer occupied.Close()
	tokenFile := filepath.Join(t.TempDir(), "pprof.token")
	cfg := Config{Setting: "pprofListen", Addr: occupied.Addr().String(), TokenFile: tokenFile, Remedy: "set pprofListen to off"}
	if _, stop, err := Start(context.Background(), cfg); err == nil {
		stop()
		t.Fatalf("Start(%s) error = nil, want a bind error", occupied.Addr())
	} else if !strings.HasPrefix(err.Error(), "pprofListen: ") || !strings.HasSuffix(err.Error(), "(set pprofListen to off)") {
		t.Fatalf("Start() error = %v, want it to name pprofListen and end with the remedy", err)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file stat = %v, want none written", err)
	}
}

// Only loopback is accepted, by IP or as localhost, and the error names the
// setting.
func TestValidate(t *testing.T) {
	for _, addr := range []string{"", "127.0.0.1:6060", "[::1]:6060", "localhost:6060", "LOCALHOST:6060", "127.0.0.2:1"} {
		if err := Validate("DISCOBOX_X", addr); err != nil {
			t.Errorf("Validate(%q) = %v, want accepted", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:6060", ":6060", "[::]:6060", "10.0.0.1:6060", "example.com:6060", "127.0.0.1", "127.0.0.1:http", "127.0.0.1:70000"} {
		err := Validate("DISCOBOX_X", addr)
		if err == nil {
			t.Errorf("Validate(%q) accepted", addr)
		} else if !strings.HasPrefix(err.Error(), "DISCOBOX_X: ") {
			t.Errorf("Validate(%q) = %v, want it to name the setting", addr, err)
		}
	}
}
