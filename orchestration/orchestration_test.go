package orchestration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obot-platform/discobox/orchestration"
)

const (
	testTypeA orchestration.Type = "test.a"
	testTypeB orchestration.Type = "test.b"
)

type testPayload struct {
	TypeName  orchestration.Type `json:"-"`
	ResourceT string             `json:"-"`
	ResourceI string             `json:"-"`
	Value     string             `json:"value,omitempty"`
	PriorityV *int               `json:"-"`
	MaxV      *int               `json:"-"`
	At        time.Time          `json:"-"`
}

func (p testPayload) JobType() orchestration.Type {
	return p.TypeName
}

func (p testPayload) Resource() orchestration.Resource {
	return orchestration.Resource{Type: p.ResourceT, ID: p.ResourceI}
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
	TypeName  orchestration.Type `json:"-"`
	ResourceT string             `json:"-"`
	ResourceI string             `json:"-"`
	Value     string             `json:"value,omitempty"`
}

func (p simplePayload) JobType() orchestration.Type {
	return p.TypeName
}

func (p simplePayload) Resource() orchestration.Resource {
	return orchestration.Resource{Type: p.ResourceT, ID: p.ResourceI}
}

type unmarshalablePayload struct {
	C chan struct{} `json:"c"`
}

func (p unmarshalablePayload) JobType() orchestration.Type {
	return testTypeA
}

func (p unmarshalablePayload) Resource() orchestration.Resource {
	return orchestration.Resource{}
}

type lifecycleResource struct {
	ID         string
	Operation  string
	Generation int64
	LastJobID  *string
	Value      string
	Reloaded   bool
}

func (r *lifecycleResource) BeginOperation(operation string, _ *string) {
	r.Operation = operation
}

func (r *lifecycleResource) IncrementGeneration() {
	r.Generation++
}

func (r *lifecycleResource) SetLastJobID(jobID *string) {
	r.LastJobID = jobID
}

func TestQueueEnqueueAppliesDefaultsAndPayloadOptions(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{
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
	if stored.Resource != (orchestration.Resource{Type: "sandbox", ID: "s1"}) {
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

func TestQueueEnqueueAppliesResourceBackoff(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{
		DefaultMaxAttempts:       1,
		ResourceBackoffThreshold: 2,
		ResourceBackoffWindow:    time.Minute,
		ResourceBackoffBaseDelay: time.Minute,
		ResourceBackoffMaxDelay:  10 * time.Minute,
	})
	payload := simplePayload{TypeName: testTypeA, ResourceT: "worker", ResourceI: "worker-1"}

	first, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	if err := store.CompleteJob(ctx, first.ID, orchestration.JobResult{}); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	second, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue second: %v", err)
	}
	if err := store.CompleteJob(ctx, second.ID, orchestration.JobResult{}); err != nil {
		t.Fatalf("complete second: %v", err)
	}
	third, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue third: %v", err)
	}

	if first.Status != orchestration.StatusPending || second.Status != orchestration.StatusPending {
		t.Fatalf("first/second status = %s/%s, want pending/pending", first.Status, second.Status)
	}
	if third.Status != orchestration.StatusBackoff {
		t.Fatalf("third status = %s, want backoff", third.Status)
	}
	if !third.ScheduledAt.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("third scheduledAt = %s, want delayed", third.ScheduledAt)
	}
}

func TestQueueEnqueueSupersedesQueuedJobForTypeAndResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})
	payload := simplePayload{TypeName: testTypeA, ResourceT: "worker", ResourceI: "worker-1"}

	first, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	second, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue successor: %v", err)
	}
	if second == nil {
		t.Fatal("successor queued job is nil")
	}

	stored, err := store.GetJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if stored.Status != orchestration.StatusCanceled {
		t.Fatalf("first status = %s, want canceled", stored.Status)
	}
	storedSecond, err := store.GetJob(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if storedSecond.Status != orchestration.StatusPending {
		t.Fatalf("second status = %s, want pending", storedSecond.Status)
	}
}

