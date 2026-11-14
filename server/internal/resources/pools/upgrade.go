package pools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// upgradeCheckInterval is how long one answer of UpgradeWatch.Pending is
// reused. The health endpoint is polled every 100ms by a waiting client; the
// pools it reads move on reconcile and heartbeat timescales.
const upgradeCheckInterval = time.Second

// upgradeCheckTimeout bounds one pass over the watched pools. A pass runs
// apart from the probe that asked for it, so a probe that gives up does not
// cancel it, and the next probe is answered from it.
const upgradeCheckTimeout = 10 * time.Second

// PendingPool is one pool the server is still bringing onto its agent, and
// what it waits on.
type PendingPool struct {
	ID        string
	Name      string
	WaitingOn string
}

// UpgradeWatch answers which pools this server run has not yet brought onto
// its agent (ADR 26-11-14-624 §1). The server's health reports upgrading
// pools until it answers none.
//
// The pools it watches are fixed when the server starts: those registered
// then, and present. A pool made since has no older agent to replace, so it
// is not waited on. A watched pool is upgraded once, in this run, a reconcile
// of its current generation has settled without an error and without its
// provider keeping the box it last provisioned (sandbox.ReportBoxKept) — so
// the box holds this server's release (ADR 26-10-24-953 §5) — and its agent
// has reported ready since. A pool deleted, or whose provider instance is
// removed or disabled, drops out. Once none is left the watch has cleared,
// and stays clear for the rest of the run: a pool that fails later is that
// pool's health, not an upgrade.
//
// It reads pool and provider rows and writes nothing. What the reconciler
// tells it of a kept box lives only in this process, as the run it describes
// does.
type UpgradeWatch struct {
	store *store.Store
	now   func() time.Time

	mu      sync.Mutex
	watched map[string]bool
	// kept is, by pool, when a reconcile settled that kept the pool's
	// previous box, and why. A later settle of the pool supersedes it.
	kept       map[string]keptBox
	begun      bool
	checked    time.Time
	refreshing chan struct{}
	last       []PendingPool
}

type keptBox struct {
	settledAt time.Time
	reason    string
}

// NewUpgradeWatch watches nothing until Begin.
func NewUpgradeWatch(appStore *store.Store) *UpgradeWatch {
	return &UpgradeWatch{store: appStore, now: time.Now, kept: map[string]keptBox{}}
}

// Begin fixes the pools to wait on: every present pool registered before this
// start. It runs once, at startup, after the pools' health checks begin.
// Until its first pass, every one of them is pending.
func (w *UpgradeWatch) Begin(ctx context.Context) error {
	projects, err := w.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	watched := map[string]bool{}
	var pending []PendingPool
	for i := range projects {
		listed, err := w.store.ListPools(ctx, projects[i].ID)
		if err != nil {
			return err
		}
		for j := range listed {
			if listed[j].RegisteredAt != nil && listed[j].DesiredState != model.DesiredStateDeleted {
				watched[listed[j].ID] = true
				pending = append(pending, PendingPool{ID: listed[j].ID, Name: listed[j].Name, WaitingOn: waitingForReconcile})
			}
		}
	}
	sortPending(pending)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watched, w.last, w.begun = watched, pending, true
	return nil
}

// settled records how a reconcile of the pool settled: kept is why its
// provider kept the box it last provisioned, nil when it did not.
func (w *UpgradeWatch) settled(poolID string, settledAt time.Time, kept error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if kept == nil {
		delete(w.kept, poolID)
		return
	}
	w.kept[poolID] = keptBox{settledAt: settledAt, reason: firstLine(kept.Error())}
}

