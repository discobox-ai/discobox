package jobqueue_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obot-platform/disco2/jobqueue"
)

const (
	testTypeA jobqueue.Type = "test.a"
	testTypeB jobqueue.Type = "test.b"
)

type testPayload struct {
	TypeName  jobqueue.Type `json:"-"`
	ResourceT string        `json:"-"`
	ResourceI string        `json:"-"`
	Value     string        `json:"value,omitempty"`
	Dupe      bool          `json:"-"`
	PriorityV *int          `json:"-"`
	MaxV      *int          `json:"-"`
	At        time.Time     `json:"-"`
}

func (p testPayload) JobType() jobqueue.Type {
	return p.TypeName
}

func (p testPayload) Resource() jobqueue.Resource {
	return jobqueue.Resource{Type: p.ResourceT, ID: p.ResourceI}
}

func (p testPayload) AllowDuplicates() bool {
	return p.Dupe
}

func (p testPayload) Priority() int {
	if p.PriorityV == nil {
		return 0
	}
	return *p.PriorityV
}

func (p testPayload) MaxAttempts() int {
	if p.MaxV == nil {
		return 0
	}
	return *p.MaxV
}

func (p testPayload) ScheduledAt() time.Time {
	return p.At
}

type simplePayload struct {
	TypeName  jobqueue.Type `json:"-"`
	ResourceT string        `json:"-"`
	ResourceI string        `json:"-"`
	Value     string        `json:"value,omitempty"`
}

func (p simplePayload) JobType() jobqueue.Type {
	return p.TypeName
}

func (p simplePayload) Resource() jobqueue.Resource {
	return jobqueue.Resource{Type: p.ResourceT, ID: p.ResourceI}
}

type unmarshalablePayload struct {
	C chan struct{} `json:"c"`
}

func (p unmarshalablePayload) JobType() jobqueue.Type {
	return testTypeA
}

func (p unmarshalablePayload) Resource() jobqueue.Resource {
	return jobqueue.Resource{}
}

func TestQueueEnqueueAppliesDefaultsAndPayloadOptions(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{
		DefaultPriority:    7,
		DefaultMaxAttempts: 3,
	})

	priority := 42
	maxAttempts := 5
	scheduledAt := time.Now().Add(time.Hour).UTC().Round(time.Millisecond)
	job, err := queue.Enqueue(ctx, testPayload{
		TypeName:  testTypeA,
		ResourceT: "sandbox",
		ResourceI: "s1",
		Value:     "hello",
		PriorityV: &priority,
		MaxV:      &maxAttempts,
		At:        scheduledAt,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Type != testTypeA {
		t.Fatalf("type = %q, want %q", stored.Type, testTypeA)
	}
	if stored.Priority != priority {
		t.Fatalf("priority = %d, want %d", stored.Priority, priority)
	}
	if stored.MaxAttempts != maxAttempts {
		t.Fatalf("max attempts = %d, want %d", stored.MaxAttempts, maxAttempts)
	}
	if !stored.ScheduledAt.Equal(scheduledAt) {
		t.Fatalf("scheduled at = %s, want %s", stored.ScheduledAt, scheduledAt)
	}
	if stored.Resource != (jobqueue.Resource{Type: "sandbox", ID: "s1"}) {
		t.Fatalf("resource = %#v", stored.Resource)
	}

	var payload map[string]string
	if err := json.Unmarshal(stored.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload["value"] != "hello" {
		t.Fatalf("payload value = %q, want hello", payload["value"])
	}
}

func TestQueueEnqueueReturnsMarshalError(t *testing.T) {
	queue := jobqueue.NewQueue(newTestStore(t), jobqueue.QueueConfig{})
	if _, err := queue.Enqueue(context.Background(), unmarshalablePayload{C: make(chan struct{})}); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestQueueEnqueueReusesActiveJobAcrossJobTypesByResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 3})

	first, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	second, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeB, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second job = %s, want existing job %s", second.ID, first.ID)
	}
}

