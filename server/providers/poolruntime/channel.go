package poolruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpchannel"
	"github.com/discobox-ai/discobox/pool-agent/cpmux"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// StreamSink receives the connections a pool's guest opens toward the control
// plane, with the pool they belong to: the server's carrier hub, which holds
// every request on one to that pool's routes (ADR 26-10-30-875 §3).
type StreamSink interface {
	Push(poolID string, conn net.Conn, cancel <-chan struct{}) error
}

// channelRedial is how long the holder waits before dialing a pool's channel
// again, doubling to channelRedialMax: short at first, because a pool's agent
// waits for its first channel before it can report, and the dial that finds
// it listening is the one its boot is waiting on.
const (
	channelRedial    = time.Second
	channelRedialMax = 15 * time.Second
	// channelSettled is how long a channel must have stayed open for the next
	// drop to start the backoff over: one that lasted is not the failure the
	// backoff is for.
	channelSettled = time.Minute
	// channelWarnEvery is how often a channel that keeps failing is logged
	// as a warning.
	channelWarnEvery = time.Minute
)

// controlPlaneChannels holds a control-plane channel open to each pool of a
// provider whose pools do not dial their control plane (ADR 26-10-30-875
// §1): from before the pool's box is made, so its agent finds one waiting,
// until the pool is removed or the provider closed.
type controlPlaneChannels struct {
	runtime RuntimeProvider
	manager PoolManager
	streams StreamSink

	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	held map[string]context.CancelFunc
}

func newControlPlaneChannels(runtime RuntimeProvider, manager PoolManager, streams StreamSink) *controlPlaneChannels {
	ctx, cancel := context.WithCancel(context.Background())
	return &controlPlaneChannels{
		runtime: runtime,
		manager: manager,
		streams: streams,
		ctx:     ctx,
		cancel:  cancel,
		held:    map[string]context.CancelFunc{},
	}
}

// hold keeps pool's channel open. Holding one already held does nothing.
func (c *controlPlaneChannels) hold(pool *model.Pool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.held[pool.ID]; ok || c.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.held[pool.ID] = cancel
	go c.keep(ctx, pool.ProjectID, pool.ID)
}

// release closes pool poolID's channel, and stops redialing it.
func (c *controlPlaneChannels) release(poolID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.held[poolID]; ok {
		cancel()
		delete(c.held, poolID)
	}
}

// close releases every channel.
func (c *controlPlaneChannels) close() {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held = map[string]context.CancelFunc{}
}

// keep dials the pool's channel, serves it while it stays open, and dials
// again when it drops, until ctx ends.
func (c *controlPlaneChannels) keep(ctx context.Context, projectID, poolID string) {
	wait := channelRedial
	var warned time.Time
	for ctx.Err() == nil {
		opened := time.Now()
		err := c.serve(ctx, projectID, poolID)
		if ctx.Err() != nil {
			return
		}
		if time.Since(opened) > channelSettled {
			wait = channelRedial
		}
		// A pool whose channel will not open cannot report, and nothing else
		// says why: its agent only sees no channel arrive. So the cause is a
		// warning here, once a minute rather than once a redial.
		if time.Since(warned) >= channelWarnEvery {
			slog.WarnContext(ctx, "pool control-plane channel down; redialing", "pool_id", poolID, "error", err, "redial_in", wait)
			warned = time.Now()
		} else {
			slog.DebugContext(ctx, "pool control-plane channel down", "pool_id", poolID, "error", err, "redial_in", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, channelRedialMax)
	}
}

// serve opens one channel and hands every stream the pool opens on it to the
// carrier hub, as the pool's, until it closes.
func (c *controlPlaneChannels) serve(ctx context.Context, projectID, poolID string) error {
	conn, err := c.dial(ctx, projectID, poolID)
	if err != nil {
		return err
	}
	session, err := cpmux.Client(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	slog.InfoContext(ctx, "pool control-plane channel open", "pool_id", poolID)
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	defer func() { _ = session.Close() }()
	closed := make(chan struct{})
	defer close(closed)
	for {
		stream, target, err := session.Accept()
		if err != nil {
			return err
		}
		if target != cpmux.TargetControlPlane {
			slog.WarnContext(ctx, "pool opened a stream to an unexpected target", "pool_id", poolID, "target", target)
			_ = stream.Close()
			continue
		}
		if err := c.streams.Push(poolID, stream, closed); err != nil {
			_ = stream.Close()
		}
	}
}

// dial opens the pool's channel at its agent, as the runtime reaches it, with
// the pool's enrolled key pinned: a pool not yet enrolled has no channel yet.
func (c *controlPlaneChannels) dial(ctx context.Context, projectID, poolID string) (net.Conn, error) {
	pool, err := c.manager.GetPool(ctx, projectID, poolID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(pool.PublicKey) == "" {
		return nil, errors.New("the pool is not enrolled yet")
	}
	poolKey, err := decodePoolKey(pool.PublicKey)
	if err != nil {
		return nil, err
	}
	serverKey, err := c.manager.AgentIssuerKey(ctx)
	if err != nil {
		return nil, err
	}
	token, err := c.manager.CreateAgentToken(ctx, poolagentauth.TokenClaims{ProjectID: projectID, PoolID: poolID, Scopes: []string{poolagentauth.ScopePoolControlPlaneChannel}})
	if err != nil {
		return nil, err
	}
	lease, err := c.runtime.AcquirePoolAgentClient(ctx, pool)
	if err != nil {
		return nil, err
	}
	conn, err := cpchannel.Dial(ctx, leaseTransport(lease.Client), leaseBaseURL(lease.BaseURL), projectID, poolID, token, serverKey, poolKey)
	if err != nil {
		lease.Release()
		return nil, err
	}
	return releasingConn{Conn: conn, release: lease.Release}, nil
}

func leaseTransport(client *http.Client) http.RoundTripper {
	if client != nil && client.Transport != nil {
		return client.Transport
	}
	return http.DefaultTransport
}

func leaseBaseURL(baseURL string) string {
	if strings.TrimSpace(baseURL) != "" {
		return baseURL
	}
	return defaultPoolBaseURL
}

func decodePoolKey(text string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("decode the pool's public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("the pool's public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// releasingConn releases the agent lease it was dialed over when it closes:
// the lease's transport carries the channel for as long as it is open.
type releasingConn struct {
	net.Conn
	release func()
}

func (c releasingConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