// Pending answers the watched pools not yet upgraded, sorted by name. Before
// Begin it answers none: nothing has said what to wait on.
//
// One pass runs at a time, at most once an interval, under a context of its
// own. A caller whose ctx ends first, and every caller while a pass runs, is
// answered the last pass's answer; so is every caller after a pass that
// failed, which is logged. The answer before the first pass is every watched
// pool, so a failed read never reports a server settled.
func (w *UpgradeWatch) Pending(ctx context.Context) []PendingPool {
	w.mu.Lock()
	if !w.begun || len(w.watched) == 0 {
		w.mu.Unlock()
		return nil
	}
	now := w.now()
	if w.refreshing != nil || (!w.checked.IsZero() && now.Sub(w.checked) < upgradeCheckInterval) {
		last := w.last
		w.mu.Unlock()
		return last
	}
	done := make(chan struct{})
	w.refreshing = done
	w.mu.Unlock()

	go func() {
		defer close(done)
		passCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upgradeCheckTimeout)
		defer cancel()
		w.refresh(passCtx, now)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// refresh runs one pass over the watched pools and records its answer.
func (w *UpgradeWatch) refresh(ctx context.Context, now time.Time) {
	w.mu.Lock()
	ids := make([]string, 0, len(w.watched))
	for id := range w.watched {
		ids = append(ids, id)
	}
	kept := make(map[string]keptBox, len(w.kept))
	for id, box := range w.kept {
		kept[id] = box
	}
	w.mu.Unlock()

	var pending []PendingPool
	var done []string
	var err error
	for _, id := range ids {
		var waiting *PendingPool
		waiting, err = w.waitingOn(ctx, id, kept[id], now)
		if err != nil {
			break
		}
		if waiting == nil {
			done = append(done, id)
			continue
		}
		pending = append(pending, *waiting)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.refreshing = nil
	w.checked = now
	if err != nil {
		slog.WarnContext(ctx, "could not read the pools still upgrading; health answers what it last read", "error", err)
		return
	}
	for _, id := range done {
		delete(w.watched, id)
	}
	sortPending(pending)
	if describePending(pending) != describePending(w.last) {
		if len(pending) == 0 {
			slog.Info("every pool is upgraded")
		} else {
			slog.Info("pools still upgrading", "pools", describePending(pending))
		}
	}
	w.last = pending
}

// waitingOn is what one watched pool is still waiting on, or nil once it is
// upgraded or no longer watched.
func (w *UpgradeWatch) waitingOn(ctx context.Context, id string, kept keptBox, now time.Time) (*PendingPool, error) {
	pool, err := w.store.GetPoolByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pool.DesiredState == model.DesiredStateDeleted {
		return nil, nil
	}
	provider, err := convergedProvider(ctx, w.store, pool)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, nil
	}
	reason := upgradeWaitingOn(pool, kept, now)
	if reason == "" {
		return nil, nil
	}
	return &PendingPool{ID: pool.ID, Name: pool.Name, WaitingOn: reason}, nil
}

const waitingForReconcile = "waiting for its reconcile"

// upgradeWaitingOn is what pool has yet to do in this run, empty once it is
// upgraded: a reconcile of its current generation settled cleanly since the
// run's health checks began, its provider did not keep the previous box in
// it, and a ready status report since that.
func upgradeWaitingOn(pool *model.Pool, kept keptBox, now time.Time) string {
	began := pool.CreatedAt
	if pool.HealthCheckStartedAt != nil {
		began = *pool.HealthCheckStartedAt
	}
	reconciled := pool.ReconciledAt != nil && !pool.ReconciledAt.Before(began)
	switch {
	case !reconciled:
		return waitingForReconcile
	case pool.ErrorMessage != nil:
		return "its reconcile failed: " + firstLine(*pool.ErrorMessage)
	case pool.ObservedGeneration < pool.Generation:
		return waitingForReconcile
	// The record is the reconcile's own clock; the row's copy of it may be
	// truncated by the database, never later.
	case kept.reason != "" && !pool.ReconciledAt.After(kept.settledAt):
		return "keeping its previous agent: " + kept.reason
	case pool.StatusReportedAt == nil || pool.StatusReportedAt.Before(*pool.ReconciledAt):
		return "waiting for its agent to report"
	case pool.Health(now) != model.PoolHealthReady:
		return "its agent reports " + pool.Health(now)
	}
	return ""
}

func sortPending(pending []PendingPool) {
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].Name != pending[j].Name {
			return pending[i].Name < pending[j].Name
		}
		return pending[i].ID < pending[j].ID
	})
}

// describePending names each pool and what it waits on, for the log.
func describePending(pending []PendingPool) string {
	parts := make([]string, 0, len(pending))
	for _, pool := range pending {
		parts = append(parts, fmt.Sprintf("%s (%s): %s", pool.Name, pool.ID, pool.WaitingOn))
	}
	return strings.Join(parts, "; ")
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	const limit = 200
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}
