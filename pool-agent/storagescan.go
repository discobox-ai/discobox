package poolagent

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"
)

const (
	// storageScanDutyCycle is the fraction of wall-clock time this agent is
	// willing to spend walking disk. Everything about the schedule falls out of
	// it: the next interval is the last sweep's cost divided by this, so a pool
	// whose trees take 400ms is walked every minute and one whose trees take a
	// minute is walked every fifty.
	//
	// It is expressed as a duty cycle rather than as a multiplier because it is
	// the one form an operator can state and check: "never spend more than 2%
	// of a core walking disk" is a budget, where "back off 50x" is a knob whose
	// consequences depend on a pool size nobody knows in advance.
	storageScanDutyCycle = 0.02
	// storageScanMinInterval floors the schedule for a pool small enough that
	// the duty cycle would allow walking constantly. Disk figures do not move
	// fast enough to be worth more than this, and the number that does move
	// fast — the filesystem's own free space — is a statfs on every report.
	storageScanMinInterval = time.Minute
	// storageScanMaxInterval caps it for a pool whose trees are enormous. At
	// the duty cycle above, reaching this cap takes a sweep of well over a
	// minute, so it rarely binds; when it does, the live statfs is still what
	// warns that the disk is filling, and this is only the attribution.
	storageScanMaxInterval = time.Hour
	// storageScanStartDelay keeps the first sweep out of the way of the
	// sandboxes starting up around it, which are competing for the same disk.
	storageScanStartDelay = 15 * time.Second
	// storageScanStagger spreads pools that share a host across their own
	// intervals. One Docker daemon hosts every local pool (ADR 0003), and after
	// a host reboot every agent on it would otherwise sweep in lockstep.
	storageScanStagger = 0.15
)

// storageScanner walks the pool's trees on an interval it sets from what the
// last walk cost.
//
// It runs on its own goroutine, and the resource reporter only ever reads its
// last completed result. That separation is the point: reading CPU and memory
// is three small files per sandbox and belongs on a fast fixed tick, while
// walking disk is one pass over every inode the pool owns and cannot share that
// tick without either making the cheap numbers stale or the expensive one
// constant.
//
// It is also what makes the loop correct rather than merely cheap. On a pool
// whose trees take longer to walk than the reporting interval, a fixed schedule
// does not degrade gracefully — sweeps overlap or the loop falls permanently
// behind. Deriving the interval from the measured cost means there is no tree
// size at which the schedule stops making sense.
type storageScanner struct {
	projectID string
	poolID    string
	logger    *slog.Logger
	// sandboxIDs is what the pool currently hosts, read at the start of each
	// sweep rather than held, so a sandbox created since the last one is
	// measured on this one.
	sandboxIDs func(context.Context) []string

	dutyCycle   float64
	minInterval time.Duration
	maxInterval time.Duration

	mu   sync.Mutex
	last *PoolStorageWalk
}

func newStorageScanner(logger *slog.Logger, bootstrap Bootstrap, sandboxIDs func(context.Context) []string) *storageScanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &storageScanner{
		projectID:   bootstrap.ProjectID,
		poolID:      bootstrap.PoolID,
		logger:      logger,
		sandboxIDs:  sandboxIDs,
		dutyCycle:   storageScanDutyCycle,
		minInterval: storageScanMinInterval,
		maxInterval: storageScanMaxInterval,
	}
}

// Snapshot is the last completed sweep, or nil when none has finished yet.
// The reporter never blocks on a sweep in flight: a report with no attribution
// is honest, and one that waited for a walk would make the CPU figures as slow
// as the disk ones.
func (s *storageScanner) Snapshot() *PoolStorageWalk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *storageScanner) start(ctx context.Context) {
	go s.run(ctx)
}

func (s *storageScanner) run(ctx context.Context) {
	if !sleepContext(ctx, storageScanStartDelay+s.stagger(storageScanStartDelay)) {
		return
	}
	for {
		interval, ok := s.sweep(ctx)
		if !ok {
			return
		}
		if !sleepContext(ctx, interval) {
			return
		}
	}
}

// sweep runs one walk and records it, returning how long to wait before the
// next. It reports false only when the context ended, which is the one case
// where there is no next.
func (s *storageScanner) sweep(ctx context.Context) (time.Duration, bool) {
	walk, ok := walkPoolTrees(ctx, s.projectID, s.poolID, s.sandboxIDs(ctx))
	if !ok {
		return 0, false
	}
	interval := s.nextInterval(time.Duration(walk.DurationMillis) * time.Millisecond)
	walk.IntervalSeconds = interval.Seconds()
	walk.NextScanAt = walk.ObservedAt.Add(interval)

	s.mu.Lock()
	s.last = walk
	s.mu.Unlock()

	// When the cap binds, the duty cycle is no longer being honored: the pool's
	// trees are too large to walk within budget even once an hour. That is a
	// real operational finding — it means per-tree attribution is costing more
	// than it is worth here — so it is said out loud rather than absorbed by
	// the clamp.
	if budget := time.Duration(float64(walk.DurationMillis) * float64(time.Millisecond) / s.dutyCycle); budget > s.maxInterval {
		s.logger.Warn("pool storage walk exceeds its time budget",
			"poolId", s.poolID,
			"durationMs", walk.DurationMillis,
			"intervalSeconds", interval.Seconds(),
			"budgetSeconds", budget.Seconds(),
			"dutyCycle", s.dutyCycle)
	} else {
		s.logger.Debug("walked pool storage",
			"poolId", s.poolID,
			"durationMs", walk.DurationMillis,
			"nextInSeconds", interval.Seconds(),
			"sandboxes", len(walk.Sandboxes))
	}
	return interval, true
}

// nextInterval spends dutyCycle of wall time walking: a sweep that cost d is
// followed by a wait of d/dutyCycle, clamped.
func (s *storageScanner) nextInterval(cost time.Duration) time.Duration {
	if s.dutyCycle <= 0 {
		return s.maxInterval
	}
	interval := time.Duration(float64(cost) / s.dutyCycle)
	if interval < s.minInterval {
		interval = s.minInterval
	}
	if interval > s.maxInterval {
		interval = s.maxInterval
	}
	return interval + s.stagger(interval)
}

// stagger is a fixed offset derived from the pool ID rather than a random one,
// so pools sharing a host spread across their intervals while each pool's own
// schedule stays reproducible — a random jitter would make the same agent
// behave differently run to run, which is harder to reason about and to test.
func (s *storageScanner) stagger(interval time.Duration) time.Duration {
	if interval <= 0 || storageScanStagger <= 0 {
		return 0
	}
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(s.poolID))
	fraction := float64(digest.Sum32()%1000) / 1000
	return time.Duration(float64(interval) * storageScanStagger * fraction)
}

// sleepContext waits out d, reporting false if the context ended first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
