package poolruntime

import (
	"context"
	"log/slog"
	"time"
)

// HostSteps logs the steps of one reconcile pass over a pool's host, each
// with how long it took and how long the pass has run, so a restart's log
// says where the time between a VM starting and its pool settling went.
//
// The drift scan re-checks every pool every minute, and a pass that finds
// the host up walks the same steps in no time, so steps log at info only
// once the pass is known to bring the host up: a provider says so with
// BringingUp when it waits for a daemon, or makes, starts, replaces or
// provisions the pool's box. Steps before that are held and logged with it;
// those of a pass that never says so are logged at debug by Finish.
type HostSteps struct {
	pool  string
	began time.Time
	last  time.Time
	up    bool
	held  []heldStep
	// now is the clock; tests replace it.
	now func() time.Time
}

type heldStep struct {
	msg   string
	attrs []any
}

// NewHostSteps starts timing a pass over the pool's host.
func NewHostSteps(poolID string) *HostSteps {
	s := &HostSteps{pool: poolID, now: time.Now}
	s.began = s.now()
	s.last = s.began
	return s
}

// BringingUp marks the pass as one that brings the pool's host up: the steps
// held so far, and every step after, log at info.
func (s *HostSteps) BringingUp(ctx context.Context) {
	if s.up {
		return
	}
	s.up = true
	for _, step := range s.held {
		slog.InfoContext(ctx, step.msg, step.attrs...)
	}
	s.held = nil
}

// Step records a step that has just ended: how long since the step before it,
// or since the pass began, and how long the pass has run.
func (s *HostSteps) Step(ctx context.Context, msg string, attrs ...any) {
	now := s.now()
	attrs = append([]any{"pool", s.pool, "duration", now.Sub(s.last).Round(time.Millisecond), "elapsed", now.Sub(s.began).Round(time.Millisecond)}, attrs...)
	s.last = now
	if s.up {
		slog.InfoContext(ctx, msg, attrs...)
		return
	}
	s.held = append(s.held, heldStep{msg: msg, attrs: attrs})
}

// Finish logs at debug the steps of a pass that never brought the host up.
func (s *HostSteps) Finish(ctx context.Context) {
	for _, step := range s.held {
		slog.DebugContext(ctx, step.msg, step.attrs...)
	}
	s.held = nil
}
