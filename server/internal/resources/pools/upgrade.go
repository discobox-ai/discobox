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

	"github.com/discobox-ai/discobox/pool-agent/handover"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
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
	// Draining is a pool whose upgrade waits for its last handover's old
	// slot to drain (WaitingOn says so too).
	Draining bool
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
// has reported ready since: since the latest time this run's reconciles
// started its agent (sandbox.ReportAgentStarted), or since the settle when
// none did (ADR 26-11-15-294 §2). After a start in a running box the old
// agent may still report, so only the new agent's first report counts from
// that start, and any report from the settle (ADR 26-11-15-602 §3). A pool deleted, or whose provider instance is
// removed or disabled, drops out. Once none is left the watch has cleared,
// and stays clear for the rest of the run: a pool that fails later is that
// pool's health, not an upgrade.
//
// It reads pool and provider rows and writes nothing. What the reconciler
// tells it of a kept box and of an agent started, and what the pools' agents
// tell it of their first reports, lives only in this process, as the run it
// describes does.
type UpgradeWatch struct {
	store *store.Store
	// manager says which provider types this server can build: a pool on one
	// it cannot is not waited on (ADR 26-11-22-507 §2).
	manager *sandbox.ProviderManager
	now     func() time.Time

	mu      sync.Mutex
	watched map[string]bool
	// kept is, by pool, when a reconcile settled that kept the pool's
	// previous box, and why. A later settle of the pool supersedes it.
	kept map[string]keptBox
	// started is, by pool, the latest start of the pool's agent a reconcile
	// in this run recorded. It is recorded when the provider reports it, not
	// at the settle: the pass that starts the agent is often not the one
	// that settles.
	started map[string]agentStart
	// draining is, by pool, when a reconcile last found its box would not
	// move because a slot still drains from the last handover
	// (sandbox.ErrPoolSlotDraining). A later settle of the pool supersedes
	// it.
	draining map[string]time.Time
	// firstReport is, by pool, when the server last received a ready first
	// report from a pool agent process (ADR 26-11-15-602 §2), on the clock
	// StatusReportedAt is stamped with.
	firstReport map[string]time.Time
	begun       bool
	checked     time.Time
	refreshing  chan struct{}
	last        []PendingPool
}

type keptBox struct {
	settledAt time.Time
	reason    string
}

// agentStart is one start of a pool's agent a reconcile recorded: when, and
// whether it started a box or may restart the agent in a running one.
type agentStart struct {
	at   time.Time
	kind sandbox.AgentStartKind
}

// NewUpgradeWatch watches nothing until Begin.
func NewUpgradeWatch(appStore *store.Store, manager *sandbox.ProviderManager) *UpgradeWatch {
	return &UpgradeWatch{
		store: appStore, manager: manager, now: time.Now, kept: map[string]keptBox{},
		started: map[string]agentStart{}, firstReport: map[string]time.Time{}, draining: map[string]time.Time{},
	}
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
	// Named now, not at the first pass: refresh logs only a change from
	// this answer, so a pool that goes straight from it to upgraded would
	// otherwise never be named.
	if len(pending) > 0 {
		slog.InfoContext(ctx, "waiting for pools to upgrade", "pools", describePending(pending))
	}
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
	delete(w.draining, poolID)
	if kept == nil {
		delete(w.kept, poolID)
		return
	}
	w.kept[poolID] = keptBox{settledAt: settledAt, reason: firstLine(kept.Error())}
}

// drainWaiting records that a reconcile at at found the pool's box would not
// move while a slot still drains from its last handover.
//
// The first such pass of a streak is kept, so the drain's age is measured
// from it (drainingNow).
func (w *UpgradeWatch) drainWaiting(poolID string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.draining[poolID]; !ok {
		w.draining[poolID] = at
	}
}

// drainEnded records that a reconcile of the pool waited on something other
// than a drain (its host coming up): the streak drainWaiting began is over.
func (w *UpgradeWatch) drainEnded(poolID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.draining, poolID)
}

// agentStarted records that a reconcile started the pool's agent at at, as
// kind says. A later start replaces an earlier one.
func (w *UpgradeWatch) agentStarted(poolID string, at time.Time, kind sandbox.AgentStartKind) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if at.After(w.started[poolID].at) {
		w.started[poolID] = agentStart{at: at, kind: kind}
	}
}