func TestQueueEnqueueDedupesConcurrentJobsByResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	const count = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var jobs []*jobqueue.Job
	var otherErrors []error

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			job, err := queue.Enqueue(ctx, simplePayload{
				TypeName:  testTypeA,
				ResourceT: "sandbox",
				ResourceI: "shared",
				Value:     fmt.Sprintf("job-%d", i),
			})

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				otherErrors = append(otherErrors, err)
				return
			}
			jobs = append(jobs, job)
		}(i)
	}
	close(start)
	wg.Wait()

	if len(otherErrors) > 0 {
		t.Fatalf("unexpected enqueue errors: %v", otherErrors)
	}
	if len(jobs) != count {
		t.Fatalf("jobs = %d, want %d", len(jobs), count)
	}
	firstID := jobs[0].ID
	for _, job := range jobs {
		if job.ID != firstID {
			t.Fatalf("job id = %s, want reused id %s", job.ID, firstID)
		}
	}

	if err := store.CompleteJob(ctx, firstID); err != nil {
		t.Fatalf("complete successful job: %v", err)
	}
	if _, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeB, ResourceT: "sandbox", ResourceI: "shared"}); err != nil {
		t.Fatalf("enqueue after terminal state: %v", err)
	}
}

func TestQueueDuplicateAllowerPermitsSecondActiveJobForResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 3})

	first, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	second, err := queue.Enqueue(ctx, testPayload{TypeName: testTypeB, ResourceT: "sandbox", ResourceI: "s1", Dupe: true})
	if err != nil {
		t.Fatalf("duplicate-allowed enqueue: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected separate jobs")
	}

	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testTypeA, testTypeB}, "worker")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected first claim")
	}

	blocked, err := store.ClaimJob(ctx, []jobqueue.Type{testTypeA, testTypeB}, "worker")
	if err != nil {
		t.Fatalf("claim while resource running: %v", err)
	}
	if blocked != nil {
		t.Fatalf("expected resource serialization to block second claim, got %s", blocked.ID)
	}
}

func TestQueueEnqueueAllowsJobsWithoutResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	first, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	second, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected separate jobs")
	}
}

func TestQueueMaxAttemptsDefaultsToOne(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{})
	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "zero-default"})
	if err != nil {
		t.Fatalf("enqueue zero default: %v", err)
	}
	if job.MaxAttempts != 1 {
		t.Fatalf("zero default max attempts = %d, want 1", job.MaxAttempts)
	}

	negativeQueue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: -10})
	job, err = negativeQueue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "negative-default"})
	if err != nil {
		t.Fatalf("enqueue negative default: %v", err)
	}
	if job.MaxAttempts != 1 {
		t.Fatalf("negative default max attempts = %d, want 1", job.MaxAttempts)
	}

	negativeMax := -2
	job, err = queue.Enqueue(ctx, testPayload{
		TypeName:  testTypeA,
		ResourceT: "sandbox",
		ResourceI: "negative-payload",
		MaxV:      &negativeMax,
	})
	if err != nil {
		t.Fatalf("enqueue negative payload max: %v", err)
	}
	if job.MaxAttempts != 1 {
		t.Fatalf("negative payload max attempts = %d, want 1", job.MaxAttempts)
	}
}

func TestStoreClaimHonorsPriorityScheduleAndResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	future := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		Priority:    100,
		MaxAttempts: 1,
		ScheduledAt: time.Now().Add(time.Hour),
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "future"},
	}
	low := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		Priority:    1,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "low"},
	}
	high := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		Priority:    10,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "high"},
	}
	for _, job := range []*jobqueue.Job{future, low, high} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}

	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testTypeA}, "worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != high.ID {
		t.Fatalf("claimed = %#v, want high priority job %s", claimed, high.ID)
	}
}

func TestStoreMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	job := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	if _, err := store.GetJob(ctx, job.ID); err != nil {
		t.Fatalf("get job after migrate: %v", err)
	}
}

func TestStoreFailJobRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	job := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 2,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testTypeA}, "worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.FailJob(ctx, claimed.ID, "try again", 0); err != nil {
		t.Fatalf("fail first: %v", err)
	}
	retry, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get retry: %v", err)
	}
	if retry.Status != jobqueue.StatusPending {
		t.Fatalf("status after first fail = %s, want pending", retry.Status)
	}
	if retry.Attempts != 1 {
		t.Fatalf("attempts after first fail = %d, want 1", retry.Attempts)
	}

	claimed, err = store.ClaimJob(ctx, []jobqueue.Type{testTypeA}, "worker")
	if err != nil {
		t.Fatalf("claim retry: %v", err)
	}
	if err := store.FailJob(ctx, claimed.ID, "done", 0); err != nil {
		t.Fatalf("fail second: %v", err)
	}
	failed, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if failed.Status != jobqueue.StatusFailed {
		t.Fatalf("status after second fail = %s, want failed", failed.Status)
	}
	if failed.CompletedAt == nil {
		t.Fatal("expected completedAt on failed job")
	}
}

func TestStoreGetLatestJobForResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	resource := jobqueue.Resource{Type: "sandbox", ID: "s1"}

	first := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    resource,
	}
	if err := store.CreateJob(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	second := &jobqueue.Job{
		Type:        testTypeB,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    resource,
	}
	if err := store.CreateJob(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	latest, err := store.GetLatestJobForResource(ctx, resource)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.ID != second.ID {
		t.Fatalf("latest = %s, want %s", latest.ID, second.ID)
	}
}

func TestStoreCleanupStaleJobs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	job := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testTypeA}, "worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Status != jobqueue.StatusRunning {
		t.Fatalf("claimed status = %s, want running", claimed.Status)
	}

	count, err := store.CleanupStaleJobs(ctx, 0)
	if err != nil {
		t.Fatalf("cleanup stale: %v", err)
	}
	if count != 1 {
		t.Fatalf("cleanup count = %d, want 1", count)
	}
	recovered, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get recovered: %v", err)
	}
	if recovered.Status != jobqueue.StatusPending {
		t.Fatalf("recovered status = %s, want pending", recovered.Status)
	}
	if recovered.WorkerID != nil || recovered.StartedAt != nil {
		t.Fatalf("expected worker and startedAt cleared, got worker=%v startedAt=%v", recovered.WorkerID, recovered.StartedAt)
	}
}

func TestStoreLeadershipAcquireRenewTimeoutRelease(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	ok, err := store.TryAcquireLeadership(ctx, "worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("worker-1 acquire = %v, %v; want true nil", ok, err)
	}
	ok, err = store.TryAcquireLeadership(ctx, "worker-2", time.Minute)
	if err != nil {
		t.Fatalf("worker-2 acquire: %v", err)
	}
	if ok {
		t.Fatal("worker-2 acquired before timeout")
	}
	ok, err = store.TryAcquireLeadership(ctx, "worker-2", -time.Nanosecond)
	if err != nil || !ok {
		t.Fatalf("worker-2 takeover = %v, %v; want true nil", ok, err)
	}
	if err := store.ReleaseLeadership(ctx, "worker-2"); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err = store.TryAcquireLeadership(ctx, "worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("worker-1 reacquire = %v, %v; want true nil", ok, err)
	}
}

func TestDispatcherRegisterValidation(t *testing.T) {
	dispatcher := newTestDispatcher(newTestStore(t))

	if err := dispatcher.Register(nil); err == nil {
		t.Fatal("expected nil executor to be rejected")
	}
	exec := &recordingExecutor{jobType: testTypeA, started: make(chan string, 1)}
	if err := dispatcher.Register(exec); err != nil {
		t.Fatalf("register first executor: %v", err)
	}
	if err := dispatcher.Register(&recordingExecutor{jobType: testTypeA, started: make(chan string, 1)}); !errors.Is(err, jobqueue.ErrExecutorAlreadyRegistered) {
		t.Fatalf("duplicate register error = %v, want ErrExecutorAlreadyRegistered", err)
	}
}

func TestDispatcherExecutesAndCompletesJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	exec := &recordingExecutor{
		jobType: testTypeA,
		started: make(chan string, 1),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopDispatcher(t, dispatcher)
	})

	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitForStarted(t, exec.started)
	waitForStatus(t, store, job.ID, jobqueue.StatusCompleted)
}

func TestDispatcherLifecycleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("second start: %v", err)
	}
	dispatcher.BeginDrain()
	dispatcher.BeginDrain()
	if !dispatcher.IsDraining() {
		t.Fatal("expected dispatcher to be draining")
	}
	stopDispatcher(t, dispatcher)

	notStarted := newTestDispatcher(store)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := notStarted.DrainAndStop(stopCtx); err != nil {
		t.Fatalf("drain stopped dispatcher before start: %v", err)
	}
	if !notStarted.IsDraining() {
		t.Fatal("expected not-started dispatcher to enter draining state")
	}
}

func TestDispatcherJobTimeoutFailsJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		jobType: testTypeA,
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	dispatcher := jobqueue.NewDispatcher(store, jobqueue.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       10 * time.Millisecond,
		JobTimeout:         20 * time.Millisecond,
		StaleJobTimeout:    time.Second,
		RetryBackoff:       time.Millisecond,
		ImmediateExecution: true,
		DefaultConcurrency: 1,
	})
	if err := dispatcher.Register(exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		closeOnce(exec.release)
		stopDispatcher(t, dispatcher)
	})

	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "timeout"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitForStarted(t, exec.started)
	waitForStatus(t, store, job.ID, jobqueue.StatusFailed)
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Error == nil || !strings.Contains(*stored.Error, "context deadline exceeded") {
		t.Fatalf("job error = %v, want context deadline exceeded", stored.Error)
	}
}

func TestDispatcherCanceledErrorCancelsJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	exec := &recordingExecutor{
		jobType: testTypeA,
		started: make(chan string, 1),
		cancel:  true,
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopDispatcher(t, dispatcher)
	})

	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "cancel"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitForStarted(t, exec.started)
	waitForStatus(t, store, job.ID, jobqueue.StatusCanceled)
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Error == nil || *stored.Error != "superseded" {
		t.Fatalf("job error = %v, want superseded", stored.Error)
	}
}

func TestDispatcherRetriesFailedJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 2})

	exec := &recordingExecutor{
		jobType:   testTypeA,
		started:   make(chan string, 2),
		failFirst: true,
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopDispatcher(t, dispatcher)
	})

	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitForStarted(t, exec.started)
	waitForStarted(t, exec.started)
	waitForStatus(t, store, job.ID, jobqueue.StatusCompleted)

	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", stored.Attempts)
	}
}

func TestDispatcherRunsMultipleJobsWithoutResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		jobType: testTypeA,
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(exec, jobqueue.WithConcurrency(2)); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		closeOnce(exec.release)
		stopDispatcher(t, dispatcher)
	})

	if _, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA}); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	if _, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA}); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	waitForStarted(t, exec.started)
	waitForStarted(t, exec.started)
}

func TestDispatcherConcurrencyLimit(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		jobType: testTypeA,
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(exec, jobqueue.WithConcurrency(1)); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		closeOnce(exec.release)
		stopDispatcher(t, dispatcher)
	})

	first, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	if _, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s2"}); err != nil {
		t.Fatalf("enqueue second: %v", err)
	}

	startedFirst := waitForStarted(t, exec.started)
	if startedFirst != first.ID {
		t.Fatalf("first started = %s, want %s", startedFirst, first.ID)
	}
	assertNoStart(t, exec.started, 50*time.Millisecond)
	closeOnce(exec.release)
	waitForStarted(t, exec.started)
}

func TestDispatcherStaleCleanupLoopRecoversRunningJobs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	dispatcher := jobqueue.NewDispatcher(store, jobqueue.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       time.Hour,
		JobTimeout:         time.Millisecond,
		StaleJobTimeout:    2 * time.Millisecond,
		StaleCheckInterval: 5 * time.Millisecond,
		RetryBackoff:       time.Millisecond,
		ImmediateExecution: true,
		DefaultConcurrency: 1,
	})
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopDispatcher(t, dispatcher)
	})

	job := &jobqueue.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "stale-loop"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testTypeA}, "abandoned-worker")
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected claimed job")
	}
	waitForStatus(t, store, job.ID, jobqueue.StatusPending)
	recovered, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get recovered job: %v", err)
	}
	if recovered.WorkerID != nil || recovered.StartedAt != nil {
		t.Fatalf("expected worker and startedAt cleared, got worker=%v startedAt=%v", recovered.WorkerID, recovered.StartedAt)
	}
}

func TestDispatcherDrainAndStopDoesNotStartNewJobsAndWaits(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		jobType: testTypeA,
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	if _, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"}); err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	waitForStarted(t, exec.started)

	dispatcher.BeginDrain()
	second, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s2"})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	assertNoStart(t, exec.started, 50*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done <- dispatcher.DrainAndStop(stopCtx)
	}()
	assertNoShutdown(t, done, 50*time.Millisecond)
	closeOnce(exec.release)
	if err := <-done; err != nil {
		t.Fatalf("drain stop: %v", err)
	}

	storedSecond, err := store.GetJob(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if storedSecond.Status != jobqueue.StatusPending {
		t.Fatalf("second status = %s, want pending", storedSecond.Status)
	}
}

