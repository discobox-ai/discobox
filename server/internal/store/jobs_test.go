package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
)

func TestListJobsForProjectScopesSandboxAndWorkerJobs(t *testing.T) {
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
		{ID: "job-sandbox-2", Type: "sandbox.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "sandbox", ID: "sandbox-2"}},
		{ID: "job-worker-2", Type: "worker.reconcile", Payload: json.RawMessage(`{}`), Resource: orchestration.Resource{Type: "worker", ID: "worker-2"}},
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
	for _, id := range []string{"job-sandbox-1", "job-worker-1"} {
		if !got[id] {
			t.Fatalf("missing project job %s in %#v", id, got)
		}
	}
	for _, id := range []string{"job-sandbox-2", "job-worker-2"} {
		if got[id] {
			t.Fatalf("unexpected other project job %s in %#v", id, got)
		}
	}
}
