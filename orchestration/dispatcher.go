package orchestration

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DispatcherConfig controls job claiming, execution, retries, and leadership.
type DispatcherConfig struct {
	// WorkerID identifies this dispatcher instance in job rows and leader
	// records. If empty, the implementation should generate an ID that is stable
	// for the lifetime of the Dispatcher.
	WorkerID string

	// SingleNode disables durable leader election and lets this dispatcher claim
	// jobs immediately. Use this for embedded SQLite or one-process deployments.
	SingleNode bool

	// PollInterval controls how often the dispatcher scans for runnable jobs when
	// it has not been woken by NotifyNewJob.
	PollInterval time.Duration

	// HeartbeatInterval controls how often a multi-node leader renews its lease.
	// It is ignored when SingleNode is true.
	HeartbeatInterval time.Duration

	// HeartbeatTimeout is the lease duration after which another dispatcher may
	// take leadership if the current leader stops heartbeating. It is ignored
	// when SingleNode is true.
	HeartbeatTimeout time.Duration

	// JobTimeout is the maximum wall-clock duration allowed for one execution
	// attempt. The dispatcher passes a context with this deadline to Executor.
	JobTimeout time.Duration

	// StaleJobTimeout is how long a job may remain running in storage before it
	// is considered abandoned and reset to pending. If it is shorter than
	// JobTimeout, implementations should use a safe effective timeout that cannot
	// mark healthy in-flight jobs stale.
	StaleJobTimeout time.Duration

	// StaleCheckInterval controls how often the leader scans for abandoned
	// running jobs. The default is one minute. Lower values are useful for tests
	// or very short-lived jobs.
	StaleCheckInterval time.Duration

	// RetryBackoff is the fixed delay used when a failed job has attempts
	// remaining.
	RetryBackoff time.Duration

	// ImmediateExecution enables NotifyNewJob wakeups. When false, the dispatcher
	// should only discover new work through polling.
	ImmediateExecution bool

	// JobContext can attach application context derived from the claimed job before
	// executing it and updating its terminal state.
	JobContext func(context.Context, *Job) context.Context

	// DefaultConcurrency is the per-job-type concurrency limit used when an
	// executor is registered without WithConcurrency. Values less than one should
	// be treated as one.
	DefaultConcurrency int
}

// Dispatcher claims durable jobs and runs registered executors.
//
// Dispatcher is generic: it does not import or know application payload types.
// Concrete job packages register Executors at composition time.
type Dispatcher struct {
	store    Store
	cfg      DispatcherConfig
	workerID string

	executors map[Type]Executor
	execCfgs  map[Type]executorConfig

	running   map[Type]int
	runningMu sync.Mutex

	isLeader   bool
	isLeaderMu sync.RWMutex

	draining   bool
	drainingMu sync.RWMutex

	notifyCh chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	jobWg  sync.WaitGroup

	startMu sync.Mutex
	started bool
}

// NewDispatcher creates a dispatcher over the given Store.
//
// The returned dispatcher does not process work until Start is called.
func NewDispatcher(store Store, cfg DispatcherConfig) *Dispatcher {
	workerID := cfg.WorkerID
	if workerID == "" {
		workerID = NewIDString()
	}
	return &Dispatcher{
		store:     store,
		cfg:       cfg.withDefaults(),
		workerID:  workerID,
		executors: make(map[Type]Executor),
		execCfgs:  make(map[Type]executorConfig),
		running:   make(map[Type]int),
		notifyCh:  make(chan struct{}, 100),
	}
}

// Register adds an executor and its dispatcher options.
//
// Exactly one executor may be registered per Type. Register should return
// ErrExecutorAlreadyRegistered when another executor already handles the same
// type. Registration is expected to happen before Start.
func (d *Dispatcher) Register(executor Executor, opts ...ExecutorOption) error {
	if executor == nil {
		return errors.New("executor is nil")
	}
	jobType := executor.Type()
	if _, ok := d.executors[jobType]; ok {
		return ErrExecutorAlreadyRegistered
	}

	cfg := executorConfig{concurrency: d.cfg.DefaultConcurrency}
	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}

	d.executors[jobType] = executor
	d.execCfgs[jobType] = cfg
	return nil
}

