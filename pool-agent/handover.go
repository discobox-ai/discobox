package poolagent

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// Lifecycle is how an agent gives way to the one that takes over from it
// (ADR 26-11-26-536 §§3–4). The zero Lifecycle binds its own listeners and
// never drains.
type Lifecycle struct {
	// Drain closes when the agent is told to drain: it stops accepting, ends
	// what runs once per pool, and serves what it holds to its end.
	Drain <-chan struct{}
	// StoppedAccepting is called once the agent accepts nothing more: it is
	// where it lets the agent taking over accept.
	StoppedAccepting func()
	// Sockets are the listeners systemd passed, by name
	// (handover.Listeners); one not among them is bound by the agent.
	Sockets map[string]net.Listener
}

// channelHandOver bounds how long a draining agent keeps its control-plane
// channel for the connections already forwarded onto it. The server dials
// again once it closes, and only then can the agent that took over make its
// first report, or carry the calls that wait on a channel
// (cpchannel.streamWait), so it is short of both.
const channelHandOver = 20 * time.Second

// untilDrained is ctx, ended also once drain closes.
func untilDrained(ctx context.Context, drain <-chan struct{}) context.Context {
	loops, stop := context.WithCancel(ctx)
	go func() {
		defer stop()
		select {
		case <-drain:
		case <-loops.Done():
		}
	}()
	return loops
}

// sweepsWaitSaid says once per process, of the several sweeps that wait,
// that they do.
var sweepsWaitSaid sync.Once

// sweepsAfterDrain runs sweep, a loop that removes what it finds nothing of
// this process holding, once no other agent of the pool is draining
// (handover.AgentDraining), or never, if ctx ends first: a draining agent
// still holds a snapshot build, an asset fetch or a sandbox's material that
// this one knows nothing of (ADR 26-11-26-536 §3).
func sweepsAfterDrain(ctx context.Context, logger *slog.Logger, root layout.Root, sweep func()) {
	path := root.System(handover.AgentDraining)
	if held, err := handover.Held(path); err == nil && held {
		sweepsWaitSaid.Do(func() { logger.Info("sweeping nothing until the pool's draining agent exits") })
	}
	if err := handover.AwaitFree(ctx, path); err != nil {
		return
	}
	sweep()
}

// becomeActive waits, until ctx ends, to be the agent of the pool that
// accepts, and reports whether it took over from another: one that held the
// lock as this one asked, which is draining now.
func becomeActive(ctx context.Context, logger *slog.Logger, root layout.Root) (*handover.Lock, bool, error) {
	path := root.System(handover.AgentLock)
	if lock, held, err := handover.TryAcquire(path); err != nil || held {
		return lock, false, err
	}
	logger.Info("waiting for the pool's draining agent to stop accepting")
	lock, err := handover.Acquire(ctx, path)
	if err == nil {
		logger.Info("took over from the pool's draining agent")
	}
	return lock, err == nil, err
}

// serveTakingOver serves, through serve. An agent that took over from one
// draining serves at once, and makes its first report as it comes (report):
// the pool is the one the draining agent registered, under the same key, and
// a request that arrives while the report waits for a control-plane channel
// is served rather than answered "starting" (ADR 26-11-26-536 §4). A report
// that fails ends it, as it ends an agent that reports first.
func serveTakingOver(ctx context.Context, tookOver bool, report func() error, serve func(context.Context) error) error {
	if !tookOver {
		return serve(ctx)
	}
	serving, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go func() {
		if err := report(); err != nil {
			stop(err)
		}
	}()
	err := serve(serving)
	if cause := context.Cause(serving); cause != nil && ctx.Err() == nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}
