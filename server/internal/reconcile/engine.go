package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"
)

type registration struct {
	reconciler  Reconciler
	concurrency int
}

// Engine owns the dirty set and runs registered reconcilers. Every node runs
// one Engine over the shared database; nodes are competing consumers (no
// leader election), so adding nodes scales reconcile throughput.
type Engine struct {
	db  *gorm.DB
	opt Options
	log *slog.Logger

	mu      sync.Mutex
	regs    map[string]*registration
	running map[string]int // in-flight reconciles per type on this node
	started bool

	notify chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates an Engine. Call Register for each resource type, then Start.
func New(db *gorm.DB, opt Options) (*Engine, error) {
	if db == nil {
		return nil, errors.New("reconcile: db is required")
	}
	if err := db.AutoMigrate(&dirtyRow{}); err != nil {
		return nil, fmt.Errorf("reconcile: migrate dirty table: %w", err)
	}
	return &Engine{
		db:      db,
		opt:     opt.withDefaults(),
		log:     slog.Default().With("component", "reconcile"),
		regs:    map[string]*registration{},
		running: map[string]int{},
		notify:  make(chan struct{}, 1),
	}, nil
}

// WorkerID returns this engine's claim identity.
func (e *Engine) WorkerID() string { return e.opt.WorkerID }

// Register installs the reconciler for a resource type. Must be called before
// Start.
func (e *Engine) Register(resourceType string, r Reconciler, opts ...RegisterOption) error {
	if resourceType == "" || r == nil {
		return errors.New("reconcile: resource type and reconciler are required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return errors.New("reconcile: register before Start")
	}
	if _, ok := e.regs[resourceType]; ok {
		return fmt.Errorf("reconcile: %q already registered", resourceType)
	}
	reg := &registration{reconciler: r, concurrency: e.opt.DefaultConcurrency}
	for _, opt := range opts {
		opt(reg)
	}
	e.regs[resourceType] = reg
	return nil
}

// Start launches the claim loop, lease renewal, and scanners. It returns
// immediately; Stop shuts down gracefully.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return nil
	}
	e.started = true
	e.mu.Unlock()
	e.ctx, e.cancel = context.WithCancel(ctx)

	if e.opt.SingleNode {
		// No other process can hold a valid lease: recover instantly instead of
		// waiting out leases from a previous crashed run.
		if err := e.db.WithContext(e.ctx).Model(&dirtyRow{}).
			Where("claimed_by IS NOT NULL").
			Updates(map[string]any{"claimed_by": nil, "lease_expires": nil}).Error; err != nil {
			return fmt.Errorf("reconcile: reset claims: %w", err)
		}
	}

	e.wg.Add(2)
	go e.claimLoop()
	go e.leaseLoop()

	e.mu.Lock()
	for resourceType, reg := range e.regs {
		if scanner, ok := reg.reconciler.(Scanner); ok {
			e.wg.Add(1)
			go e.scanLoop(resourceType, scanner)
		}
	}
	e.mu.Unlock()
	return nil
}

// Stop cancels background loops and waits for in-flight reconciles.
func (e *Engine) Stop(ctx context.Context) error {
	if e.cancel != nil {
		e.cancel()
	}
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wake nudges the claim loop without waiting for the next poll tick.
func (e *Engine) wake() {
	select {
	case e.notify <- struct{}{}:
	default:
	}
}

func (e *Engine) claimLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.opt.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
		case <-e.notify:
		}
		e.claimAvailable()
	}
}

// claimAvailable claims and launches every currently runnable row, respecting
// per-type concurrency, until nothing more is claimable.
func (e *Engine) claimAvailable() {
	for {
		types := e.typesWithCapacity()
		if len(types) == 0 {
			return
		}
		row, ok := e.claimOne(types)
		if !ok {
			return
		}
		e.mu.Lock()
		e.running[row.ResourceType]++
		e.mu.Unlock()
		e.wg.Add(1)
		go e.execute(row)
	}
}

func (e *Engine) typesWithCapacity() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	types := make([]string, 0, len(e.regs))
	for t, reg := range e.regs {
		if e.running[t] < reg.concurrency {
			types = append(types, t)
		}
	}
	return types
}

// claimOne atomically claims a runnable row for this worker. The two-step
// select-then-guarded-update is portable across SQLite and Postgres; losing a
// race costs one retry, not correctness.
func (e *Engine) claimOne(types []string) (dirtyRow, bool) {
	now := time.Now()
	var candidates []dirtyRow
	err := e.db.WithContext(e.ctx).
		Where("resource_type IN ?", types).
		Where("not_before <= ?", now).
		Where("claimed_by IS NULL OR lease_expires < ?", now).
		Order("not_before ASC").
		Limit(8).
		Find(&candidates).Error
	if err != nil || len(candidates) == 0 {
		return dirtyRow{}, false
	}
	for _, c := range candidates {
		lease := now.Add(e.opt.Lease)
		res := e.db.WithContext(e.ctx).Model(&dirtyRow{}).
			Where("resource_type = ? AND resource_id = ? AND seq = ?", c.ResourceType, c.ResourceID, c.Seq).
			Where("claimed_by IS NULL OR lease_expires < ?", now).
			Updates(map[string]any{"claimed_by": e.opt.WorkerID, "lease_expires": lease})
		if res.Error == nil && res.RowsAffected == 1 {
			return c, true
		}
	}
	return dirtyRow{}, false
}

