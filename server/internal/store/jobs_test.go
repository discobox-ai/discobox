package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

func TestListJobsForProjectScopesSandboxWorkerAndProviderJobs(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	otherProject := &model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Other", Slug: "other"}
	if err := db.Write.WithContext(ctx).Create(otherProject).Error; err != nil {
		t.Fatalf("create other project: %v", err)
	}
	for _, sandbox := range []model.Sandbox{
		{ID: "sandbox-1", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "one"},
		{ID: "sandbox-2", ProjectID: "project-2", CreatedByUserID: "user-1", Name: "two"},
	} {
		sandbox := sandbox
		if err := s.CreateSandbox(ctx, &sandbox); err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}
	for _, provider := range []model.SandboxProviderInstance{
		{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"},
		{ID: "provider-2", ProjectID: "project-2", Type: "docker", Name: "two"},
	} {
		provider := provider
		if err := s.CreateSandboxProviderInstance(ctx, &provider); err != nil {
			t.Fatalf("create provider %s: %v", provider.ID, err)
		}
	}
	for _, worker := range []model.Worker{
		{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: "provider-1"},
		{ID: "worker-2", ProjectID: "project-2", ProviderInstanceID: "provider-2"},
	} {
		worker := worker
		if err := s.CreateWorker(ctx, &worker); err != nil {
			t.Fatalf("create worker %s: %v", worker.ID, err)
		}
	}
	for _, job := range []orchestration.Job{
		{ID: "job-sandbox-1", Type: "sandbox.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "sandbox", ID: "sandbox-1"}},
		{ID: "job-worker-1", Type: "worker.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "worker", ID: "worker-1"}},
		{ID: "job-provider-1", Type: "workerprovider.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "workerprovider", ID: "provider-1"}},
		{ID: "job-sandbox-2", Type: "sandbox.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "sandbox", ID: "sandbox-2"}},
		{ID: "job-worker-2", Type: "worker.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "worker", ID: "worker-2"}},
		{ID: "job-provider-2", Type: "workerprovider.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "workerprovider", ID: "provider-2"}},
	} {
		job := job
		if err := s.CreateJob(ctx, &job); err != nil {
			t.Fatalf("create job %s: %v", job.ID, err)
		}
	}

	jobs, err := s.ListJobsForProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	got := map[string]bool{}
	for _, job := range jobs {
		got[job.ID] = true
	}
	for _, id := range []string{"job-sandbox-1", "job-worker-1", "job-provider-1"} {
		if !got[id] {
			t.Fatalf("missing project job %s in %#v", id, got)
		}
	}
	for _, id := range []string{"job-sandbox-2", "job-worker-2", "job-provider-2"} {
		if got[id] {
			t.Fatalf("unexpected other project job %s in %#v", id, got)
		}
	}
	for _, id := range []string{"job-sandbox-1", "job-worker-1", "job-provider-1"} {
		var projectID string
		if err := db.Write.WithContext(ctx).Raw("SELECT project_id FROM jobqueue_jobs WHERE id = ?", id).Scan(&projectID).Error; err != nil {
			t.Fatalf("get job project_id for %s: %v", id, err)
		}
		if projectID != "project-1" {
			t.Fatalf("job %s project_id = %q, want project-1", id, projectID)
		}
	}
}

func TestForceJobForProjectMakesBackoffJobRunnable(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: provider.ID}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	job := &orchestration.Job{
		ID:          "job-worker-1",
		Type:        "worker.reconcile",
		Payload:     json.RawMessage(`{}`),
		Status:      orchestration.StatusBackoff,
		Resource:    orchestration.Resource{Type: "worker", ID: worker.ID},
		ScheduledAt: time.Now().Add(time.Hour),
	}
	if err := s.CreateJob(ctx, job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	forced, err := s.ForceJobForProject(ctx, "project-1", job.ID)
	if err != nil {
		t.Fatalf("force job: %v", err)
	}
	if forced.Status != orchestration.StatusPending {
		t.Fatalf("forced status = %s, want pending", forced.Status)
	}
	if time.Until(forced.ScheduledAt) > time.Second {
		t.Fatalf("forced scheduledAt = %s, want runnable now", forced.ScheduledAt)
	}
	if _, err := s.ForceJobForProject(ctx, "project-2", job.ID); !errors.Is(err, orchestration.ErrJobNotFound) {
		t.Fatalf("force other project error = %v, want ErrJobNotFound", err)
	}
}

func TestCreateJobSupersedesQueuedTypeResource(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: provider.ID}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	first := &orchestration.Job{
		ID:       "job-worker-1",
		Type:     "worker.reconcile",
		Payload:  json.RawMessage(`{}`),
		Resource: orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	duplicate := &orchestration.Job{
		ID:       "job-worker-2",
		Type:     "worker.reconcile",
		Payload:  json.RawMessage(`{}`),
		Resource: orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, duplicate); err != nil {
		t.Fatalf("create duplicate: %v", err)
	}
	storedFirst, err := s.GetJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if storedFirst.Status != orchestration.StatusCanceled {
		t.Fatalf("first status = %s, want canceled", storedFirst.Status)
	}
	otherType := &orchestration.Job{
		ID:       "job-worker-provider",
		Type:     "workerprovider.reconcile",
		Payload:  json.RawMessage(`{}`),
		Resource: orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, otherType); err != nil {
		t.Fatalf("create other type: %v", err)
	}
	if _, err := s.ClaimJob(ctx, []orchestration.Type{"worker.reconcile"}, "dispatcher-1"); err != nil {
		t.Fatalf("claim first: %v", err)
	}
	successor := &orchestration.Job{
		ID:       "job-worker-3",
		Type:     "worker.reconcile",
		Payload:  json.RawMessage(`{}`),
		Resource: orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, successor); err != nil {
		t.Fatalf("create successor behind running: %v", err)
	}

	var queued int64
	if err := db.Write.WithContext(ctx).Table("jobqueue_jobs").
		Where("type = ? AND resource_type = ? AND resource_id = ? AND status IN ?", "worker.reconcile", "worker", worker.ID, []orchestration.Status{orchestration.StatusPending, orchestration.StatusBackoff}).
		Count(&queued).Error; err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued worker jobs = %d, want 1", queued)
	}
}

func TestCountRecentJobsForResourceIgnoresCanceledJobs(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStoreWithDB(t, nil)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: provider.ID}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	first := &orchestration.Job{
		ID:       "job-worker-1",
		Type:     "worker.reconcile",
		Payload:  json.RawMessage(`{}`),
		Resource: orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := &orchestration.Job{
		ID:       "job-worker-2",
		Type:     "worker.reconcile",
		Payload:  json.RawMessage(`{}`),
		Resource: orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	count, err := s.CountRecentJobsForResource(ctx, "worker.reconcile", orchestration.Resource{Type: "worker", ID: worker.ID}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("count recent jobs: %v", err)
	}
	if count != 1 {
		t.Fatalf("recent jobs = %d, want 1", count)
	}
}

func TestFailJobDoesNotAssignActiveResourceKeyToNonUniqueRetries(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: provider.ID}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	firstJob := &orchestration.Job{
		ID:          "job-worker-1",
		Type:        "worker.reconcile",
		Payload:     json.RawMessage(`{}`),
		MaxAttempts: 2,
		Resource:    orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, firstJob); err != nil {
		t.Fatalf("create first job: %v", err)
	}

	first, err := s.ClaimJob(ctx, []orchestration.Type{"worker.reconcile"}, "dispatcher-1")
	if err != nil {
		t.Fatalf("claim first: %v", err)
	}
	if first == nil || first.ID != "job-worker-1" {
		t.Fatalf("claimed first job = %#v, want job-worker-1", first)
	}
	secondJob := &orchestration.Job{
		ID:          "job-worker-2",
		Type:        "worker.reconcile",
		Payload:     json.RawMessage(`{}`),
		MaxAttempts: 2,
		Resource:    orchestration.Resource{Type: "worker", ID: worker.ID},
	}
	if err := s.CreateJob(ctx, secondJob); err != nil {
		t.Fatalf("create second job: %v", err)
	}
	if err := s.FailJob(ctx, first.ID, "try again", orchestration.JobResult{}, time.Hour); err != nil {
		t.Fatalf("fail first: %v", err)
	}

	storedFirst, err := s.GetJob(ctx, first.ID)
	if err != nil {
		t.Fatalf("get first: %v", err)
	}
	if storedFirst.Status != orchestration.StatusCanceled {
		t.Fatalf("first status = %s, want canceled", storedFirst.Status)
	}
	second, err := s.ClaimJob(ctx, []orchestration.Type{"worker.reconcile"}, "dispatcher-1")
	if err != nil {
		t.Fatalf("claim second: %v", err)
	}
	if second == nil || second.ID != "job-worker-2" {
		t.Fatalf("claimed second job = %#v, want job-worker-2", second)
	}
	if err := s.FailJob(ctx, second.ID, "try again", orchestration.JobResult{}, time.Hour); err != nil {
		t.Fatalf("fail second: %v", err)
	}

	var activeKeys int64
	if err := db.Write.WithContext(ctx).Table("jobqueue_jobs").
		Where("resource_type = ? AND resource_id = ? AND active_resource_key IS NOT NULL", "worker", worker.ID).
		Count(&activeKeys).Error; err != nil {
		t.Fatalf("count active keys: %v", err)
	}
	if activeKeys != 0 {
		t.Fatalf("non-unique worker retries have %d active resource keys, want 0", activeKeys)
	}
}

func TestFailJobRestoresActiveResourceKeyForUniqueRetry(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	job := &orchestration.Job{
		ID:          "job-provider-1",
		Type:        "workerprovider.reconcile",
		Payload:     json.RawMessage(`{}`),
		MaxAttempts: 2,
		Resource:    orchestration.Resource{Type: "workerprovider", ID: provider.ID},
	}
	if err := s.CreateJob(ctx, job, orchestration.WithUniqueResource()); err != nil {
		t.Fatalf("create job: %v", err)
	}
	claimed, err := s.ClaimJob(ctx, []orchestration.Type{"workerprovider.reconcile"}, "dispatcher-1")
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claimed job = %#v, want %s", claimed, job.ID)
	}
	if err := s.FailJob(ctx, claimed.ID, "try again", orchestration.JobResult{}, time.Hour); err != nil {
		t.Fatalf("fail job: %v", err)
	}

	var activeKeys int64
	if err := db.Write.WithContext(ctx).Table("jobqueue_jobs").
		Where("resource_type = ? AND resource_id = ? AND active_resource_key IS NOT NULL AND unique_resource = ?", "workerprovider", provider.ID, true).
		Count(&activeKeys).Error; err != nil {
		t.Fatalf("count active keys: %v", err)
	}
	if activeKeys != 1 {
		t.Fatalf("unique retry active keys = %d, want 1", activeKeys)
	}
}

func TestBackfillJobProjectIDs(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStoreWithDB(t, nil)

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"}
	if err := s.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	worker := &model.Worker{ID: "worker-1", ProjectID: "project-1", ProviderInstanceID: provider.ID}
	if err := s.CreateWorker(ctx, worker); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if err := db.Write.WithContext(ctx).Exec(`
INSERT INTO jobqueue_jobs (id, type, payload, status, priority, attempts, max_attempts, scheduled_at, resource_type, resource_id, created_at, updated_at)
VALUES (?, ?, ?, ?, 0, 0, 1, CURRENT_TIMESTAMP, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
`, "job-worker-legacy", "worker.reconcile", json.RawMessage(`{}`), orchestration.StatusPending, "worker", worker.ID).Error; err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}

	if err := store.BackfillJobProjectIDs(ctx, db.Write); err != nil {
		t.Fatalf("backfill job project ids: %v", err)
	}
	jobs, err := s.ListJobsForProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, job := range jobs {
		if job.ID == "job-worker-legacy" {
			return
		}
	}
	t.Fatalf("legacy job missing from project jobs: %#v", jobs)
}
