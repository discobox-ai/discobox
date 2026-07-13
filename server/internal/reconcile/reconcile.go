// Package reconcile is a small level-triggered reconciliation engine.
//
// Callers mark a resource dirty (optionally not-before a future time) and a
// registered Reconciler converges it by reading the latest persisted state.
// See DESIGN.md for the model: dirty set, lease-based multi-node claiming,
// seq-based re-mark detection, failure backoff, and periodic scan backstop.
package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Reconciler converges one resource. Implementations read desired and observed
// state from the store themselves and must be idempotent: a reconcile may be
// re-run at any time, including concurrently with newer intent being written.
type Reconciler interface {
	Reconcile(ctx context.Context, id string) error
}

// ErrSuperseded marks a reconcile that lost a generation-guarded write because
// newer intent arrived mid-run. Reconcilers treat it as a clean settle: the
// newer intent's own transactional mark re-runs the reconcile against current
// state.
var ErrSuperseded = errors.New("reconcile superseded")

type supersededError struct{ message string }

func (e supersededError) Error() string {
	if e.message == "" {
		return ErrSuperseded.Error()
	}
	return e.message
}

func (e supersededError) Unwrap() error { return ErrSuperseded }

// Superseded wraps message as an ErrSuperseded so errors.Is matches.
func Superseded(message string) error { return supersededError{message: message} }

// Scanner is optionally implemented by a Reconciler to report resources that
// need attention independent of any explicit mark (the level-triggered
// backstop). The canonical implementation is one query, e.g.
// `SELECT id FROM sandboxes WHERE desired_generation > observed_generation`.
type Scanner interface {
	ScanDirty(ctx context.Context) ([]string, error)
}

// Options configures an Engine.
type Options struct {
	// WorkerID identifies this process in claims. Defaults to hostname plus a
	// random suffix.
	WorkerID string

	// SingleNode clears every claim at startup. Safe only when no other process
	// shares the database (e.g. embedded SQLite); it restores instant recovery
	// of rows claimed by a previous crashed run instead of waiting out leases.
	SingleNode bool

	// Lease is how long a claim is valid without renewal. A node that dies
	// releases its claims implicitly after this long. Default 30s.
	Lease time.Duration

	// PollInterval is how often the runner looks for claimable rows beyond
	// in-process wakeups. Default 5s.
	PollInterval time.Duration

	// ScanInterval is how often Scanner reconcilers are asked for dirty ids.
	// Default 60s.
	ScanInterval time.Duration

	// BackoffBase is the first retry delay after a failed reconcile; it doubles
	// per consecutive failure up to BackoffMax. Defaults 2s and 5m.
	BackoffBase time.Duration
	BackoffMax  time.Duration

	// DefaultConcurrency caps simultaneous reconciles per resource type on this
	// node when Register is not given WithConcurrency. Default 4.
	DefaultConcurrency int
}

