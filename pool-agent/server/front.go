package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpchannel"
	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// Front serves the agent's own listener from the moment the agent starts,
// before it has a runtime to serve: a pool reached over a control-plane
// channel (ADR 26-10-30-875 §2) cannot make its first report until the server
// has opened one, and the first report comes before everything else. It owns
// the channel's route for the agent's whole life; everything else is 503
// until Serve hands it the agent's router, and the router's afterwards.
type Front struct {
	server   *http.Server
	listener net.Listener
	logger   *slog.Logger
	// inFlight counts the requests being handled, an upgraded stream's whole
	// life included, which a drain waits out (ADR 26-11-26-536 §3).
	inFlight handover.InFlight
	// channel is the channel's route, behind the authentication of server
	// requests; nil for a pool that dials its control plane.
	channel     http.Handler
	channelPath string
	router      atomic.Pointer[http.Handler]
	done        chan error
}

// NewFront is the front of listener, which serves nothing until Start, so
// that what can fail of it fails before the agent says it is ready to take
// over (ADR 26-11-26-536 §4). channel is the pool end of the control-plane
// channel, or nil for a pool that dials its control plane itself.
func NewFront(logger *slog.Logger, listener net.Listener, identity Identity, controlPlanePublicKey string, channel http.Handler) (*Front, error) {
	if logger == nil {
		logger = slog.Default()
	}
	f := &Front{done: make(chan error, 1), listener: listener, logger: logger}
	if channel != nil {
		authenticator, err := NewSignedTokenAuthenticator(identity, controlPlanePublicKey)
		if err != nil {
			return nil, err
		}
		f.channel = authenticator.Middleware(requireChannelScope(channel))
		f.channelPath = cpchannel.Path(identity.ProjectID, identity.PoolID)
	}
	f.server = &http.Server{
		Handler:           f.inFlight.Wrap(f),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: those set absolute per-request conn
		// deadlines that survive protocol upgrades (exec attach websockets
		// proxied to the sandbox agent, the control-plane channel) and cut
		// long-lived streams off mid-flight. Liveness comes from
		// ReadHeaderTimeout, IdleTimeout, and the keepalives inside upgraded
		// connections.
		IdleTimeout: 120 * time.Second,
	}
	return f, nil
}

// Start serves the front's listener from here on.
func (f *Front) Start() {
	go func() {
		f.logger.Info("pool agent serving", "addr", f.listener.Addr())
		f.done <- f.server.Serve(f.listener)
	}()
}

func (f *Front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.channel != nil && r.URL.EscapedPath() == f.channelPath {
		f.channel.ServeHTTP(w, r)
		return
	}
	if router := f.router.Load(); router != nil {
		(*router).ServeHTTP(w, r)
		return
	}
	http.Error(w, "the pool agent is starting", http.StatusServiceUnavailable)
}

// serve hands the agent's router everything but the channel, and waits for
// ctx or the listener to end.
func (f *Front) serve(ctx context.Context, router http.Handler) error {
	f.router.Store(&router)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-f.done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Shutdown stops the front, as ctx bounds.
func (f *Front) Shutdown(ctx context.Context) error {
	return f.server.Shutdown(ctx)
}

// StopAccepting closes the front's listener, and nothing else: the
// connections it accepted are still served. Under a handover the listener is
// a socket systemd holds, which the agent taking over accepts from.
func (f *Front) StopAccepting() {
	_ = f.listener.Close()
}

// Drain serves what the front holds to its end, a stream's included, until
// ctx ends: a keep-alive connection closes once its current response is sent,
// so its client's next request opens a connection the agent that took over
// accepts. Whatever is left when ctx ends is cut.
func (f *Front) Drain(ctx context.Context) error {
	err := errors.Join(f.server.Shutdown(ctx), f.inFlight.Wait(ctx))
	if err != nil {
		_ = f.server.Close()
	}
	return err
}

// requireChannelScope admits a server token carrying
// ScopePoolControlPlaneChannel, and refuses any other.
func requireChannelScope(channel http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := SignedTokenClaimsFromContext(r.Context())
		if !ok || !claims.HasScope(ScopePoolControlPlaneChannel) {
			http.Error(w, "opening the control-plane channel needs the "+ScopePoolControlPlaneChannel+" scope", http.StatusForbidden)
			return
		}
		channel.ServeHTTP(w, r)
	})
}
