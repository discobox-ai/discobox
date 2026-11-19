package provision

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

var retryIntervals = [...]time.Duration{15 * time.Second, time.Minute, 4 * time.Minute}

type automaticRetry struct {
	ctx     context.Context
	run     func(provisionspec.Status)
	attempt int
	timer   *time.Timer
}

// EnableAutomaticRetry lets the sandbox agent rerun failed provisioning.
// The callback runs the reprovision the tracker has already started.
func (t *Tracker) EnableAutomaticRetry(ctx context.Context, run func(provisionspec.Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.automatic = automaticRetry{ctx: ctx, run: run}
}

func (t *Tracker) scheduleRetryLocked() {
	retry := &t.automatic
	if t.state() != provisionspec.StateFailed {
		retry.attempt = 0
		return
	}
	if retry.run == nil || retry.ctx.Err() != nil || retry.attempt == len(retryIntervals) {
		return
	}
	interval := retryIntervals[retry.attempt]
	delay := interval - interval/5 + rand.N(2*interval/5+1) //nolint:gosec // Jitter spreads retries across boxes.
	retry.attempt++
	scheduled := &provisionspec.ScheduledRetry{
		Attempt: retry.attempt,
		Of:      len(retryIntervals),
		At:      time.Now().Add(delay).UTC(),
	}
	t.status.Retry = scheduled
	retry.timer = time.AfterFunc(time.Until(scheduled.At), func() {
		t.mu.Lock()
		// A manual request may have replaced the schedule while the timer fired.
		if t.status.Retry != scheduled || retry.ctx.Err() != nil {
			t.mu.Unlock()
			return
		}
		status, started := t.rerunLocked(true)
		t.mu.Unlock()
		if started {
			retry.run(status)
		}
	})
}
