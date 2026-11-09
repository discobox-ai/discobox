package model

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

const PoolBootstrapSilence = 2 * time.Minute
const PoolBootstrapOperationLimit = 15 * time.Minute

// PoolBootstrap is durable evidence of one initial attempt, independent of
// display phases. A nil document on a legacy pool grants no entitlement.
type PoolBootstrap struct {
	Attempt              string
	Generation           int64
	Operation            string
	StartedAt            time.Time
	ProgressAt           time.Time
	Marker               uint64
	Deadline             time.Time
	RegistrationDeadline time.Time
	Operations           map[string]BootstrapOperation
	Failure              string
	// EndedAt is when Failure was recorded.
	EndedAt time.Time
	// Failed counts the attempts for this generation that failed before this
	// one; the attempt after this one is paced by it (ADR 26-11-09-593 §2).
	Failed int
	// Repair is the reason this attempt owes the runtime a replacement before
	// anything else: what the attempt it replaced left is not trusted (§3).
	// The executor clears it once the replacement succeeds, so a pass that
	// dies first leaves it owed.
	Repair string
}

type BootstrapOperation struct {
	Bytes           int64
	Extracted       int64
	LayersExtracted int64
	Layers          int64
	Done            bool
	Bounded         bool
	Deadline        time.Time
}

// BootstrapEvidence comes only from the executor, before dispatch for bounded
// work and from actual transfer counters for streaming work.
type BootstrapEvidence struct {
	Operation       string
	Bytes           int64
	Extracted       int64
	LayersExtracted int64
	Layers          int64
	Bounded         bool
	Complete        bool
	Registration    bool
	Failure         string
	// Repaired reports that the replacement the attempt owed succeeded. It
	// is not work evidence: it renews no budget.
	Repaired bool
}

// NewPoolBootstrap starts a pool's first attempt for generation: a fresh
// identity and a silence budget that runs from now.
func NewPoolBootstrap(generation int64, now time.Time) PoolBootstrap {
	return PoolBootstrap{Attempt: rand.Text(), Generation: generation, ProgressAt: now}
}

// Next is the attempt that replaces this ended one for generation
// (ADR 26-11-09-593 §§1, 3). It owes the runtime a repair. A failure in the
// same generation counts toward its pacing; new intent starts the count over.
func (b *PoolBootstrap) Next(generation int64, now time.Time) PoolBootstrap {
	next := NewPoolBootstrap(generation, now)
	next.Repair = "previous bootstrap attempt ended"
	if b.Failure != "" {
		next.Repair += ": " + b.Failure
	}
	if b.Generation == generation {
		next.Failed = b.Failed + 1
	}
	return next
}

func (p *Pool) BootstrapFacts() *PoolBootstrap {
	if len(p.Bootstrap) == 0 {
		return nil
	}
	var b PoolBootstrap
	if json.Unmarshal(p.Bootstrap, &b) != nil || b.Attempt == "" {
		return nil
	}
	return &b
}

// Ended reports an attempt that can make no further progress for a pass at
// generation: it recorded a failure, or it belongs to an earlier generation.
// Expiry not yet recorded is not ended: the pass that observes it records the
// failure, and a later pass starts the new attempt (ADR 26-11-09-593 §§1-2).
func (b *PoolBootstrap) Ended(generation int64) bool {
	return b.Failure != "" || b.Generation != generation
}

func (b *PoolBootstrap) Allowance() time.Time {
	if !b.RegistrationDeadline.IsZero() {
		return b.RegistrationDeadline
	}
	if !b.Deadline.IsZero() {
		return b.Deadline
	}
	return b.ProgressAt.Add(PoolBootstrapSilence)
}

func (b *PoolBootstrap) Expired(now time.Time) string {
	if b.Failure != "" {
		return b.Failure
	}
	if now.Before(b.Allowance()) {
		return ""
	}
	if !b.RegistrationDeadline.IsZero() {
		return "pool agent did not register before timeout"
	}
	if !b.Deadline.IsZero() {
		return "bootstrap operation timed out"
	}
	return "bootstrap stopped making progress"
}

