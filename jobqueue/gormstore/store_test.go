package gormstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/obot-platform/disco2/jobqueue"
	"github.com/obot-platform/disco2/jobqueue/gormstore"
)

const testType jobqueue.Type = "test.job"

type testPayload struct {
	ResourceID string `json:"resourceId"`
	Value      string `json:"value,omitempty"`
	Dupe       bool   `json:"-"`
}

func (p testPayload) JobType() jobqueue.Type {
	return testType
}

func (p testPayload) Resource() jobqueue.Resource {
	return jobqueue.Resource{Type: "sandbox", ID: p.ResourceID}
}

func (p testPayload) AllowDuplicates() bool {
	return p.Dupe
}

func TestStoreUniqueResourceBlocksUntilTerminalState(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	first := &jobqueue.Job{
		Type:        testType,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, first, jobqueue.WithUniqueResource()); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := &jobqueue.Job{
		Type:        testType,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, second, jobqueue.WithUniqueResource()); !errors.Is(err, jobqueue.ErrJobAlreadyExists) {
		t.Fatalf("create duplicate error = %v, want ErrJobAlreadyExists", err)
	}
	if err := store.CompleteJob(ctx, first.ID); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	if err := store.CreateJob(ctx, second, jobqueue.WithUniqueResource()); err != nil {
		t.Fatalf("create after complete: %v", err)
	}
}

func TestStoreEnsureActiveJobForPayloadReusesActiveJob(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	payload := testPayload{ResourceID: "s1"}

	first, created, err := store.EnsureActiveJobForPayload(ctx, payload, jobqueue.QueueConfig{DefaultMaxAttempts: 3})
	if err != nil {
		t.Fatalf("ensure first: %v", err)
	}
	if !created {
		t.Fatal("expected first ensure to create job")
	}
	if first.MaxAttempts != 3 {
		t.Fatalf("max attempts = %d, want 3", first.MaxAttempts)
	}

	second, created, err := store.EnsureActiveJobForPayload(ctx, payload, jobqueue.QueueConfig{DefaultMaxAttempts: 3})
	if err != nil {
		t.Fatalf("ensure second: %v", err)
	}
	if created {
		t.Fatal("expected second ensure to reuse active job")
	}
	if second.ID != first.ID {
		t.Fatalf("second job = %s, want first job %s", second.ID, first.ID)
	}

	if err := store.CompleteJob(ctx, first.ID); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	third, created, err := store.EnsureActiveJobForPayload(ctx, payload, jobqueue.QueueConfig{DefaultMaxAttempts: 3})
	if err != nil {
		t.Fatalf("ensure third: %v", err)
	}
	if !created {
		t.Fatal("expected third ensure to create after completion")
	}
	if third.ID == first.ID {
		t.Fatalf("third job reused completed job %s", first.ID)
	}
}

func TestStoreEnsureActiveJobForPayloadCreatesPendingWhileRunning(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	payload := testPayload{ResourceID: "s1"}

	first, created, err := store.EnsureActiveJobForPayload(ctx, payload, jobqueue.QueueConfig{})
	if err != nil {
		t.Fatalf("ensure first: %v", err)
	}
	if !created {
		t.Fatal("expected first ensure to create job")
	}

	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if claimed == nil || claimed.ID != first.ID {
		t.Fatalf("claimed = %#v, want first job %s", claimed, first.ID)
	}

	second, created, err := store.EnsureActiveJobForPayload(ctx, payload, jobqueue.QueueConfig{})
	if err != nil {
		t.Fatalf("ensure while running: %v", err)
	}
	if !created {
		t.Fatal("expected ensure while running to create pending job")
	}
	if second.ID == first.ID {
		t.Fatalf("second job reused running job %s", first.ID)
	}
}