func TestQueueEnqueueAllowsQueuedJobBehindRunningJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 2})
	payload := simplePayload{TypeName: testTypeA, ResourceT: "worker", ResourceI: "worker-1"}

	first, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA}, "worker-1")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if claimed == nil || claimed.ID != first.ID {
		t.Fatalf("claimed = %#v, want %s", claimed, first.ID)
	}

	second, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue successor: %v", err)
	}
	if second == nil {
		t.Fatal("successor queued behind running job is nil")
	}
	third, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue duplicate successor: %v", err)
	}
	if third == nil {
		t.Fatal("replacement successor is nil")
	}
	storedSecond, err := store.GetJob(ctx, second.ID)
	if err != nil {
		t.Fatalf("get second: %v", err)
	}
	if storedSecond.Status != orchestration.StatusCanceled {
		t.Fatalf("second status = %s, want canceled", storedSecond.Status)
	}
}

func TestFailJobCancelsRetryWhenQueuedSuccessorExists(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 2})
	payload := simplePayload{TypeName: testTypeA, ResourceT: "worker", ResourceI: "worker-1"}

	first, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue first: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA}, "worker-1")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if claimed == nil || claimed.ID != first.ID {
		t.Fatalf("claimed = %#v, want %s", claimed, first.ID)
	}
	successor, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue successor: %v", err)
	}
	if successor == nil {
		t.Fatal("successor queued behind running job is nil")
	}
	if err := store.FailJob(ctx, first.ID, "try again", orchestration.JobResult{}, time.Minute); err != nil {
		t.Fatalf("fail first: %v", err)
	}
	stored, err := store.GetJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if stored.Status != orchestration.StatusCanceled {
		t.Fatalf("first status = %s, want canceled", stored.Status)
	}
}

func TestQueueEnqueueDefaultResourceBackoffUsesLongWindowAndCap(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})
	payload := simplePayload{TypeName: "workerprovider.reconcile", ResourceT: "workerprovider", ResourceI: "provider-1"}

	for i := range 10 {
		job, err := queue.Enqueue(ctx, payload)
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		if job.Status != orchestration.StatusPending {
			t.Fatalf("job %d status = %s, want pending", i, job.Status)
		}
		if err := store.CompleteJob(ctx, job.ID, orchestration.JobResult{}); err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}
	backoff, err := queue.Enqueue(ctx, payload)
	if err != nil {
		t.Fatalf("enqueue backoff: %v", err)
	}
	if backoff.Status != orchestration.StatusBackoff {
		t.Fatalf("backoff status = %s, want backoff", backoff.Status)
	}
	if backoff.ScheduledAt.Before(time.Now().Add(25 * time.Second)) {
		t.Fatalf("backoff scheduledAt = %s, want default delay", backoff.ScheduledAt)
	}
	if got := orchestration.ResourceBackoffDelay(10, 30*time.Second, 15*time.Minute); got != 15*time.Minute {
		t.Fatalf("default capped delay = %s, want 15m", got)
	}
}

func TestQueueEnqueueReturnsMarshalError(t *testing.T) {
	queue := orchestration.NewQueue(newTestStore(t), orchestration.QueueConfig{})
	if _, err := queue.Enqueue(context.Background(), unmarshalablePayload{C: make(chan struct{})}); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestDispatcherSubmitCanRunInCallerTransaction(t *testing.T) {
	ctx := context.Background()
	baseStore := newTestStore(t)
	txStore := newTestStore(t)
	dispatcher := orchestration.NewDispatcher(baseStore, orchestration.DispatcherConfig{})
	resource := &lifecycleResource{ID: "resource-1"}

	txCalled := false
	persisted := false
	job, err := dispatcher.Submit(ctx,
		simplePayload{TypeName: testTypeA, ResourceT: "resource", ResourceI: resource.ID},
		orchestration.WithQueueConfig(orchestration.QueueConfig{DefaultMaxAttempts: 3}),
		orchestration.WithSubmitTransaction(func(ctx context.Context, fn orchestration.SubmitAppendFunc) (*orchestration.Job, error) {
			txCalled = true
			resource.IncrementGeneration()
			job, err := fn(ctx, txStore, simplePayload{TypeName: testTypeA, ResourceT: "resource", ResourceI: resource.ID})
			if err != nil {
				return nil, err
			}
			if job == nil {
				return nil, errors.New("submit callback returned nil job")
			}
			resource.SetLastJobID(&job.ID)
			persisted = true
			return job, nil
		}),
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !txCalled {
		t.Fatal("expected transaction to run")
	}
	if !persisted {
		t.Fatal("expected resource persist callback")
	}
	if resource.Generation != 1 {
		t.Fatalf("generation = %d, want 1", resource.Generation)
	}
	if resource.LastJobID == nil || *resource.LastJobID != job.ID {
		t.Fatalf("last job ID = %v, job ID = %s", resource.LastJobID, job.ID)
	}
	stored, err := txStore.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get transaction job: %v", err)
	}
	if stored.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d, want 3", stored.MaxAttempts)
	}
	if _, err := baseStore.GetJob(ctx, job.ID); !errors.Is(err, orchestration.ErrJobNotFound) {
		t.Fatalf("base store get err = %v, want ErrJobNotFound", err)
	}
}

