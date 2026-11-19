package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/internal/privatefs"
	"github.com/discobox-ai/discobox/pkg/endpoint"
)

// newAPICommand groups serving the Discobox API to a program on this machine.
//
// A program that wants the API — an admin UI, a script, an editor — would
// otherwise have to reach every server itself: dial iroh, find the registered
// servers, carry the token. The CLI already does all of that, so it serves it:
// the program speaks plain HTTP to a socket only its user can open, and every
// registered server is behind it, reached the way the CLI reaches it.
func (a *App) newAPICommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Serve the Discobox API to a program on this machine",
	}
	cmd.AddCommand(a.newAPIServeCommand())
	return cmd
}

func (a *App) newAPIServeCommand() *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve every registered server's API on a local socket",
		Long: `Serve every registered server's API on a local socket, for a program on this
machine that speaks HTTP: an admin UI, a script.

The socket is a unix socket only you can open, in the CLI's state directory
unless --listen names another. A custom socket must be in a private directory.
The unregistered primary is listed under its address label (usually "local"),
or "_primary" if a registered server already has that name. Behind it:

  GET /v1/servers                        the servers, the primary first, read
                                         again on every request, so a server
                                         "discobox admin remote add" registers
                                         is listed at once
  /v1/servers/NAME/api/PATH              the API of the server listed as NAME,
                                         reached the way this CLI reaches it:
                                         iroh, a socket or HTTPS, with its token
  GET /v1/health                         whether this command is serving

It runs until it is interrupted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serveAPI(cmd, listen, serverChosen(cmd))
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "", "listen on `ADDRESS`, unix:///PATH or a loopback http://127.0.0.1:PORT; defaults to api.sock in the CLI's state directory")
	return cmd
}

// defaultAPISocket is where `discobox admin api serve` listens when --listen
// names nothing: beside the CLI's own state, which is this user's alone.
func defaultAPISocket() string {
	return filepath.Join(cliStateDir(), "api.sock")
}

func (a *App) serveAPI(cmd *cobra.Command, listen string, chosen bool) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener, where, lock, err := listenForAPI(ctx, listen)
	if err != nil {
		return err
	}
	if lock != nil {
		defer func() { _ = lock.Release() }()
	}
	defer func() { _ = listener.Close() }()
	handler := newAPIHandler(a, chosen)
	handler.loopbackTCP = listener.Addr().Network() == "tcp"
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "serving the Discobox API on %s\n", where)

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

// listenForAPI opens what --listen names: a unix socket, by default, made
// readable and writable by this user alone, or a TCP port on a loopback
// address. Nothing on the socket authenticates a caller, so who can open it is
// the whole of its access control; a TCP port is reachable by every user on
// the machine, which is why it is loopback-only and not the default.
func listenForAPI(ctx context.Context, listen string) (net.Listener, string, *filelock.Lock, error) {
	value := strings.TrimSpace(listen)
	useDefault := value == ""
	if useDefault {
		value = "unix://" + defaultAPISocket()
	}
	var config net.ListenConfig
	if path, unix := strings.CutPrefix(value, "unix://"); unix {
		if path == "" {
			return nil, "", nil, fmt.Errorf("--listen %q names no socket path", listen)
		}
		dir := filepath.Dir(path)
		if useDefault {
			if err := ensureStateDir(dir); err != nil {
				return nil, "", nil, err
			}
		} else if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, "", nil, err
		}
		// The directory must exclude other users before Listen makes the
		// socket reachable. Leave existing custom directory permissions alone.
		if err := privatefs.Check(dir); err != nil {
			return nil, "", nil, fmt.Errorf("the API socket needs a private directory: %w", err)
		}
		lock, err := filelock.TryAcquire(path + ".lock")
		if errors.Is(err, filelock.ErrBusy) {
			return nil, "", nil, fmt.Errorf("%s is already being served or starting", path)
		}
		if err != nil {
			return nil, "", nil, err
		}

		if err := clearStaleSocket(ctx, path); err != nil {
			_ = lock.Release()
			return nil, "", nil, err
		}
		listener, err := config.Listen(ctx, "unix", path)
		if err != nil {
			_ = lock.Release()
			return nil, "", nil, fmt.Errorf("listen on %s: %w", path, err)
		}
		if runtime.GOOS == "windows" {
			err = privatefs.Restrict(path)
		} else {
			err = os.Chmod(path, 0o600)
		}
		if err != nil {
			_ = listener.Close()
			_ = lock.Release()
			return nil, "", nil, fmt.Errorf("make %s this user's alone: %w", path, err)
		}
		return listener, "unix://" + path, lock, nil
	}
	u, err := url.Parse(value)
	if err != nil {
		return nil, "", nil, fmt.Errorf("--listen %q: %w", listen, err)
	}
	switch u.Scheme {
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			host = "127.0.0.1"
		}
		if !isLoopbackHost(host) {
			return nil, "", nil, fmt.Errorf("--listen %q is not a loopback address: the API is served unauthenticated, so only this machine may reach it", listen)
		}
		if u.Port() == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return nil, "", nil, fmt.Errorf("--listen %q: use http://127.0.0.1:PORT", listen)
		}
		listener, err := config.Listen(ctx, "tcp", net.JoinHostPort(host, u.Port()))
		if err != nil {
			return nil, "", nil, fmt.Errorf("listen on %s: %w", u.Host, err)
		}
		return listener, "http://" + listener.Addr().String(), nil, nil
	default:
		return nil, "", nil, fmt.Errorf("--listen %q: use unix:///PATH or http://127.0.0.1:PORT", listen)
	}
}

// clearStaleSocket removes a socket file a command that is no longer running
// left behind, and refuses one that is still answering: two commands serving
// one path would take turns being reached.
func clearStaleSocket(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	var dialer net.Dialer
	dial, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := dialer.DialContext(dial, "unix", path)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("%s is already being served; stop that command first", path)
	}
	// Winsock uses WSAECONNREFUSED, distinct from Go's syscall.ECONNREFUSED.
	const wsaConnRefused = syscall.Errno(10061)
	refused := errors.Is(err, syscall.ECONNREFUSED) || runtime.GOOS == "windows" && errors.Is(err, wsaConnRefused)
	if !refused {
		return fmt.Errorf("check whether %s is already served: %w", path, err)
	}
	return os.Remove(path)
}

// apiServerEntry is one server as GET /v1/servers lists it.
type apiServerEntry struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	ID         string `json:"id,omitempty"`
	Primary    bool   `json:"primary"`
	Registered bool   `json:"registered"`
}

// apiHandler serves the servers and a proxy to each. The set of servers is
// read again on every request, so a registration, a rename, a removal or a new
// primary is seen without a restart; the client for each server is kept for
// the life of the command, because what a client holds — over iroh, streams
// on the one connection to the peer — is not free to rebuild per request.
type apiHandler struct {
	app *App
	// chosen is whether --server or DISCOBOX_SERVER named the primary; when
	// it did, the primary is fixed, and otherwise it follows the registry.
	chosen      bool
	loopbackTCP bool
	mu          sync.Mutex
	apps        map[string]*App
	mux         *http.ServeMux
}

func newAPIHandler(app *App, chosen bool) *apiHandler {
	app.preserveHTTPErrorResponses = true
	h := &apiHandler{app: app, chosen: chosen, apps: map[string]*App{}, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeAPIJSON(w, http.StatusOK, "application/json", map[string]bool{"ok": true})
	})
	h.mux.HandleFunc("GET /v1/servers", h.listServers)
	h.mux.HandleFunc("/v1/servers/{name}/api/{path...}", h.proxy)
	return h
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.loopbackTCP {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = strings.Trim(r.Host, "[]")
		}
		origin := r.Header.Get("Origin")
		if (!strings.EqualFold(host, "localhost") && !isLoopbackHost(host)) ||
			r.Header.Get("Sec-Fetch-Site") == "cross-site" ||
			(origin != "" && origin != "http://"+r.Host) {
			writeAPIProblem(w, http.StatusForbidden, "Request must come from this listener's origin", "")
			return
		}
	}
	h.mux.ServeHTTP(w, r)
}

// apiServerKey keeps case-sensitive HTTP paths distinct when choosing which
// upstream an API name represents. Ordinary remote commands use serverKey.
func apiServerKey(address string) string {
	parsed, err := endpoint.Parse(address)
	if err != nil {
		return serverKey(address)
	}
	switch parsed.Scheme {
	case "http", "https", endpoint.SchemeDiscoboxHTTP, endpoint.SchemeDiscoboxHTTPS:
		if u, err := url.Parse(parsed.Value); err == nil {
			u.Scheme = strings.ToLower(u.Scheme)
			u.Host = strings.ToLower(u.Host)
			return parsed.Scheme + " " + u.String()
		}
	}
	return serverKey(address)
}

// serverSet is the servers as they are now: the primary first, as the CLI
// would choose it for a command run at this moment, then the registered ones
// in the order they were registered.
func (h *apiHandler) serverSet() ([]apiServerEntry, error) {
	reg, err := loadServerRegistry()
	if err != nil {
		return nil, err
	}
	primary := h.app.serverURL
	if !h.chosen {
		primary = endpoint.DefaultEndpoint()
		if reg.Primary != "" {
			primary = reg.Primary
		}
	}
	servers := reg.serverSet(primary, apiServerKey)
	set := make([]apiServerEntry, 0, len(servers))
	for _, entry := range servers {
		set = append(set, apiServerEntry{
			Name: entry.name, Address: entry.address, ID: entry.id,
			Primary: entry.primary, Registered: entry.registered,
		})
	}
	if !set[0].Registered {
		set[0].ID = addressPeerID(primary)
		if _, collision := reg.byName(set[0].Name); collision {
			// Registered names cannot start with an underscore.
			set[0].Name = "_primary"
		}
	}
	return set, nil
}

func (h *apiHandler) listServers(w http.ResponseWriter, _ *http.Request) {
	set, err := h.serverSet()
	if err != nil {
		writeAPIProblem(w, http.StatusInternalServerError, "The registered servers could not be read", err.Error())
		return
	}
	writeAPIJSON(w, http.StatusOK, "application/json", map[string][]apiServerEntry{"servers": set})
}

// appFor is the App that reaches address, kept per address. The invocation's
// own server is the invocation itself, so --token reaches the server it was
// given for and no other.
func (h *apiHandler) appFor(address string) *App {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := address
	if app, ok := h.apps[key]; ok {
		return app
	}
	app := h.app
	if key != h.app.serverURL {
		app = h.app.forServer(address)
		app.preserveHTTPErrorResponses = true
	}
	h.apps[key] = app
	return app
}

// proxy passes a request to the server listed as its name, with the path
// after /api/ as the API path. Responses stream as they arrive, so a log
// follow, an event stream or an attach is not held back.
func (h *apiHandler) proxy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	set, err := h.serverSet()
	if err != nil {
		writeAPIProblem(w, http.StatusInternalServerError, "The registered servers could not be read", err.Error())
		return
	}
	var target *apiServerEntry
	for i := range set {
		if set[i].Name == name {
			target = &set[i]
			break
		}
	}
	if target == nil {
		writeAPIProblem(w, http.StatusNotFound, "No such server", fmt.Sprintf("no server is listed as %q; GET /v1/servers lists them", name))
		return
	}
	path := "/" + r.PathValue("path")
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "." || segment == ".." {
			writeAPIProblem(w, http.StatusBadRequest, "The API path contains a dot segment", "")
			return
		}
	}
	baseURL, client, err := h.appFor(target.Address).httpClient()
	if err != nil {
		writeAPIProblem(w, http.StatusBadGateway, "The server cannot be reached", err.Error())
		return
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		writeAPIProblem(w, http.StatusBadGateway, "The server cannot be reached", err.Error())
		return
	}
	// Keep escaped slashes inside path parameters, even though ServeMux
	// exposes a decoded wildcard. Split the escaped route by its separators.
	parts := strings.SplitN(r.URL.EscapedPath(), "/", 6)
	rawPath := "/" + parts[5]
	proxy := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.Out.URL.Scheme = base.Scheme
			out.Out.URL.Host = base.Host
			out.Out.URL.Path = strings.TrimSuffix(base.Path, "/") + path
			out.Out.URL.RawPath = strings.TrimSuffix(base.EscapedPath(), "/") + rawPath
			out.Out.Host = base.Host
			// The CLI authenticates; whatever the caller sent is not its to send.
			out.Out.Header.Del("Authorization")
		},
		Transport:     client.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeAPIProblem(w, http.StatusBadGateway, "The server did not answer", err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

func writeAPIJSON(w http.ResponseWriter, status int, contentType string, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "The response could not be encoded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// writeAPIProblem answers as the API itself does when it refuses something
// (RFC 9457), so a caller reads one error shape whoever answered.
func writeAPIProblem(w http.ResponseWriter, status int, title, detail string) {
	writeAPIJSON(w, status, "application/problem+json", struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}{status, title, detail})
}