func TestDispatcherMultiNodeOnlyLeaderExecutesJobs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	leaderExec := &recordingExecutor{jobType: testTypeA, started: make(chan string, 1)}
	standbyExec := &recordingExecutor{jobType: testTypeA, started: make(chan string, 1)}

	leader := newMultiNodeDispatcher(store, "worker-1", time.Minute)
	standby := newMultiNodeDispatcher(store, "worker-2", time.Minute)
	if err := leader.Register(leaderExec); err != nil {
		t.Fatalf("register leader: %v", err)
	}
	if err := standby.Register(standbyExec); err != nil {
		t.Fatalf("register standby: %v", err)
	}
	if err := leader.Start(ctx); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	t.Cleanup(func() { stopDispatcher(t, leader) })
	waitForLeader(t, leader, true)
	if err := standby.Start(ctx); err != nil {
		t.Fatalf("start standby: %v", err)
	}
	t.Cleanup(func() { stopDispatcher(t, standby) })
	waitForLeader(t, standby, false)
	queue.SetNotifyFunc(func() {
		leader.NotifyNewJob()
		standby.NotifyNewJob()
	})

	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if got := waitForStarted(t, leaderExec.started); got != job.ID {
		t.Fatalf("leader started %s, want %s", got, job.ID)
	}
	assertNoStart(t, standbyExec.started, 100*time.Millisecond)
	waitForStatus(t, store, job.ID, jobqueue.StatusCompleted)
}

func TestDispatcherMultiNodeDrainReleasesLeadership(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	first := newMultiNodeDispatcher(store, "worker-1", time.Minute)
	if err := first.Register(&recordingExecutor{jobType: testTypeA, started: make(chan string, 1)}); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first: %v", err)
	}
	waitForLeader(t, first, true)

	stopDispatcher(t, first)

	second := newMultiNodeDispatcher(store, "worker-2", time.Minute)
	if err := second.Register(&recordingExecutor{jobType: testTypeA, started: make(chan string, 1)}); err != nil {
		t.Fatalf("register second: %v", err)
	}
	if err := second.Start(ctx); err != nil {
		t.Fatalf("start second: %v", err)
	}
	t.Cleanup(func() { stopDispatcher(t, second) })
	waitForLeader(t, second, true)
}

func TestDispatcherMultiNodeFailoverAfterHeartbeatTimeout(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{DefaultMaxAttempts: 1})

	leaderExec := &blockingExecutor{
		jobType: testTypeA,
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	standbyExec := &recordingExecutor{jobType: testTypeA, started: make(chan string, 1)}

	leader := newMultiNodeDispatcherCustom(store, "worker-1", 120*time.Millisecond, time.Hour, time.Hour)
	standby := newMultiNodeDispatcherCustom(store, "worker-2", 120*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond)
	if err := leader.Register(leaderExec); err != nil {
		t.Fatalf("register leader: %v", err)
	}
	if err := standby.Register(standbyExec); err != nil {
		t.Fatalf("register standby: %v", err)
	}
	if err := leader.Start(ctx); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	waitForLeader(t, leader, true)
	if err := standby.Start(ctx); err != nil {
		t.Fatalf("start standby: %v", err)
	}
	t.Cleanup(func() {
		closeOnce(leaderExec.release)
		stopDispatcher(t, leader)
		stopDispatcher(t, standby)
	})

	waitForLeader(t, standby, false)

	first, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	leader.NotifyNewJob()
	if got := waitForStarted(t, leaderExec.started); got != first.ID {
		t.Fatalf("leader started %s, want %s", got, first.ID)
	}

	waitForLeader(t, standby, true)

	second, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s2"})
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	if got := waitForStartedWithNotifyAndStatus(t, standbyExec.started, standby.NotifyNewJob, store, second.ID, standby); got != second.ID {
		t.Fatalf("standby started %s, want %s", got, second.ID)
	}
	waitForStatus(t, store, second.ID, jobqueue.StatusCompleted)
}

type recordingExecutor struct {
	jobType   jobqueue.Type
	started   chan string
	failFirst bool
	cancel    bool
	mu        sync.Mutex
	calls     int
}

func (e *recordingExecutor) Type() jobqueue.Type {
	return e.jobType
}

func (e *recordingExecutor) Execute(ctx context.Context, job *jobqueue.Job) error {
	select {
	case e.started <- job.ID:
	case <-ctx.Done():
		return ctx.Err()
	}

	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()

	if e.failFirst && call == 1 {
		return fmt.Errorf("fail first")
	}
	if e.cancel {
		return jobqueue.Canceled("superseded")
	}
	return nil
}