// firstReported records that the pool's agent sent a ready first report,
// received at at (ADR 26-11-15-602 §2).
func (w *UpgradeWatch) firstReported(poolID string, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if at.After(w.firstReport[poolID]) {
		w.firstReport[poolID] = at
	}
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
	started := make(map[string]agentStart, len(w.started))
	for id, start := range w.started {
		started[id] = start
	}
	firsts := make(map[string]time.Time, len(w.firstReport))
	for id, at := range w.firstReport {
		firsts[id] = at
	}
	draining := make(map[string]time.Time, len(w.draining))
	for id, at := range w.draining {
		draining[id] = at
	}
	w.mu.Unlock()

	var pending []PendingPool
	var done []string
	var err error
	for _, id := range ids {
		var waiting *PendingPool
		waiting, err = w.waitingOn(ctx, id, kept[id], started[id], firsts[id], draining[id], now)
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
func (w *UpgradeWatch) waitingOn(ctx context.Context, id string, kept keptBox, started agentStart, first, drainAt time.Time, now time.Time) (*PendingPool, error) {
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
	provider, err := convergedProvider(ctx, w.store, w.manager, pool)
	if err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, nil
	}
	if drainingNow(pool, drainAt, now) {
		return &PendingPool{ID: pool.ID, Name: pool.Name, WaitingOn: waitingForDrain, Draining: true}, nil
	}
	if drainStuck(pool, drainAt, now) {
		return &PendingPool{ID: pool.ID, Name: pool.Name, WaitingOn: drainOutlasted}, nil
	}
	reason := upgradeWaitingOn(pool, kept, started, first, now)
	if reason == "" {
		return nil, nil
	}
	return &PendingPool{ID: pool.ID, Name: pool.Name, WaitingOn: reason}, nil
}

const waitingForReconcile = "waiting for its reconcile"

// waitingForDrain is a pool whose box moves once its last handover's old slot
// has drained, within that slot's drain bound (30 minutes).
var waitingForDrain = fmt.Sprintf("waiting for its last handover to drain, up to %d minutes", int(handover.DrainBound.Minutes()))

// drainOutlasted is a pool whose box still would not move for a drain past
// the drain's bound: a drain ends there, so the pool is stuck, not draining.
const drainOutlasted = "its last handover's drain outlasted its bound"

// drainMargin is how long past handover.DrainBound a drain streak still
// counts as one: the bound's process exits at it, and the next reconcile
// needs a pass to see so.
const drainMargin = 5 * time.Minute

// drainingNow is whether pool's upgrade waits on a drain: a reconcile streak
// that began at drainAt found its box would not move for one, none has
// settled since, and the streak is within the drain's bound.
func drainingNow(pool *model.Pool, drainAt, now time.Time) bool {
	return !drainAt.IsZero() && (pool.ReconciledAt == nil || !pool.ReconciledAt.After(drainAt)) &&
		now.Sub(drainAt) < handover.DrainBound+drainMargin
}

// drainStuck is whether pool's streak of passes that would not move for a
// drain has outlasted the drain's bound.
func drainStuck(pool *model.Pool, drainAt, now time.Time) bool {
	return !drainAt.IsZero() && (pool.ReconciledAt == nil || !pool.ReconciledAt.After(drainAt)) &&
		now.Sub(drainAt) >= handover.DrainBound+drainMargin
}

// upgradeWaitingOn is what pool has yet to do in this run, empty once it is
// upgraded: a reconcile of its current generation settled cleanly since the
// run's health checks began, its provider did not keep the previous box in
// it, and a ready status report that is its current agent's (agentReported).
func upgradeWaitingOn(pool *model.Pool, kept keptBox, started agentStart, first time.Time, now time.Time) string {
	switch {
	case !reconciledThisRun(pool):
		return waitingForReconcile
	case pool.ErrorMessage != nil:
		return "its reconcile failed: " + firstLine(*pool.ErrorMessage)
	case pool.ObservedGeneration < pool.Generation:
		return waitingForReconcile
	// The record is the reconcile's own clock; the row's copy of it may be
	// truncated by the database, never later.
	case kept.reason != "" && !pool.ReconciledAt.After(kept.settledAt):
		return "keeping its previous agent: " + kept.reason
	case !agentReported(pool, started, first):
		return "waiting for its agent to report"
	case pool.Health(now) != model.PoolHealthReady:
		return "its agent reports " + pool.Health(now)
	}
	return ""
}

// reconciledThisRun is whether a reconcile of pool has settled since this
// run's health checks began (HealthCheckStartedAt, or the pool's creation when
// it has none). The upgrade watch waits for one, and the reconciler logs the
// first at info.
func reconciledThisRun(pool *model.Pool) bool {
	began := pool.CreatedAt
	if pool.HealthCheckStartedAt != nil {
		began = *pool.HealthCheckStartedAt
	}
	return pool.ReconciledAt != nil && !pool.ReconciledAt.Before(began)
}

// agentReported is whether the pool's agent has reported since it is the
// current one, given its latest start this run recorded (started) and its
// latest ready first report (first). pool has settled (ReconciledAt is set).
//
//   - After a box start, any report counts (ADR 26-11-15-294 §2): no agent
//     ran before it, so the new agent's first report counts however soon it
//     lands, often before the pass that settles.
//   - After an in-place start the old agent may still report until the
//     restart stops it, so a report counts if it is a first report received
//     after the start (ADR 26-11-15-602 §3): the old agent's first report was
//     long before, and its heartbeats are not marked. An agent a box start
//     booted just before may send its first report after it; that counts
//     only if the restarted agent then fails its own, which 602's
//     Consequences accept.
//   - Otherwise, and as an in-place start's fallback (a restart that
//     restarted nothing, a first report lost), a report from the settle on
//     counts: a report from before it may be the old agent's.
func agentReported(pool *model.Pool, started agentStart, first time.Time) bool {
	if pool.StatusReportedAt == nil {
		return false
	}
	if started.kind == sandbox.AgentStartBox {
		return !pool.StatusReportedAt.Before(started.at)
	}
	if started.kind == sandbox.AgentStartInPlace && !first.IsZero() && !first.Before(started.at) {
		return true
	}
	return !pool.StatusReportedAt.Before(*pool.ReconciledAt)
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