// execute runs one reconcile and settles the row: delete on success (unless
// re-marked mid-run), backoff on failure. Panics are failures.
func (e *Engine) execute(row dirtyRow) {
	defer e.wg.Done()
	defer func() {
		e.mu.Lock()
		e.running[row.ResourceType]--
		e.mu.Unlock()
		e.wake() // capacity freed; look for more work
	}()

	e.mu.Lock()
	reg := e.regs[row.ResourceType]
	e.mu.Unlock()

	result, err := func() (result Result, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("reconcile panic: %v", r)
			}
		}()
		// withInFlight is what makes a self-mark detectable anywhere under this
		// call, however deep: MarkDirtyAtTx reads it off the context.
		return reg.reconciler.Reconcile(withInFlight(e.ctx, row.ResourceType, row.ResourceID), row.ResourceID)
	}()

	if err != nil {
		if e.ctx.Err() == nil {
			e.log.Warn("reconcile failed",
				"type", row.ResourceType, "id", row.ResourceID,
				"attempts", row.Attempts+1, "err", err)
		}
		e.release(row)
		return
	}
	e.complete(row, result)
}

// complete settles a row after a successful reconcile.
//
// Without a requeue the row is deleted. If it was re-marked while running the
// seq guard makes that delete miss, which means newer intent arrived mid-run:
// release the claim and leave the row dirty so the reconciler runs again
// against the newer state. That is the entire supersede story.
//
// With a requeue the row SURVIVES as the reconciler's timer, and not_before is
// assigned outright rather than pulled forward the way MarkDirtyAt does it.
// That is the point: the reconciler just read the resource, so it — not the
// stale value already on the row — is the authority on when it next needs
// attention. Doing this here, under the same seq guard, is also what keeps the
// timer honest about newer intent: a mid-run mark makes the guarded update miss
// and its own earlier not_before stands, so intent still beats the timer.
func (e *Engine) complete(row dirtyRow, result Result) {
	settled := map[string]any{"claimed_by": nil, "lease_expires": nil, "attempts": 0}

	var res *gorm.DB
	if result.RequeueAt.IsZero() {
		res = e.db.Model(&dirtyRow{}).
			Where("resource_type = ? AND resource_id = ? AND seq = ?", row.ResourceType, row.ResourceID, row.Seq).
			Delete(&dirtyRow{})
	} else {
		armed := map[string]any{"not_before": result.RequeueAt}
		for k, v := range settled {
			armed[k] = v
		}
		res = e.db.Model(&dirtyRow{}).
			Where("resource_type = ? AND resource_id = ? AND seq = ? AND claimed_by = ?",
				row.ResourceType, row.ResourceID, row.Seq, e.opt.WorkerID).
			Updates(armed)
	}
	if res.Error == nil && res.RowsAffected == 1 {
		// Settled, or armed for the future. Either way there is nothing to run
		// now, so deliberately no wake.
		return
	}

	// Re-marked during the run (or a transient error): clear our claim and reset
	// attempts — this run succeeded — and let the newer mark drive the re-run.
	e.db.Model(&dirtyRow{}).
		Where("resource_type = ? AND resource_id = ? AND claimed_by = ?", row.ResourceType, row.ResourceID, e.opt.WorkerID).
		Updates(settled)
	e.wake()
}

// release returns a failed row to the dirty set with exponential backoff.
func (e *Engine) release(row dirtyRow) {
	backoff := e.opt.BackoffBase << row.Attempts
	if backoff > e.opt.BackoffMax || backoff <= 0 {
		backoff = e.opt.BackoffMax
	}
	e.db.Model(&dirtyRow{}).
		Where("resource_type = ? AND resource_id = ? AND claimed_by = ?", row.ResourceType, row.ResourceID, e.opt.WorkerID).
		Updates(map[string]any{
			"claimed_by":    nil,
			"lease_expires": nil,
			"attempts":      gorm.Expr("attempts + 1"),
			"not_before":    time.Now().Add(backoff),
		})
}

// leaseLoop renews every claim held by this worker at a fraction of the lease,
// so long-running reconciles are not stolen by other nodes.
func (e *Engine) leaseLoop() {
	defer e.wg.Done()
	interval := e.opt.Lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.db.Model(&dirtyRow{}).
				Where("claimed_by = ?", e.opt.WorkerID).
				Update("lease_expires", time.Now().Add(e.opt.Lease))
		}
	}
}

// scanLoop periodically asks a Scanner reconciler for unconverged ids and
// marks them dirty. This is the level-triggered backstop: lost edges heal on
// the next scan.
func (e *Engine) scanLoop(resourceType string, scanner Scanner) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.opt.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			ids, err := scanner.ScanDirty(e.ctx)
			if err != nil {
				if e.ctx.Err() == nil {
					e.log.Warn("scan failed", "type", resourceType, "err", err)
				}
				continue
			}
			for _, id := range ids {
				if err := e.MarkDirty(e.ctx, resourceType, id); err != nil && e.ctx.Err() == nil {
					e.log.Warn("scan mark failed", "type", resourceType, "id", id, "err", err)
				}
			}
		}
	}
}
