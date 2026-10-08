// Package credentials serves the agent credentials protocol inside the sandbox
// and relays it to the pool (ADR 0031).
//
// It holds no credential and makes no authorization decision. Its whole job is
// to put a URL in front of the sandbox that speaks the portable protocol, and
// to carry each call to the pool over the sandbox's own mTLS client
// certificate — the same material, and the same bridge.Dialer, the egress
// bridge uses, so the identity a request arrives with is the identity the
// sandbox already has and cannot choose, over whichever transport the pool's
// URL names.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/proxy/bridge"
)

const (
	// DefaultBridgeConfigPath is the egress bridge config the runtime-config
	// intake renders, which carries both the mTLS keypair and the pool endpoint
	// to dial.
	DefaultBridgeConfigPath = "/etc/discobox/proxy/bridge.json"
)

// ListenAddress is where the protocol is served inside the sandbox: the
// protocol's own well-known loopback address, so the server and every client
// built from the protocol package agree on it without either naming a port.
//
// Loopback-only matters here: sandboxes on a pool share a network, and an
// endpoint that answers for whoever connects must not be reachable from a
// sibling sandbox.
const ListenAddress = agentcreds.DefaultAddress

// bridgeConfig is the subset of the pool-written bridge config this package
// needs. The file is shared with the proxy bridge, which reads the rest.
type bridgeConfig struct {
	CredentialsURL string `json:"credentialsUrl"`
	ServerName     string `json:"serverName"`
	MTLSCAPath     string `json:"mtlsCaPath"`
	ClientCertPath string `json:"clientCertPath"`
	ClientKeyPath  string `json:"clientKeyPath"`
}

// Relay implements the protocol by forwarding to the pool. Every method is a
// pass-through: interpreting a request here would be interpreting it on the
// untrusted side of the boundary.
//
// The material it relays with is delivered to the sandbox while it runs — in
// the pool's runtime-config document, and again whenever the pool renews or
// replaces it (ADR 26-10-08-127 §5) — so a relay built once at start would go
// on presenting a certificate the pool has moved past. Each call checks the
// files it was built from and rebuilds when any has changed. The Dialer reads
// the material afresh on every handshake, but the HTTP client keeps
// connections open between calls, each carrying the certificate it was opened
// with; a new client is what makes the next call handshake again.
type Relay struct {
	path string

	mu     sync.Mutex
	built  materialStamp
	client *agentcreds.Client
}

var _ agentcreds.Service = (*Relay)(nil)

// New builds a relay from the intake-rendered bridge config at path (or
// DefaultBridgeConfigPath when empty).
func New(path string) (*Relay, error) {
	if path == "" {
		path = DefaultBridgeConfigPath
	}
	client, stamp, err := load(path)
	if err != nil {
		return nil, err
	}
	return &Relay{path: path, built: stamp, client: client}, nil
}

// materialStamp is the size and modification time of each file a relay was
// built from: the bridge config and the material it names. The intake replaces
// a file by renaming a new one over it, so any change shows here.
type materialStamp [4]struct {
	size    int64
	modTime time.Time
}

func stampOf(paths ...string) materialStamp {
	var out materialStamp
	for i, path := range paths {
		if info, err := os.Stat(path); err == nil {
			out[i].size, out[i].modTime = info.Size(), info.ModTime()
		}
	}
	return out
}