func TestDispatcherSubmitAppendsWithoutTransaction(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	dispatcher := orchestration.NewDispatcher(store, orchestration.DispatcherConfig{})

	job, err := dispatcher.Submit(ctx,
		simplePayload{TypeName: testTypeA, ResourceT: "resource", ResourceI: "resource-1"},
		orchestration.WithQueueConfig(orchestration.QueueConfig{DefaultMaxAttempts: 2}),
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.MaxAttempts != 2 {
		t.Fatalf("max attempts = %d, want 2", stored.MaxAttempts)
	}
}

func TestQueueEnqueueAppendsJobsAcrossJobTypesByResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 3})

	first, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	second, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeB, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("expected appended job with distinct ID")
	}
}

func TestQueueEnqueueKeepsOneQueuedJobByTypeAndResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	const count = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var jobs []*orchestration.Job
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
	seen := map[string]bool{}
	for _, job := range jobs {
		if seen[job.ID] {
			t.Fatalf("duplicate job id %s", job.ID)
		}
		seen[job.ID] = true
	}
	queued := 0
	for _, job := range jobs {
		stored, err := store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("get job %s: %v", job.ID, err)
		}
		if stored.Status == orchestration.StatusPending || stored.Status == orchestration.StatusBackoff {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("queued jobs = %d, want 1", queued)
	}

	if _, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeB, ResourceT: "sandbox", ResourceI: "shared"}); err != nil {
		t.Fatalf("enqueue additional job: %v", err)
	}
}

func TestQueueAppendsJobsAndSerializesExecutionByResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 3})

	first, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	second, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeB, ResourceT: "sandbox", ResourceI: "s1"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("expected separate jobs")
	}

	claimed, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA, testTypeB}, "worker")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected first claim")
	}

	blocked, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA, testTypeB}, "worker")
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
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

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

	queue := orchestration.NewQueue(store, orchestration.QueueConfig{})
	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "zero-default"})
	if err != nil {
		t.Fatalf("enqueue zero default: %v", err)
	}
	if job.MaxAttempts != 1 {
		t.Fatalf("zero default max attempts = %d, want 1", job.MaxAttempts)
	}

	negativeQueue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: -10})
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

	future := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		Priority:    100,
		MaxAttempts: 1,
		ScheduledAt: time.Now().Add(time.Hour),
		Resource:    orchestration.Resource{Type: "sandbox", ID: "future"},
	}
	low := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		Priority:    1,
		MaxAttempts: 1,
		Resource:    orchestration.Resource{Type: "sandbox", ID: "low"},
	}
	high := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		Priority:    10,
		MaxAttempts: 1,
		Resource:    orchestration.Resource{Type: "sandbox", ID: "high"},
	}
	for _, job := range []*orchestration.Job{future, low, high} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}

	claimed, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA}, "worker")
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
	job := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		MaxAttempts: 1,
		Resource:    orchestration.Resource{Type: "sandbox", ID: "s1"},
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

	job := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		MaxAttempts: 2,
		Resource:    orchestration.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	claimed, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA}, "worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.FailJob(ctx, claimed.ID, "try again", orchestration.JobResult{}, 0); err != nil {
		t.Fatalf("fail first: %v", err)
	}
	retry, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get retry: %v", err)
	}
	if retry.Status != orchestration.StatusPending {
		t.Fatalf("status after first fail = %s, want pending", retry.Status)
	}
	if retry.Attempts != 1 {
		t.Fatalf("attempts after first fail = %d, want 1", retry.Attempts)
	}

	claimed, err = store.ClaimJob(ctx, []orchestration.Type{testTypeA}, "worker")
	if err != nil {
		t.Fatalf("claim retry: %v", err)
	}
	if err := store.FailJob(ctx, claimed.ID, "done", orchestration.JobResult{}, 0); err != nil {
		t.Fatalf("fail second: %v", err)
	}
	failed, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if failed.Status != orchestration.StatusFailed {
		t.Fatalf("status after second fail = %s, want failed", failed.Status)
	}
	if failed.CompletedAt == nil {
		t.Fatal("expected completedAt on failed job")
	}
}

func TestStoreGetLatestJobForResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	resource := orchestration.Resource{Type: "sandbox", ID: "s1"}

	first := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		MaxAttempts: 1,
		Resource:    resource,
	}
	if err := store.CreateJob(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	second := &orchestration.Job{
		Type:        testTypeB,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
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

	job := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		MaxAttempts: 1,
		Resource:    orchestration.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA}, "worker")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Status != orchestration.StatusRunning {
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
	if recovered.Status != orchestration.StatusPending {
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

	if err := dispatcher.Register(testTypeA, nil); err == nil {
		t.Fatal("expected nil executor to be rejected")
	}
	exec := &recordingExecutor{started: make(chan string, 1)}
	if err := dispatcher.Register(testTypeA, exec); err != nil {
		t.Fatalf("register first executor: %v", err)
	}
	if err := dispatcher.Register(testTypeA, &recordingExecutor{started: make(chan string, 1)}); !errors.Is(err, orchestration.ErrExecutorAlreadyRegistered) {
		t.Fatalf("duplicate register error = %v, want ErrExecutorAlreadyRegistered", err)
	}
}

func TestDispatcherExecutesAndCompletesJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &recordingExecutor{
		started: make(chan string, 1),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec); err != nil {
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
	waitForStatus(t, store, job.ID, orchestration.StatusCompleted)

	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get completed job: %v", err)
	}
	if stored.Message == nil || *stored.Message != "completed" {
		t.Fatalf("message = %v, want completed", stored.Message)
	}
	if string(stored.Metadata) != `{"ok":true}` {
		t.Fatalf("metadata = %s, want ok metadata", stored.Metadata)
	}
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
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	dispatcher := orchestration.NewDispatcher(store, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       10 * time.Millisecond,
		JobTimeout:         20 * time.Millisecond,
		StaleJobTimeout:    time.Second,
		RetryBackoff:       time.Millisecond,
		ImmediateExecution: true,
		DefaultConcurrency: 1,
	})
	if err := dispatcher.Register(testTypeA, exec); err != nil {
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
	waitForStatus(t, store, job.ID, orchestration.StatusFailed)
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
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &recordingExecutor{
		started: make(chan string, 1),
		cancel:  true,
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec); err != nil {
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
	waitForStatus(t, store, job.ID, orchestration.StatusCanceled)
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Error == nil || *stored.Error != "superseded" {
		t.Fatalf("job error = %v, want superseded", stored.Error)
	}
}

func TestDispatcherGenerationAssertionCancelsBeforeExecute(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &generationAssertingExecutor{
		assertErr: orchestration.Superseded("generation changed"),
		executed:  make(chan string, 1),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	queue.SetNotifyFunc(dispatcher.NotifyNewJob)
	if err := dispatcher.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		stopDispatcher(t, dispatcher)
	})

	job, err := queue.Enqueue(ctx, simplePayload{TypeName: testTypeA, ResourceT: "sandbox", ResourceI: "stale"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitForStatus(t, store, job.ID, orchestration.StatusCanceled)

	select {
	case executed := <-exec.executed:
		t.Fatalf("Execute called for %s; generation assertion should cancel first", executed)
	default:
	}

	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Error == nil || *stored.Error != "generation changed" {
		t.Fatalf("job error = %v, want generation changed", stored.Error)
	}
}

func TestDispatcherRetriesFailedJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 2})

	exec := &recordingExecutor{
		started:   make(chan string, 2),
		failFirst: true,
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec); err != nil {
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
	waitForStatus(t, store, job.ID, orchestration.StatusCompleted)

	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", stored.Attempts)
	}
}