// NotifyNewJob wakes the dispatcher after enqueue.
//
// Queue callers normally wire this through Queue.SetNotifyFunc. The method must
// be safe to call frequently and should never block the enqueuer.
func (d *Dispatcher) NotifyNewJob() {
	if !d.cfg.ImmediateExecution {
		return
	}
	select {
	case d.notifyCh <- struct{}{}:
	default:
	}
}

// Start begins dispatcher background loops.
//
// Start should return when ctx is canceled or when the dispatcher is stopped.
// In multi-node mode it performs leader election before claiming work. In
// single-node mode it may claim work immediately.
func (d *Dispatcher) Start(ctx context.Context) error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	if d.started {
		return nil
	}
	d.started = true
	d.ctx, d.cancel = context.WithCancel(ctx)

	if d.cfg.SingleNode {
		d.setLeader(true)
		_, _ = d.store.CleanupStaleJobs(d.ctx, 0)
	} else {
		d.wg.Add(1)
		go d.leaderLoop()
	}

	d.wg.Add(1)
	go d.processingLoop()

	d.wg.Add(1)
	go d.staleCleanupLoop()

	return nil
}

// BeginDrain stops the dispatcher from claiming new jobs.
//
// Already-running jobs are allowed to finish. BeginDrain is intended for
// graceful shutdown paths and may be called more than once.
func (d *Dispatcher) BeginDrain() {
	d.drainingMu.Lock()
	d.draining = true
	d.drainingMu.Unlock()
}

// DrainAndStop waits for in-flight jobs, then stops background loops.
//
// The supplied context bounds how long shutdown may wait. If the context expires
// before running jobs or background loops finish, DrainAndStop should return the
// context error while leaving durable job recovery to stale-job cleanup.
func (d *Dispatcher) DrainAndStop(ctx context.Context) error {
	d.BeginDrain()

	var retErr error
	if err := waitGroup(ctx, &d.jobWg); err != nil {
		retErr = err
	}

	if d.cancel != nil {
		d.cancel()
	}
	if err := waitGroup(ctx, &d.wg); err != nil && retErr == nil {
		retErr = err
	}

	if !d.cfg.SingleNode && d.IsLeader() {
		releaseCtx := context.Background()
		if d.ctx != nil {
			releaseCtx = context.WithoutCancel(d.ctx)
		}
		if err := d.store.ReleaseLeadership(releaseCtx, d.workerID); err != nil && retErr == nil {
			retErr = err
		}
	}

	return retErr
}

// IsLeader reports whether this dispatcher currently owns job claiming.
func (d *Dispatcher) IsLeader() bool {
	d.isLeaderMu.RLock()
	defer d.isLeaderMu.RUnlock()
	return d.isLeader
}

// WorkerID returns this dispatcher instance's worker identifier.
func (d *Dispatcher) WorkerID() string {
	return d.workerID
}

func (d *Dispatcher) leaderLoop() {
	defer d.wg.Done()
	d.tryAcquireLeadership()

	ticker := time.NewTicker(d.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.tryAcquireLeadership()
		}
	}
}

func (d *Dispatcher) tryAcquireLeadership() {
	acquired, err := d.store.TryAcquireLeadership(d.ctx, d.workerID, d.cfg.HeartbeatTimeout)
	if err != nil {
		d.setLeader(false)
		return
	}
	d.setLeader(acquired)
}

func (d *Dispatcher) processingLoop() {
	defer d.wg.Done()

	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.processAvailableJobs()
		case <-d.notifyCh:
			d.processAvailableJobs()
		}
	}
}

func (d *Dispatcher) processAvailableJobs() {
	if !d.IsLeader() {
		return
	}

	for {
		if d.IsDraining() {
			return
		}
		types := d.availableTypes()
		if len(types) == 0 {
			return
		}

		job, err := d.store.ClaimJob(d.ctx, types, d.workerID)
		if err != nil || job == nil {
			return
		}

		jobType := job.Type
		d.runningMu.Lock()
		d.running[jobType]++
		d.runningMu.Unlock()

		d.wg.Add(1)
		d.jobWg.Add(1)
		go func() {
			defer d.wg.Done()
			defer d.jobWg.Done()
			defer d.decrementRunning(jobType)
			d.executeJob(job)
		}()
	}
}

