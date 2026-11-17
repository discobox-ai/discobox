package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// apiHandlerFor is the handler `admin api serve` would serve, for an
// invocation aimed at primary with --server.
func apiHandlerFor(t *testing.T, primary string) *apiHandler {
	t.Helper()
	_, app := newRootCommand(false)
	app.serverURL = primary
	app.errOut = io.Discard
	return newAPIHandler(app, true)
}

func getJSON(t *testing.T, h http.Handler, path string, into any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if into != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("GET %s: %v in %q", path, err, rec.Body.String())
		}
	}
	return rec.Code
}

type listedServers struct {
	Servers []apiServerEntry `json:"servers"`
}

func TestAPIServeListsServersPrimaryFirstAndSeesNewRegistrations(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha")
	delta := fakeServer(t, "Delta Box")
	registerForTest(t, registeredServer{Name: "delta-box", Address: delta.URL})
	h := apiHandlerFor(t, primary.URL)

	var got listedServers
	if code := getJSON(t, h, "/v1/servers", &got); code != http.StatusOK {
		t.Fatalf("GET /v1/servers = %d", code)
	}
	if len(got.Servers) != 2 || !got.Servers[0].Primary || got.Servers[0].Address != primary.URL {
		t.Fatalf("servers = %+v, want the primary first", got.Servers)
	}
	if got.Servers[1].Name != "delta-box" || !got.Servers[1].Registered || got.Servers[1].Primary {
		t.Fatalf("servers[1] = %+v, want the registered delta-box", got.Servers[1])
	}

	// A server registered while it serves is listed on the next request: the
	// registry is read every time, not once.
	epsilon := fakeServer(t, "epsilon")
	registerForTest(t,
		registeredServer{Name: "delta-box", Address: delta.URL},
		registeredServer{Name: "epsilon", Address: epsilon.URL},
	)
	got = listedServers{}
	getJSON(t, h, "/v1/servers", &got)
	if len(got.Servers) != 3 || got.Servers[2].Name != "epsilon" {
		t.Fatalf("after registering = %+v, want epsilon listed", got.Servers)
	}
}

func TestAPIServeListsARegisteredPrimaryOnceUnderItsName(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha")
	registerForTest(t, registeredServer{Name: "home", Address: primary.URL})
	var got listedServers
	getJSON(t, apiHandlerFor(t, primary.URL), "/v1/servers", &got)
	if len(got.Servers) != 1 || got.Servers[0].Name != "home" || !got.Servers[0].Primary || !got.Servers[0].Registered {
		t.Fatalf("servers = %+v, want the primary once, as home", got.Servers)
	}
}

func TestAPIServeProxiesToTheNamedServer(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha")
	delta := fakeServer(t, "Delta Box")
	registerForTest(t, registeredServer{Name: "delta-box", Address: delta.URL})
	h := apiHandlerFor(t, primary.URL)

	var info struct{ Name string }
	if code := getJSON(t, h, "/v1/servers/delta-box/api/server", &info); code != http.StatusOK || info.Name != "Delta Box" {
		t.Fatalf("GET delta-box's /server = %d %+v, want Delta Box's answer", code, info)
	}
	info.Name = ""
	primaryName := addressLabel(primary.URL)
	if code := getJSON(t, h, "/v1/servers/"+primaryName+"/api/server", &info); code != http.StatusOK || info.Name != "alpha" {
		t.Fatalf("GET the primary's /server = %d %+v, want alpha's answer", code, info)
	}
}

func TestAPIServeAnswersAnUnknownServerAsAProblem(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha")
	rec := httptest.NewRecorder()
	apiHandlerFor(t, primary.URL).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/servers/nobody/api/server", nil))
	if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("unknown server = %d %s, want a 404 problem", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestAPIServeDropsTheCallersAuthorization(t *testing.T) {
	useTempServersFile(t)
	seen := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)
	req := httptest.NewRequest(http.MethodGet, "/v1/servers/"+addressLabel(upstream.URL)+"/api/server", nil)
	req.Header.Set("Authorization", "Bearer from-the-caller")
	apiHandlerFor(t, upstream.URL).ServeHTTP(httptest.NewRecorder(), req)
	if got := <-seen; got == "Bearer from-the-caller" {
		t.Fatal("the caller's Authorization reached the server; the CLI authenticates, not the caller")
	}
}

func TestAPIServeListensOnASocketOnlyItsUserCanOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("listens on a socket")
	}
	useTempServersFile(t)
	primary := fakeServer(t, "alpha")
	// A short directory: a unix socket path has a small length limit.
	dir, err := os.MkdirTemp("", "dbx-api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, _ := newRootCommand(false)
	cmd.SetArgs([]string{"--server", primary.URL, "admin", "api", "serve", "--listen", "unix://" + socket})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	var resp *http.Response
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if resp, err = client.Get("http://discobox/v1/health"); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("the socket never answered: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/health = %d", resp.StatusCode)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", mode)
	}

	// A second command on the same path is refused while the first serves.
	second, _ := newRootCommand(false)
	second.SetArgs([]string{"--server", primary.URL, "admin", "api", "serve", "--listen", "unix://" + socket})
	second.SetOut(io.Discard)
	second.SetErr(io.Discard)
	if err := second.ExecuteContext(ctx); err == nil || !strings.Contains(err.Error(), "already being served") {
		t.Fatalf("second serve on one socket = %v, want it refused", err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop when its context ended")
	}
}

func TestAPIServeRefusesANonLoopbackAddress(t *testing.T) {
	if _, _, err := listenForAPI(context.Background(), "http://0.0.0.0:0"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("listen on 0.0.0.0 = %v, want it refused", err)
	}
}