type blockingExecutor struct {
	jobType jobqueue.Type
	started chan string
	release chan struct{}
}

func (e *blockingExecutor) Type() jobqueue.Type {
	return e.jobType
}

func (e *blockingExecutor) Execute(ctx context.Context, job *jobqueue.Job) error {
	select {
	case e.started <- job.ID:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newTestStore(t *testing.T) *memoryStore {
	t.Helper()
	return newMemoryStore()
}

func newTestDispatcher(store jobqueue.Store) *jobqueue.Dispatcher {
	return jobqueue.NewDispatcher(store, jobqueue.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       10 * time.Millisecond,
		JobTimeout:         time.Second,
		StaleJobTimeout:    2 * time.Second,
		RetryBackoff:       time.Millisecond,
		ImmediateExecution: true,
		DefaultConcurrency: 5,
	})
}

func newMultiNodeDispatcher(store jobqueue.Store, workerID string, heartbeatTimeout time.Duration) *jobqueue.Dispatcher {
	return newMultiNodeDispatcherWithHeartbeat(store, workerID, heartbeatTimeout, 20*time.Millisecond)
}

func newMultiNodeDispatcherWithHeartbeat(
	store jobqueue.Store,
	workerID string,
	heartbeatTimeout time.Duration,
	heartbeatInterval time.Duration,
) *jobqueue.Dispatcher {
	return newMultiNodeDispatcherCustom(store, workerID, heartbeatTimeout, heartbeatInterval, time.Hour)
}

func newMultiNodeDispatcherCustom(
	store jobqueue.Store,
	workerID string,
	heartbeatTimeout time.Duration,
	heartbeatInterval time.Duration,
	pollInterval time.Duration,
) *jobqueue.Dispatcher {
	return jobqueue.NewDispatcher(store, jobqueue.DispatcherConfig{
		WorkerID:           workerID,
		SingleNode:         false,
		PollInterval:       pollInterval,
		HeartbeatInterval:  heartbeatInterval,
		HeartbeatTimeout:   heartbeatTimeout,
		JobTimeout:         time.Second,
		StaleJobTimeout:    2 * time.Second,
		RetryBackoff:       time.Millisecond,
		ImmediateExecution: true,
		DefaultConcurrency: 5,
	})
}

func stopDispatcher(t *testing.T, dispatcher *jobqueue.Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dispatcher.DrainAndStop(ctx); err != nil {
		t.Fatalf("stop dispatcher: %v", err)
	}
}

func waitForStarted(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for executor start")
		return ""
	}
}

func waitForStartedWithNotifyAndStatus(
	t *testing.T,
	ch <-chan string,
	notify func(),
	store jobqueue.Store,
	jobID string,
	dispatcher *jobqueue.Dispatcher,
) string {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(time.Second)

	for {
		notify()
		select {
		case id := <-ch:
			return id
		case <-ticker.C:
		case <-deadline:
			job, err := store.GetJob(context.Background(), jobID)
			if err != nil {
				t.Fatalf("timed out waiting for executor start; get job: %v", err)
			}
			t.Fatalf("timed out waiting for executor start; leader=%v job status=%s worker=%v startedAt=%v error=%v", dispatcher.IsLeader(), job.Status, job.WorkerID, job.StartedAt, job.Error)
			return ""
		}
	}
}

func assertNoStart(t *testing.T, ch <-chan string, wait time.Duration) {
	t.Helper()
	select {
	case id := <-ch:
		t.Fatalf("unexpected job start: %s", id)
	case <-time.After(wait):
	}
}

func assertNoShutdown(t *testing.T, ch <-chan error, wait time.Duration) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("shutdown returned early: %v", err)
	case <-time.After(wait):
	}
}

func waitForLeader(t *testing.T, dispatcher *jobqueue.Dispatcher, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if dispatcher.IsLeader() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dispatcher %s IsLeader() = %v, want %v", dispatcher.WorkerID(), dispatcher.IsLeader(), want)
}

func waitForStatus(t *testing.T, store jobqueue.Store, id string, status jobqueue.Status) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, err := store.GetJob(context.Background(), id)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.Status == status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	t.Fatalf("job status = %s, want %s", job.Status, status)
}

func closeOnce(ch chan struct{}) {
	defer func() {
		_ = recover()
	}()
	close(ch)
}