func (d *Dispatcher) executeJob(job *Job) {
	jobCtx := d.ctx
	if d.cfg.JobContext != nil {
		jobCtx = d.cfg.JobContext(jobCtx, job)
	}
	executor, ok := d.executors[job.Type]
	if !ok {
		_ = d.store.FailJob(jobCtx, job.ID, ErrExecutorNotRegistered.Error(), d.cfg.RetryBackoff)
		return
	}

	ctx, cancel := context.WithTimeout(jobCtx, d.cfg.JobTimeout)
	defer cancel()

	if assertor, ok := executor.(GenerationAssertor); ok {
		if err := assertor.AssertGeneration(ctx, job); err != nil {
			d.finishErroredJob(ctx, executor, job, err)
			return
		}
	}

	if err := executor.Execute(ctx, job); err != nil {
		d.finishErroredJob(ctx, executor, job, err)
		return
	}
	if err := d.store.CompleteJob(ctx, job.ID); err == nil {
		d.notifyTerminal(ctx, executor, job.ID)
	}
}

func (d *Dispatcher) finishErroredJob(ctx context.Context, executor Executor, job *Job, err error) {
	if errors.Is(err, ErrJobCanceled) {
		if err := d.store.CancelJob(ctx, job.ID, err.Error()); err == nil {
			d.notifyTerminal(ctx, executor, job.ID)
		}
		return
	}
	if err := d.store.FailJob(ctx, job.ID, err.Error(), d.cfg.RetryBackoff); err == nil {
		d.notifyTerminal(ctx, executor, job.ID)
	}
}

func (d *Dispatcher) notifyTerminal(ctx context.Context, executor Executor, jobID string) {
	observer, ok := executor.(TerminalObserver)
	if !ok {
		return
	}
	job, err := d.store.GetJob(ctx, jobID)
	if err != nil {
		return
	}
	switch job.Status {
	case StatusCompleted, StatusFailed, StatusCanceled:
	default:
		return
	}
	_ = observer.OnTerminal(ctx, job)
}

func (d *Dispatcher) staleCleanupLoop() {
	defer d.wg.Done()

	ticker := time.NewTicker(d.cfg.StaleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if d.IsLeader() {
				_, _ = d.store.CleanupStaleJobs(d.ctx, d.effectiveStaleJobTimeout())
			}
		}
	}
}

func (d *Dispatcher) availableTypes() []Type {
	d.runningMu.Lock()
	defer d.runningMu.Unlock()

	types := make([]Type, 0, len(d.executors))
	for jobType := range d.executors {
		if d.running[jobType] < d.execCfgs[jobType].concurrency {
			types = append(types, jobType)
		}
	}
	return types
}

func (d *Dispatcher) decrementRunning(jobType Type) {
	d.runningMu.Lock()
	defer d.runningMu.Unlock()
	d.running[jobType]--
}

func (d *Dispatcher) setLeader(value bool) {
	d.isLeaderMu.Lock()
	d.isLeader = value
	d.isLeaderMu.Unlock()
}

// IsDraining reports whether the dispatcher is refusing to claim new jobs.
func (d *Dispatcher) IsDraining() bool {
	d.drainingMu.RLock()
	defer d.drainingMu.RUnlock()
	return d.draining
}

func (d *Dispatcher) effectiveStaleJobTimeout() time.Duration {
	if d.cfg.StaleJobTimeout > d.cfg.JobTimeout {
		return d.cfg.StaleJobTimeout
	}
	return d.cfg.JobTimeout + time.Minute
}

func waitGroup(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (cfg DispatcherConfig) withDefaults() DispatcherConfig {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 30 * time.Second
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = 20 * time.Minute
	}
	if cfg.StaleJobTimeout <= 0 {
		cfg.StaleJobTimeout = 10 * time.Minute
	}
	if cfg.StaleCheckInterval <= 0 {
		cfg.StaleCheckInterval = time.Minute
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 2 * time.Second
	}
	if cfg.DefaultConcurrency < 1 {
		cfg.DefaultConcurrency = 1
	}
	return cfg
}