func (b *PoolBootstrap) Accept(e BootstrapEvidence, now time.Time) {
	if b.Failure != "" {
		return
	}
	if reason := b.Expired(now); reason != "" {
		b.Failure, b.EndedAt = reason, now
		return
	}
	if e.Failure != "" {
		b.Failure, b.EndedAt = e.Failure, now
		return
	}
	if e.Repaired {
		b.Repair = ""
		return
	}
	if e.Operation == "" || !b.RegistrationDeadline.IsZero() {
		return
	}
	if b.Operations == nil {
		b.Operations = map[string]BootstrapOperation{}
	}
	op, seen := b.Operations[e.Operation]
	// Never re-enter an old operation, reopen a completed one, or reset a
	// deadline when a worker is restarted.
	if (seen && b.Operation != e.Operation) || op.Done {
		return
	}
	if b.Operation != e.Operation {
		b.Operation = e.Operation
		b.StartedAt = now
		b.Deadline = time.Time{}
		if e.Bounded {
			op.Bounded = true
			op.Deadline = now.Add(PoolBootstrapOperationLimit)
		}
		b.Marker++
		b.ProgressAt = now
	}
	if op.Bounded {
		b.Deadline = op.Deadline
	}
	if e.Bytes > op.Bytes || e.Layers > op.Layers || e.Extracted > op.Extracted || e.LayersExtracted > op.LayersExtracted {
		op.Extracted = max(op.Extracted, e.Extracted)
		op.LayersExtracted = max(op.LayersExtracted, e.LayersExtracted)
		op.Bytes = max(op.Bytes, e.Bytes)
		op.Layers = max(op.Layers, e.Layers)
		b.Marker++
		b.ProgressAt = now
	}
	if e.Complete {
		op.Done = true
		b.Marker++
		b.ProgressAt = now
		b.Deadline = time.Time{}
		if e.Registration {
			b.RegistrationDeadline = now.Add(PoolBootstrapSilence)
		}
	}
	b.Operations[e.Operation] = op
}

// BootstrapWait pins one caller to one attempt. Reading old work never renews
// its silence budget; persisted deadlines can only shorten that budget.
type BootstrapWait struct {
	Attempt  string
	Marker   uint64
	Deadline time.Time
	Finished bool
}

func (w *BootstrapWait) Observe(p *Pool, now time.Time) (time.Time, bool, error) {
	if w.Finished {
		return time.Time{}, false, nil
	}
	b := p.BootstrapFacts()
	if w.Attempt != "" && (b == nil || b.Attempt != w.Attempt || b.Generation != p.Generation) {
		return time.Time{}, false, fmt.Errorf("pool %s: bootstrap attempt changed", p.ID)
	}
	if p.RegisteredAt != nil {
		w.Finished = true
		return time.Time{}, false, nil
	}
	if b == nil || b.Generation != p.Generation || w.Finished {
		return time.Time{}, false, nil
	}
	if reason := b.Expired(now); reason != "" {
		return time.Time{}, true, fmt.Errorf("pool %s bootstrap %s: %s", p.ID, b.Operation, reason)
	}
	if w.Attempt == "" {
		w.Attempt = b.Attempt
		w.Marker = b.Marker
		w.Deadline = now.Add(PoolBootstrapSilence)
	} else if b.Marker > w.Marker {
		w.Marker = b.Marker
		w.Deadline = now.Add(PoolBootstrapSilence)
	}
	deadline := w.Deadline
	if !b.Deadline.IsZero() || !b.RegistrationDeadline.IsZero() {
		deadline = b.Allowance()
	} else if b.Allowance().Before(deadline) {
		deadline = b.Allowance()
	}
	if !now.Before(deadline) {
		return deadline, true, fmt.Errorf("pool %s bootstrap %s: bootstrap stopped making progress", p.ID, b.Operation)
	}
	return deadline, true, nil
}