type memoryStore struct {
	mu        sync.Mutex
	jobs      map[string]*jobqueue.Job
	active    map[string]string
	leader    string
	leaderAt  time.Time
	leaderSet bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]*jobqueue.Job{}, active: map[string]string{}}
}

func (s *memoryStore) Migrate(context.Context) error { return nil }

func (s *memoryStore) EnsureActiveJobForPayload(ctx context.Context, payload jobqueue.Payload, cfg jobqueue.QueueConfig) (*jobqueue.Job, bool, error) {
	resource := payload.Resource()
	allowDuplicates := false
	if d, ok := payload.(jobqueue.DuplicateAllower); ok {
		allowDuplicates = d.AllowDuplicates()
	}
	if !allowDuplicates && resource.Type != "" && resource.ID != "" {
		if existing := s.latestPendingForResource(resource); existing != nil {
			job, err := jobqueue.JobFromPayload(payload, cfg)
			if err != nil {
				return nil, false, err
			}
			s.mu.Lock()
			stored := s.jobs[existing.ID]
			stored.Type = job.Type
			stored.Payload = job.Payload
			stored.Priority = job.Priority
			stored.MaxAttempts = job.MaxAttempts
			stored.ScheduledAt = job.ScheduledAt
			stored.Resource = job.Resource
			stored.UpdatedAt = time.Now()
			out := cloneJob(stored)
			s.mu.Unlock()
			return out, false, nil
		}
	}
	job, err := jobqueue.JobFromPayload(payload, cfg)
	if err != nil {
		return nil, false, err
	}
	var opts []jobqueue.CreateJobOption
	if !allowDuplicates && resource.Type != "" && resource.ID != "" {
		opts = append(opts, jobqueue.WithUniqueResource())
	}
	if err := s.CreateJob(ctx, job, opts...); err != nil {
		if errors.Is(err, jobqueue.ErrJobAlreadyExists) && resource.Type != "" && resource.ID != "" {
			if existing := s.latestPendingForResource(resource); existing != nil {
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	return job, true, nil
}

func (s *memoryStore) CreateJob(_ context.Context, job *jobqueue.Job, options ...jobqueue.CreateJobOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts := jobqueue.ResolveCreateJobOptions(options...)
	now := time.Now()
	if job.ID == "" {
		id, err := jobqueue.NewID()
		if err != nil {
			return err
		}
		job.ID = id
	}
	if job.Status == "" {
		job.Status = jobqueue.StatusPending
	}
	if job.MaxAttempts < 1 {
		job.MaxAttempts = 1
	}
	if job.ScheduledAt.IsZero() {
		job.ScheduledAt = now
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if opts.UniqueResource && job.Resource.Type != "" && job.Resource.ID != "" {
		key := memoryResourceKey(job.Resource)
		if _, ok := s.active[key]; ok {
			return jobqueue.ErrJobAlreadyExists
		}
		s.active[key] = job.ID
	}
	cp := cloneJob(job)
	s.jobs[job.ID] = cp
	*job = *cloneJob(cp)
	return nil
}

func (s *memoryStore) GetJob(_ context.Context, id string) (*jobqueue.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return nil, jobqueue.ErrJobNotFound
	}
	return cloneJob(job), nil
}

func (s *memoryStore) GetLatestJobForResource(_ context.Context, resource jobqueue.Resource) (*jobqueue.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *jobqueue.Job
	for _, job := range s.jobs {
		if job.Resource == resource && (latest == nil || job.CreatedAt.After(latest.CreatedAt)) {
			latest = job
		}
	}
	if latest == nil {
		return nil, jobqueue.ErrJobNotFound
	}
	return cloneJob(latest), nil
}

func (s *memoryStore) HasActiveJobForResource(_ context.Context, resource jobqueue.Resource) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.Resource == resource && (job.Status == jobqueue.StatusPending || job.Status == jobqueue.StatusRunning) {
			return true, nil
		}
	}
	return false, nil
}

func (s *memoryStore) ClaimJob(_ context.Context, types []jobqueue.Type, workerID string) (*jobqueue.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(types) == 0 {
		return nil, nil
	}
	typeOK := map[jobqueue.Type]bool{}
	for _, typ := range types {
		typeOK[typ] = true
	}
	now := time.Now()
	var best *jobqueue.Job
	for _, job := range s.jobs {
		if !typeOK[job.Type] || job.Status != jobqueue.StatusPending || job.ScheduledAt.After(now) {
			continue
		}
		if job.Resource.Type != "" || job.Resource.ID != "" {
			blocked := false
			for _, other := range s.jobs {
				if other.ID != job.ID && other.Resource == job.Resource && other.Status == jobqueue.StatusRunning {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
		}
		if best == nil || job.Priority > best.Priority || (job.Priority == best.Priority && (job.ScheduledAt.Before(best.ScheduledAt) || (job.ScheduledAt.Equal(best.ScheduledAt) && job.CreatedAt.Before(best.CreatedAt)))) {
			best = job
		}
	}
	if best == nil {
		return nil, nil
	}
	best.Status = jobqueue.StatusRunning
	best.WorkerID = &workerID
	started := now
	best.StartedAt = &started
	best.Attempts++
	best.UpdatedAt = now
	delete(s.active, memoryResourceKey(best.Resource))
	return cloneJob(best), nil
}

func (s *memoryStore) CompleteJob(_ context.Context, id string) error {
	return s.finish(id, jobqueue.StatusCompleted, "")
}

func (s *memoryStore) CancelJob(_ context.Context, id string, message string) error {
	return s.finish(id, jobqueue.StatusCanceled, message)
}

func (s *memoryStore) FailJob(_ context.Context, id string, message string, retryBackoff time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return jobqueue.ErrJobNotFound
	}
	now := time.Now()
	job.Error = &message
	if job.Attempts < job.MaxAttempts {
		job.Status = jobqueue.StatusPending
		job.WorkerID = nil
		job.StartedAt = nil
		job.ScheduledAt = now.Add(time.Duration(job.Attempts) * retryBackoff)
		if job.Resource.Type != "" && job.Resource.ID != "" {
			s.active[memoryResourceKey(job.Resource)] = job.ID
		}
	} else {
		job.Status = jobqueue.StatusFailed
		completed := now
		job.CompletedAt = &completed
		delete(s.active, memoryResourceKey(job.Resource))
	}
	job.UpdatedAt = now
	return nil
}

func (s *memoryStore) CleanupStaleJobs(_ context.Context, staleAfter time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-staleAfter)
	var count int64
	for _, job := range s.jobs {
		if job.Status == jobqueue.StatusRunning && job.StartedAt != nil && !job.StartedAt.After(cutoff) {
			job.Status = jobqueue.StatusPending
			job.WorkerID = nil
			job.StartedAt = nil
			job.UpdatedAt = time.Now()
			count++
		}
	}
	return count, nil
}

func (s *memoryStore) TryAcquireLeadership(_ context.Context, workerID string, timeout time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.leaderSet || s.leader == workerID || s.leaderAt.Before(now.Add(-timeout)) {
		s.leader = workerID
		s.leaderAt = now
		s.leaderSet = true
		return true, nil
	}
	return false, nil
}

func (s *memoryStore) ReleaseLeadership(_ context.Context, workerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leaderSet && s.leader == workerID {
		s.leaderSet = false
		s.leader = ""
	}
	return nil
}

func (s *memoryStore) finish(id string, status jobqueue.Status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return jobqueue.ErrJobNotFound
	}
	now := time.Now()
	job.Status = status
	if message != "" {
		job.Error = &message
	}
	job.CompletedAt = &now
	job.UpdatedAt = now
	delete(s.active, memoryResourceKey(job.Resource))
	return nil
}

func (s *memoryStore) latestPendingForResource(resource jobqueue.Resource) *jobqueue.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *jobqueue.Job
	for _, job := range s.jobs {
		if job.Resource == resource && job.Status == jobqueue.StatusPending && (latest == nil || job.CreatedAt.After(latest.CreatedAt)) {
			latest = job
		}
	}
	return cloneJob(latest)
}

func memoryResourceKey(resource jobqueue.Resource) string {
	return resource.Type + "\x00" + resource.ID
}

func cloneJob(job *jobqueue.Job) *jobqueue.Job {
	if job == nil {
		return nil
	}
	cp := *job
	if job.Payload != nil {
		cp.Payload = append([]byte(nil), job.Payload...)
	}
	if job.Error != nil {
		v := *job.Error
		cp.Error = &v
	}
	if job.WorkerID != nil {
		v := *job.WorkerID
		cp.WorkerID = &v
	}
	if job.StartedAt != nil {
		v := *job.StartedAt
		cp.StartedAt = &v
	}
	if job.CompletedAt != nil {
		v := *job.CompletedAt
		cp.CompletedAt = &v
	}
	return &cp
}