// load builds the protocol client from the bridge config at path, and stamps
// the files it read.
func load(path string) (*agentcreds.Client, materialStamp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, materialStamp{}, fmt.Errorf("read proxy material: %w", err)
	}
	var cfg bridgeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, materialStamp{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.CredentialsURL == "" {
		return nil, materialStamp{}, fmt.Errorf("%s names no pool credentials endpoint", path)
	}
	stamp := stampOf(path, cfg.MTLSCAPath, cfg.ClientCertPath, cfg.ClientKeyPath)
	pool, err := bridge.NewDialer(bridge.DialConfig{
		URL:            cfg.CredentialsURL,
		ServerName:     cfg.ServerName,
		MTLSCAPath:     cfg.MTLSCAPath,
		ClientCertPath: cfg.ClientCertPath,
		ClientKeyPath:  cfg.ClientKeyPath,
	})
	if err != nil {
		return nil, materialStamp{}, fmt.Errorf("pool credentials endpoint: %w", err)
	}
	// No client-wide timeout: the protocol client bounds each call itself,
	// and a use waits on the pool's judge for far longer than any other call.
	httpClient := &http.Client{
		Transport: &http.Transport{
			// Never proxied: this call goes straight to the pool, and the
			// sandbox's HTTP_PROXY points at the egress forwarder, which has no
			// business carrying it.
			Proxy:          nil,
			DialTLSContext: pool.DialTLSContext,
		},
	}
	return agentcreds.NewClient(pool.ServerURL(), agentcreds.WithHTTPClient(httpClient)), stamp, nil
}

// current is the client for the material as it is now. A rebuild that fails
// keeps the client that worked, and is tried again on the next call. One
// failure is expected: the intake replaces the certificate and the key one
// after the other, so a call between the two finds a pair that does not match.
func (r *Relay) current() *agentcreds.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path == "" {
		return r.client
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return r.client
	}
	var cfg bridgeConfig
	if json.Unmarshal(data, &cfg) != nil {
		return r.client
	}
	if stampOf(r.path, cfg.MTLSCAPath, cfg.ClientCertPath, cfg.ClientKeyPath) == r.built {
		return r.client
	}
	client, stamp, err := load(r.path)
	if err != nil {
		slog.Warn("reload sandbox credentials material; keeping the previous", "error", err)
		return r.client
	}
	r.client, r.built = client, stamp
	return r.client
}

func (r *Relay) List(ctx context.Context) ([]agentcreds.Credential, error) {
	return r.current().List(ctx)
}

func (r *Relay) Request(ctx context.Context, body agentcreds.RequestBody) (agentcreds.RequestStatus, error) {
	return r.current().Request(ctx, body)
}

func (r *Relay) RequestStatus(ctx context.Context, requestID string) (agentcreds.RequestStatus, error) {
	return r.current().RequestStatus(ctx, requestID)
}

func (r *Relay) Get(ctx context.Context, body agentcreds.UseBody) (agentcreds.UseResponse, error) {
	return r.current().Get(ctx, body)
}

func (r *Relay) Trusts(ctx context.Context) ([]agentcreds.Trust, error) {
	return r.current().Trusts(ctx)
}

func (r *Relay) RequestTrust(ctx context.Context, body agentcreds.TrustRequestBody) (agentcreds.TrustRequestStatus, error) {
	return r.current().RequestTrust(ctx, body)
}

func (r *Relay) TrustRequestStatus(ctx context.Context, requestID string) (agentcreds.TrustRequestStatus, error) {
	return r.current().TrustRequestStatus(ctx, requestID)
}

// Serve runs the loopback protocol endpoint until ctx is done.
//
// There is no token on this listener, for the same reason the hook socket has
// none: everything inside the sandbox is equally untrusted, so a secret shared
// between them would authenticate nothing. Authority comes from the client
// certificate one hop out, which no in-sandbox process can forge or borrow
// without already having root in the sandbox — at which point it has the
// listener too.
func Serve(ctx context.Context, logger *slog.Logger, relay *Relay, addr string) error {
	if logger == nil {
		logger = slog.Default()
	}
	if addr == "" {
		addr = ListenAddress
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           agentcreds.NewHandler(relay),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Longer than the longest call the relay makes, a use waiting on the
		// pool's judge, so a slow pool answer reaches the caller as the pool's
		// error rather than as a truncated response.
		WriteTimeout: agentcreds.UseTimeout + 30*time.Second,
		IdleTimeout:  120 * time.Second,
		BaseContext:  func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		// WithoutCancel, not Background: the shutdown deadline has to outlive the
		// cancellation that triggered it, while still descending from the caller's
		// context rather than starting a detached one.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("sandbox credentials endpoint serving", "addr", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