func TestDispatcherNotifiesTerminalObserverAfterRetryCompletes(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 2})

	exec := &terminalRecordingExecutor{
		recordingExecutor: recordingExecutor{
			started:   make(chan string, 2),
			failFirst: true,
		},
		terminal: make(chan orchestration.Status, 1),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec); err != nil {
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
	select {
	case status := <-exec.terminal:
		t.Fatalf("terminal observer fired during retry with status %s", status)
	default:
	}
	waitForStarted(t, exec.started)
	waitForStatus(t, store, job.ID, orchestration.StatusCompleted)
	if status := waitForTerminalStatus(t, exec.terminal); status != orchestration.StatusCompleted {
		t.Fatalf("terminal status = %s, want completed", status)
	}
}

func TestDispatcherNotifiesTerminalObserverAfterExhaustedFailure(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &terminalRecordingExecutor{
		recordingExecutor: recordingExecutor{
			started:   make(chan string, 1),
			failFirst: true,
		},
		terminal: make(chan orchestration.Status, 1),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec); err != nil {
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
	waitForStatus(t, store, job.ID, orchestration.StatusFailed)
	if status := waitForTerminalStatus(t, exec.terminal); status != orchestration.StatusFailed {
		t.Fatalf("terminal status = %s, want failed", status)
	}
}

func TestDispatcherRunsMultipleJobsWithoutResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec, orchestration.WithConcurrency(2)); err != nil {
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
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec, orchestration.WithConcurrency(1)); err != nil {
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
	dispatcher := orchestration.NewDispatcher(store, orchestration.DispatcherConfig{
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

	job := &orchestration.Job{
		Type:        testTypeA,
		Payload:     []byte(`{}`),
		Status:      orchestration.StatusPending,
		MaxAttempts: 1,
		Resource:    orchestration.Resource{Type: "sandbox", ID: "stale-loop"},
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []orchestration.Type{testTypeA}, "abandoned-worker")
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected claimed job")
	}
	waitForStatus(t, store, job.ID, orchestration.StatusPending)
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
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	exec := &blockingExecutor{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	dispatcher := newTestDispatcher(store)
	if err := dispatcher.Register(testTypeA, exec); err != nil {
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
	if storedSecond.Status != orchestration.StatusPending {
		t.Fatalf("second status = %s, want pending", storedSecond.Status)
	}
}

func TestDispatcherMultiNodeOnlyLeaderExecutesJobs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	leaderExec := &recordingExecutor{started: make(chan string, 1)}
	standbyExec := &recordingExecutor{started: make(chan string, 1)}

	leader := newMultiNodeDispatcher(store, "worker-1", time.Minute)
	standby := newMultiNodeDispatcher(store, "worker-2", time.Minute)
	if err := leader.Register(testTypeA, leaderExec); err != nil {
		t.Fatalf("register leader: %v", err)
	}
	if err := standby.Register(testTypeA, standbyExec); err != nil {
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
	waitForStatus(t, store, job.ID, orchestration.StatusCompleted)
}

func TestDispatcherMultiNodeDrainReleasesLeadership(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	first := newMultiNodeDispatcher(store, "worker-1", time.Minute)
	if err := first.Register(testTypeA, &recordingExecutor{started: make(chan string, 1)}); err != nil {
		t.Fatalf("register first: %v", err)
	}
	if err := first.Start(ctx); err != nil {
		t.Fatalf("start first: %v", err)
	}
	waitForLeader(t, first, true)

	stopDispatcher(t, first)

	second := newMultiNodeDispatcher(store, "worker-2", time.Minute)
	if err := second.Register(testTypeA, &recordingExecutor{started: make(chan string, 1)}); err != nil {
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
	queue := orchestration.NewQueue(store, orchestration.QueueConfig{DefaultMaxAttempts: 1})

	leaderExec := &blockingExecutor{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	standbyExec := &recordingExecutor{started: make(chan string, 1)}

	leader := newMultiNodeDispatcherCustom(store, "worker-1", 120*time.Millisecond, time.Hour, time.Hour)
	standby := newMultiNodeDispatcherCustom(store, "worker-2", 120*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond)
	if err := leader.Register(testTypeA, leaderExec); err != nil {
		t.Fatalf("register leader: %v", err)
	}
	if err := standby.Register(testTypeA, standbyExec); err != nil {
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
	waitForStatus(t, store, second.ID, orchestration.StatusCompleted)
}

type recordingExecutor struct {
	started   chan string
	failFirst bool
	cancel    bool
	result    orchestration.JobResult
	mu        sync.Mutex
	calls     int
}

func (e *recordingExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
	select {
	case e.started <- job.ID:
	case <-ctx.Done():
		return orchestration.JobResult{}, ctx.Err()
	}

	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()

	if e.failFirst && call == 1 {
		return orchestration.JobResult{}, fmt.Errorf("fail first")
	}
	if e.cancel {
		return orchestration.JobResult{}, orchestration.Canceled("superseded")
	}
	if e.result.Message != nil || e.result.Metadata != nil {
		return e.result, nil
	}
	return orchestration.JobResult{Message: ptr("completed"), Metadata: []byte(`{"ok":true}`)}, nil
}

type terminalRecordingExecutor struct {
	recordingExecutor
	terminal chan orchestration.Status
}

func (e *terminalRecordingExecutor) OnTerminal(_ context.Context, job *orchestration.Job) error {
	select {
	case e.terminal <- job.Status:
	default:
	}
	return nil
}

type generationAssertingExecutor struct {
	assertErr error
	executed  chan string
}

func (e *generationAssertingExecutor) AssertGeneration(context.Context, *orchestration.Job) error {
	return e.assertErr
}

func (e *generationAssertingExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
	select {
	case e.executed <- job.ID:
	case <-ctx.Done():
		return orchestration.JobResult{}, ctx.Err()
	}
	return orchestration.JobResult{}, nil
}

type blockingExecutor struct {
	started chan string
	release chan struct{}
}

func (e *blockingExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
	select {
	case e.started <- job.ID:
	case <-ctx.Done():
		return orchestration.JobResult{}, ctx.Err()
	}

	select {
	case <-e.release:
		return orchestration.JobResult{}, nil
	case <-ctx.Done():
		return orchestration.JobResult{}, ctx.Err()
	}
}

func newTestStore(t *testing.T) *memoryStore {
	t.Helper()
	return newMemoryStore()
}

func newTestDispatcher(store orchestration.Store) *orchestration.Dispatcher {
	return orchestration.NewDispatcher(store, orchestration.DispatcherConfig{
		SingleNode:         true,
		PollInterval:       10 * time.Millisecond,
		JobTimeout:         time.Second,
		StaleJobTimeout:    2 * time.Second,
		RetryBackoff:       time.Millisecond,
		ImmediateExecution: true,
		DefaultConcurrency: 5,
	})
}

func newMultiNodeDispatcher(store orchestration.Store, workerID string, heartbeatTimeout time.Duration) *orchestration.Dispatcher {
	return newMultiNodeDispatcherWithHeartbeat(store, workerID, heartbeatTimeout, 20*time.Millisecond)
}

func newMultiNodeDispatcherWithHeartbeat(
	store orchestration.Store,
	workerID string,
	heartbeatTimeout time.Duration,
	heartbeatInterval time.Duration,
) *orchestration.Dispatcher {
	return newMultiNodeDispatcherCustom(store, workerID, heartbeatTimeout, heartbeatInterval, time.Hour)
}

func newMultiNodeDispatcherCustom(
	store orchestration.Store,
	workerID string,
	heartbeatTimeout time.Duration,
	heartbeatInterval time.Duration,
	pollInterval time.Duration,
) *orchestration.Dispatcher {
	return orchestration.NewDispatcher(store, orchestration.DispatcherConfig{
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

func stopDispatcher(t *testing.T, dispatcher *orchestration.Dispatcher) {
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
	store orchestration.Store,
	jobID string,
	dispatcher *orchestration.Dispatcher,
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

func waitForLeader(t *testing.T, dispatcher *orchestration.Dispatcher, want bool) {
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

func waitForStatus(t *testing.T, store orchestration.Store, id string, status orchestration.Status) {
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

func waitForTerminalStatus(t *testing.T, ch <-chan orchestration.Status) orchestration.Status {
	t.Helper()
	select {
	case status := <-ch:
		return status
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal observer")
		return ""
	}
}

func closeOnce(ch chan struct{}) {
	defer func() {
		_ = recover()
	}()
	close(ch)
}

type memoryStore struct {
	mu        sync.Mutex
	jobs      map[string]*orchestration.Job
	active    map[string]string
	leader    string
	leaderAt  time.Time
	leaderSet bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]*orchestration.Job{}, active: map[string]string{}}
}

func (s *memoryStore) Migrate(context.Context) error { return nil }

func (s *memoryStore) CreateJob(_ context.Context, job *orchestration.Job, options ...orchestration.CreateJobOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	opts := orchestration.ResolveCreateJobOptions(options...)
	now := time.Now()
	if job.ID == "" {
		id, err := orchestration.NewID()
		if err != nil {
			return err
		}
		job.ID = id
	}
	if job.Status == "" {
		job.Status = orchestration.StatusPending
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
	if job.Resource.Type != "" && job.Resource.ID != "" && claimableStatus(job.Status) {
		for _, existing := range s.jobs {
			if existing.Type == job.Type && existing.Resource == job.Resource && claimableStatus(existing.Status) {
				existing.Status = orchestration.StatusCanceled
				message := "superseded by newer queued job"
				existing.Message = &message
				completed := now
				existing.CompletedAt = &completed
				existing.UpdatedAt = now
				delete(s.active, memoryResourceKey(existing.Resource))
			}
		}
	}
	if opts.UniqueResource && job.Resource.Type != "" && job.Resource.ID != "" {
		key := memoryResourceKey(job.Resource)
		if _, ok := s.active[key]; ok {
			return orchestration.ErrJobAlreadyExists
		}
		s.active[key] = job.ID
	}
	cp := cloneJob(job)
	s.jobs[job.ID] = cp
	*job = *cloneJob(cp)
	return nil
}

func (s *memoryStore) GetJob(_ context.Context, id string) (*orchestration.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return nil, orchestration.ErrJobNotFound
	}
	return cloneJob(job), nil
}

func (s *memoryStore) GetLatestJobForResource(_ context.Context, resource orchestration.Resource) (*orchestration.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *orchestration.Job
	for _, job := range s.jobs {
		if job.Resource == resource && (latest == nil || job.CreatedAt.After(latest.CreatedAt)) {
			latest = job
		}
	}
	if latest == nil {
		return nil, orchestration.ErrJobNotFound
	}
	return cloneJob(latest), nil
}

func (s *memoryStore) CountRecentJobsForResource(_ context.Context, jobType orchestration.Type, resource orchestration.Resource, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	for _, job := range s.jobs {
		if job.Type == jobType && job.Resource == resource && !job.CreatedAt.Before(since) {
			count++
		}
	}
	return count, nil
}

func (s *memoryStore) HasActiveJobForResource(_ context.Context, resource orchestration.Resource) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		if job.Resource == resource && (job.Status == orchestration.StatusPending || job.Status == orchestration.StatusBackoff || job.Status == orchestration.StatusRunning) {
			return true, nil
		}
	}
	return false, nil
}

func (s *memoryStore) ClaimJob(_ context.Context, types []orchestration.Type, workerID string) (*orchestration.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(types) == 0 {
		return nil, nil
	}
	typeOK := map[orchestration.Type]bool{}
	for _, typ := range types {
		typeOK[typ] = true
	}
	now := time.Now()
	var best *orchestration.Job
	for _, job := range s.jobs {
		if !typeOK[job.Type] || !claimableStatus(job.Status) || job.ScheduledAt.After(now) {
			continue
		}
		if job.Resource.Type != "" || job.Resource.ID != "" {
			blocked := false
			for _, other := range s.jobs {
				if other.ID != job.ID && other.Resource == job.Resource && other.Status == orchestration.StatusRunning {
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
	best.Status = orchestration.StatusRunning
	best.WorkerID = &workerID
	started := now
	best.StartedAt = &started
	best.Attempts++
	best.UpdatedAt = now
	delete(s.active, memoryResourceKey(best.Resource))
	return cloneJob(best), nil
}

func (s *memoryStore) CompleteJob(_ context.Context, id string, result orchestration.JobResult) error {
	return s.finish(id, orchestration.StatusCompleted, "", result)
}

func (s *memoryStore) CancelJob(_ context.Context, id string, result orchestration.JobResult) error {
	message := ""
	if result.Message != nil {
		message = *result.Message
	}
	return s.finish(id, orchestration.StatusCanceled, message, result)
}

func (s *memoryStore) FailJob(_ context.Context, id string, message string, result orchestration.JobResult, retryBackoff time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return orchestration.ErrJobNotFound
	}
	now := time.Now()
	job.Error = &message
	job.Message = result.Message
	job.Metadata = result.Metadata
	if job.Attempts < job.MaxAttempts {
		if job.Resource.Type != "" && job.Resource.ID != "" {
			for _, existing := range s.jobs {
				if existing.ID != id && existing.Type == job.Type && existing.Resource == job.Resource && claimableStatus(existing.Status) {
					job.Status = orchestration.StatusCanceled
					completed := now
					job.CompletedAt = &completed
					delete(s.active, memoryResourceKey(job.Resource))
					job.UpdatedAt = now
					return nil
				}
			}
		}
		job.Status = orchestration.StatusPending
		job.WorkerID = nil
		job.StartedAt = nil
		job.ScheduledAt = now.Add(retryBackoff)
		if job.Resource.Type != "" && job.Resource.ID != "" {
			s.active[memoryResourceKey(job.Resource)] = job.ID
		}
	} else {
		job.Status = orchestration.StatusFailed
		completed := now
		job.CompletedAt = &completed
		delete(s.active, memoryResourceKey(job.Resource))
	}
	job.UpdatedAt = now
	return nil
}

func claimableStatus(status orchestration.Status) bool {
	return status == orchestration.StatusPending || status == orchestration.StatusBackoff
}

func (s *memoryStore) CleanupStaleJobs(_ context.Context, staleAfter time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-staleAfter)
	var count int64
	for _, job := range s.jobs {
		if job.Status == orchestration.StatusRunning && job.StartedAt != nil && !job.StartedAt.After(cutoff) {
			job.Status = orchestration.StatusPending
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

func (s *memoryStore) finish(id string, status orchestration.Status, message string, result orchestration.JobResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return orchestration.ErrJobNotFound
	}
	now := time.Now()
	job.Status = status
	if message != "" {
		job.Error = &message
	}
	job.Message = result.Message
	job.Metadata = result.Metadata
	job.CompletedAt = &now
	job.UpdatedAt = now
	delete(s.active, memoryResourceKey(job.Resource))
	return nil
}

func memoryResourceKey(resource orchestration.Resource) string {
	return resource.Type + "\x00" + resource.ID
}

func ptr[T any](value T) *T {
	return &value
}

func cloneJob(job *orchestration.Job) *orchestration.Job {
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
	if job.Message != nil {
		v := *job.Message
		cp.Message = &v
	}
	if job.Metadata != nil {
		cp.Metadata = append([]byte(nil), job.Metadata...)
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