func (o Options) withDefaults() Options {
	if o.WorkerID == "" {
		host, _ := os.Hostname()
		var b [4]byte
		_, _ = rand.Read(b[:])
		o.WorkerID = host + "-" + hex.EncodeToString(b[:])
	}
	if o.Lease <= 0 {
		o.Lease = 30 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	if o.ScanInterval <= 0 {
		o.ScanInterval = 60 * time.Second
	}
	if o.BackoffBase <= 0 {
		o.BackoffBase = 2 * time.Second
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 5 * time.Minute
	}
	if o.DefaultConcurrency < 1 {
		o.DefaultConcurrency = 4
	}
	return o
}

// RegisterOption configures one registered reconciler.
type RegisterOption func(*registration)

// WithConcurrency caps simultaneous reconciles of this type on one node.
func WithConcurrency(n int) RegisterOption {
	return func(r *registration) {
		if n >= 1 {
			r.concurrency = n
		}
	}
}

// dirtyRow is one entry in the dirty set: this resource may need attention.
// Primary-key upsert makes marking coalescing by construction.
type dirtyRow struct {
	ResourceType string     `gorm:"primaryKey;size:64"`
	ResourceID   string     `gorm:"primaryKey;size:128"`
	Seq          int64      // bumped on every mark; detects re-marks during a run
	NotBefore    time.Time  `gorm:"index"` // earliest claim time (timers, backoff)
	Attempts     int        // consecutive failures since last success
	ClaimedBy    *string    `gorm:"size:128"`
	LeaseExpires *time.Time // claim is void after this; dead nodes release implicitly
	MarkedAt     time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (dirtyRow) TableName() string { return "reconcile_dirty" }

// DirtyResource is one pending entry in the dirty set, for observability.
type DirtyResource struct {
	ResourceType string
	ResourceID   string
	NotBefore    time.Time
	Attempts     int
	ClaimedBy    *string
	MarkedAt     time.Time
}

// ListDirty returns the current dirty set (optionally filtered by resource
// type), ordered by not_before. Intended for observability and tests.
func (e *Engine) ListDirty(ctx context.Context, resourceType ...string) ([]DirtyResource, error) {
	q := e.db.WithContext(ctx).Model(&dirtyRow{}).Order("not_before ASC")
	if len(resourceType) > 0 {
		q = q.Where("resource_type IN ?", resourceType)
	}
	var rows []dirtyRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]DirtyResource, 0, len(rows))
	for _, r := range rows {
		out = append(out, DirtyResource{
			ResourceType: r.ResourceType,
			ResourceID:   r.ResourceID,
			NotBefore:    r.NotBefore,
			Attempts:     r.Attempts,
			ClaimedBy:    r.ClaimedBy,
			MarkedAt:     r.MarkedAt,
		})
	}
	return out, nil
}

// MarkDirty flags a resource for reconciliation as soon as possible.
func (e *Engine) MarkDirty(ctx context.Context, resourceType, id string) error {
	return e.MarkDirtyTx(ctx, e.db, resourceType, id)
}

// MarkDirtyAt flags a resource for reconciliation no earlier than `at`. This is
// the timer primitive ("re-check this provider at now+timeout"). Marking
// earlier than an existing not_before pulls the row forward; marking later
// never pushes it back.
func (e *Engine) MarkDirtyAt(ctx context.Context, resourceType, id string, at time.Time) error {
	return e.MarkDirtyAtTx(ctx, e.db, resourceType, id, at)
}

// MarkDirtyTx is MarkDirty inside the caller's transaction, so intent writes
// and the dirty mark commit atomically.
func (e *Engine) MarkDirtyTx(ctx context.Context, tx *gorm.DB, resourceType, id string) error {
	return e.MarkDirtyAtTx(ctx, tx, resourceType, id, time.Now())
}

// MarkDirtyAtTx is MarkDirtyAt inside the caller's transaction.
func (e *Engine) MarkDirtyAtTx(ctx context.Context, tx *gorm.DB, resourceType, id string, at time.Time) error {
	if resourceType == "" || id == "" {
		return fmt.Errorf("reconcile: resource type and id are required")
	}
	now := time.Now()
	row := dirtyRow{
		ResourceType: resourceType,
		ResourceID:   id,
		Seq:          1,
		NotBefore:    at,
		MarkedAt:     now,
	}
	err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "resource_type"}, {Name: "resource_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"seq":       gorm.Expr("reconcile_dirty.seq + 1"),
			"marked_at": now,
			// Pull forward, never push back. `excluded` works on SQLite and Postgres.
			"not_before": gorm.Expr(
				"CASE WHEN excluded.not_before < reconcile_dirty.not_before THEN excluded.not_before ELSE reconcile_dirty.not_before END"),
			"updated_at": now,
		}),
	}).Create(&row).Error
	if err != nil {
		return err
	}
	e.wake()
	return nil
}