func TestStoreEnsureActiveJobForPayloadUpdatesPendingPayload(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	first, created, err := store.EnsureActiveJobForPayload(ctx, testPayload{ResourceID: "s1", Value: "old"}, jobqueue.QueueConfig{})
	if err != nil {
		t.Fatalf("ensure first: %v", err)
	}
	if !created {
		t.Fatal("expected first ensure to create job")
	}

	second, created, err := store.EnsureActiveJobForPayload(ctx, testPayload{ResourceID: "s1", Value: "new"}, jobqueue.QueueConfig{})
	if err != nil {
		t.Fatalf("ensure second: %v", err)
	}
	if created {
		t.Fatal("expected second ensure to update pending job")
	}
	if second.ID != first.ID {
		t.Fatalf("second id = %s, want %s", second.ID, first.ID)
	}

	var payload testPayload
	if err := json.Unmarshal(second.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Value != "new" {
		t.Fatalf("payload value = %q, want new", payload.Value)
	}
}

func TestStoreEnsureActiveJobForPayloadHonorsDuplicateAllower(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	first, created, err := store.EnsureActiveJobForPayload(ctx, testPayload{ResourceID: "s1"}, jobqueue.QueueConfig{})
	if err != nil {
		t.Fatalf("ensure first: %v", err)
	}
	if !created {
		t.Fatal("expected first ensure to create job")
	}

	second, created, err := store.EnsureActiveJobForPayload(ctx, testPayload{ResourceID: "s1", Dupe: true}, jobqueue.QueueConfig{})
	if err != nil {
		t.Fatalf("ensure duplicate-allowed: %v", err)
	}
	if !created {
		t.Fatal("expected duplicate-allowed ensure to create job")
	}
	if second.ID == first.ID {
		t.Fatalf("duplicate-allowed job reused %s", first.ID)
	}
}

func TestStoreFailedTerminalJobReleasesUniqueResource(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	first := &jobqueue.Job{
		Type:        testType,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, first, jobqueue.WithUniqueResource()); err != nil {
		t.Fatalf("create first: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected claimed job")
	}
	if err := store.FailJob(ctx, claimed.ID, "failed", time.Millisecond); err != nil {
		t.Fatalf("fail first: %v", err)
	}
	second := &jobqueue.Job{
		Type:        testType,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, second, jobqueue.WithUniqueResource()); err != nil {
		t.Fatalf("create after terminal failure: %v", err)
	}
}

func TestStoreCancelJobIsTerminal(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	job := &jobqueue.Job{
		Type:        testType,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, job, jobqueue.WithUniqueResource()); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.CancelJob(ctx, job.ID, "superseded"); err != nil {
		t.Fatalf("cancel job: %v", err)
	}
	canceled, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get canceled: %v", err)
	}
	if canceled.Status != jobqueue.StatusCanceled {
		t.Fatalf("status = %s, want canceled", canceled.Status)
	}
	if canceled.Error == nil || *canceled.Error != "superseded" {
		t.Fatalf("error = %v, want superseded", canceled.Error)
	}
	if canceled.CompletedAt == nil {
		t.Fatal("expected completedAt")
	}

	next := &jobqueue.Job{
		Type:        testType,
		Payload:     []byte(`{}`),
		Status:      jobqueue.StatusPending,
		MaxAttempts: 1,
		Resource:    jobqueue.Resource{Type: "sandbox", ID: "s1"},
	}
	if err := store.CreateJob(ctx, next, jobqueue.WithUniqueResource()); err != nil {
		t.Fatalf("create after cancel: %v", err)
	}
}

func TestStoreClaimSerializesRunningResources(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	resource := jobqueue.Resource{Type: "sandbox", ID: "s1"}

	first := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 1, Resource: resource}
	second := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 1, Resource: resource}
	if err := store.CreateJob(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	if err := store.CreateJob(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected first claim")
	}
	blocked, err := store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker")
	if err != nil {
		t.Fatalf("claim second while resource running: %v", err)
	}
	if blocked != nil {
		t.Fatalf("expected second claim to be blocked, got %s", blocked.ID)
	}
	if err := store.CompleteJob(ctx, claimed.ID); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	claimed, err = store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker")
	if err != nil {
		t.Fatalf("claim second after complete: %v", err)
	}
	if claimed == nil || claimed.ID != second.ID {
		t.Fatalf("claimed = %#v, want second job %s", claimed, second.ID)
	}
}

func TestStoreReadAPIs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	resource := jobqueue.Resource{Type: "sandbox", ID: "s1"}

	exists, err := store.HasActiveJobForResource(ctx, resource)
	if err != nil {
		t.Fatalf("has active before create: %v", err)
	}
	if exists {
		t.Fatal("expected no active job before create")
	}
	if _, err := store.GetJob(ctx, "missing"); !errors.Is(err, jobqueue.ErrJobNotFound) {
		t.Fatalf("missing get error = %v, want ErrJobNotFound", err)
	}
	if _, err := store.GetLatestJobForResource(ctx, resource); !errors.Is(err, jobqueue.ErrJobNotFound) {
		t.Fatalf("missing latest error = %v, want ErrJobNotFound", err)
	}

	first := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 1, Resource: resource}
	if err := store.CreateJob(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	time.Sleep(time.Millisecond)
	second := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 1, Resource: resource}
	if err := store.CreateJob(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	exists, err = store.HasActiveJobForResource(ctx, resource)
	if err != nil {
		t.Fatalf("has active after create: %v", err)
	}
	if !exists {
		t.Fatal("expected active job after create")
	}
	loaded, err := store.GetJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if loaded.ID != first.ID {
		t.Fatalf("loaded id = %s, want %s", loaded.ID, first.ID)
	}
	latest, err := store.GetLatestJobForResource(ctx, resource)
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.ID != second.ID {
		t.Fatalf("latest id = %s, want %s", latest.ID, second.ID)
	}

	if err := store.CompleteJob(ctx, first.ID); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	if err := store.CompleteJob(ctx, second.ID); err != nil {
		t.Fatalf("complete second: %v", err)
	}
	exists, err = store.HasActiveJobForResource(ctx, resource)
	if err != nil {
		t.Fatalf("has active after complete: %v", err)
	}
	if exists {
		t.Fatal("expected no active job after complete")
	}
}

