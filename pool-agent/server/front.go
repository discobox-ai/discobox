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
)

// Front serves the agent's own listener from the moment the agent starts,
// before it has a runtime to serve: a pool reached over a control-plane
// channel (ADR 26-10-30-875 §2) cannot make its first report until the server
// has opened one, and the first report comes before everything else. It owns
// the channel's route for the agent's whole life; everything else is 503
// until Serve hands it the agent's router, and the router's afterwards.
type Front struct {
	server *http.Server
	// channel is the channel's route, behind the authentication of server
	// requests; nil for a pool that dials its control plane.
	channel     http.Handler
	channelPath string
	router      atomic.Pointer[http.Handler]
	done        chan error
}

// StartFront serves listener. channel is the pool end of the control-plane
// channel, or nil for a pool that dials its control plane itself.
func StartFront(logger *slog.Logger, listener net.Listener, identity Identity, controlPlanePublicKey string, channel http.Handler) (*Front, error) {
	if logger == nil {
		logger = slog.Default()
	}
	f := &Front{done: make(chan error, 1)}
	if channel != nil {
		authenticator, err := NewSignedTokenAuthenticator(identity, controlPlanePublicKey)
		if err != nil {
			return nil, err
		}
		f.channel = authenticator.Middleware(requireChannelScope(channel))
		f.channelPath = cpchannel.Path(identity.ProjectID, identity.PoolID)
	}
	f.server = &http.Server{
		Handler:           f,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: those set absolute per-request conn
		// deadlines that survive protocol upgrades (exec attach websockets
		// proxied to the sandbox agent, the control-plane channel) and cut
		// long-lived streams off mid-flight. Liveness comes from
		// ReadHeaderTimeout, IdleTimeout, and the keepalives inside upgraded
		// connections.
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		logger.Info("pool agent serving", "addr", listener.Addr())
		f.done <- f.server.Serve(listener)
	}()
	return f, nil
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