func TestStoreFailJobRetryAndNotFound(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.FailJob(ctx, "missing", "nope", time.Millisecond); !errors.Is(err, jobqueue.ErrJobNotFound) {
		t.Fatalf("missing fail error = %v, want ErrJobNotFound", err)
	}

	resource := jobqueue.Resource{Type: "sandbox", ID: "retry"}
	job := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 2, Resource: resource}
	if err := store.CreateJob(ctx, job, jobqueue.WithUniqueResource()); err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker")
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected claimed job")
	}

	beforeFail := time.Now()
	if err := store.FailJob(ctx, claimed.ID, "retry", 50*time.Millisecond); err != nil {
		t.Fatalf("fail for retry: %v", err)
	}
	retry, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get retry: %v", err)
	}
	if retry.Status != jobqueue.StatusPending {
		t.Fatalf("retry status = %s, want pending", retry.Status)
	}
	if retry.WorkerID != nil || retry.StartedAt != nil {
		t.Fatalf("expected worker and start cleared, got worker=%v started=%v", retry.WorkerID, retry.StartedAt)
	}
	if retry.Error == nil || *retry.Error != "retry" {
		t.Fatalf("retry error = %v, want retry", retry.Error)
	}
	if !retry.ScheduledAt.After(beforeFail) {
		t.Fatalf("retry scheduled_at = %s, want after %s", retry.ScheduledAt, beforeFail)
	}

	duplicate := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 1, Resource: resource}
	if err := store.CreateJob(ctx, duplicate, jobqueue.WithUniqueResource()); !errors.Is(err, jobqueue.ErrJobAlreadyExists) {
		t.Fatalf("create duplicate while retry pending error = %v, want ErrJobAlreadyExists", err)
	}
}

func TestStoreCleanupStaleJobs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	job := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 1}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker")
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected claimed job")
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
	if recovered.Status != jobqueue.StatusPending || recovered.WorkerID != nil || recovered.StartedAt != nil {
		t.Fatalf("recovered = status %s worker %v started %v", recovered.Status, recovered.WorkerID, recovered.StartedAt)
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

func TestStoreOpenAccessorsAndClose(t *testing.T) {
	store, err := gormstore.Open(gormstore.Config{
		DSN:    filepath.Join(t.TempDir(), "jobs.db"),
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if store.WriteDB() == nil {
		t.Fatal("expected write db")
	}
	if store.ReadDB() == nil {
		t.Fatal("expected read db")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func TestStoreOpenReturnsError(t *testing.T) {
	if _, err := gormstore.Open(gormstore.Config{DSN: "file:/definitely/missing/jobs.db?mode=ro"}); err == nil {
		t.Fatal("expected open error")
	}
}

func TestStoreCloseHandlesSharedPool(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	store := gormstore.New(db)
	if err := store.Close(); err != nil {
		t.Fatalf("close shared pool: %v", err)
	}
}

func TestStoreMethodsReturnDatabaseErrorsAfterClose(t *testing.T) {
	ctx := context.Background()
	store, err := gormstore.Open(gormstore.Config{
		DSN:    filepath.Join(t.TempDir(), "jobs.db"),
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	job := &jobqueue.Job{Type: testType, Payload: []byte(`{}`), Status: jobqueue.StatusPending, MaxAttempts: 1}
	if err := store.CreateJob(ctx, job); err == nil {
		t.Fatal("expected create error after close")
	}
	if _, err := store.GetJob(ctx, "missing"); err == nil || errors.Is(err, jobqueue.ErrJobNotFound) {
		t.Fatalf("get after close error = %v, want database error", err)
	}
	if _, err := store.GetLatestJobForResource(ctx, jobqueue.Resource{Type: "sandbox", ID: "s1"}); err == nil || errors.Is(err, jobqueue.ErrJobNotFound) {
		t.Fatalf("latest after close error = %v, want database error", err)
	}
	if _, err := store.HasActiveJobForResource(ctx, jobqueue.Resource{Type: "sandbox", ID: "s1"}); err == nil {
		t.Fatal("expected has-active error after close")
	}
	if _, err := store.ClaimJob(ctx, []jobqueue.Type{testType}, "worker"); err == nil {
		t.Fatal("expected claim error after close")
	}
	if err := store.FailJob(ctx, "missing", "failed", time.Millisecond); err == nil || errors.Is(err, jobqueue.ErrJobNotFound) {
		t.Fatalf("fail after close error = %v, want database error", err)
	}
	if _, err := store.CleanupStaleJobs(ctx, 0); err == nil {
		t.Fatal("expected cleanup error after close")
	}
	if _, err := store.TryAcquireLeadership(ctx, "worker", time.Minute); err == nil {
		t.Fatal("expected leadership error after close")
	}
}

func newTestStore(t *testing.T) *gormstore.Store {
	t.Helper()
	store, err := gormstore.Open(gormstore.Config{
		DSN:    filepath.Join(t.TempDir(), "jobs.db"),
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	return store
}
