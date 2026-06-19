package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/obot-platform/discobox/orchestration"
)

type memoryStore struct {
	mu sync.Mutex

	jobs      map[string]*orchestration.Job
	sandboxes map[string]*Sandbox
	active    map[string]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		jobs:      map[string]*orchestration.Job{},
		sandboxes: map[string]*Sandbox{},
		active:    map[string]string{},
	}
}

func (s *memoryStore) Transaction(ctx context.Context, fn func(context.Context, *memoryStore) error) error {
	return fn(ctx, s)
}

func (s *memoryStore) CreateSandbox(_ context.Context, sandbox *Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sandboxes[sandbox.ID]; ok {
		return fmt.Errorf("sandbox %s already exists", sandbox.ID)
	}
	s.sandboxes[sandbox.ID] = cloneSandbox(sandbox)
	return nil
}

func (s *memoryStore) GetSandbox(_ context.Context, sandboxID string) (*Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sandbox := s.sandboxes[sandboxID]
	if sandbox == nil {
		return nil, fmt.Errorf("sandbox %s not found", sandboxID)
	}
	return cloneSandbox(sandbox), nil
}

func (s *memoryStore) UpdateSandbox(_ context.Context, sandbox *Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sandboxes[sandbox.ID]; !ok {
		return fmt.Errorf("sandbox %s not found", sandbox.ID)
	}
	s.sandboxes[sandbox.ID] = cloneSandbox(sandbox)
	return nil
}

func (s *memoryStore) CreateJob(_ context.Context, job *orchestration.Job, options ...orchestration.CreateJobOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()

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

	opts := orchestration.ResolveCreateJobOptions(options...)
	if opts.UniqueResource && job.Resource.Type != "" && job.Resource.ID != "" {
		key := memoryResourceKey(job.Resource)
		if _, ok := s.active[key]; ok {
			return orchestration.ErrJobAlreadyExists
		}
		s.active[key] = job.ID
	}

	s.jobs[job.ID] = cloneJob(job)
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
	if job := s.latestForResource(resource); job != nil {
		return job, nil
	}
	return nil, orchestration.ErrJobNotFound
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
	return s.hasActiveJobForResourceLocked(resource), nil
}

func (s *memoryStore) ClaimJob(_ context.Context, types []orchestration.Type, workerID string) (*orchestration.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
		if s.hasRunningJobForResourceLocked(job.Resource) {
			continue
		}
		if best == nil || job.Priority > best.Priority || (job.Priority == best.Priority && job.CreatedAt.Before(best.CreatedAt)) {
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

func (s *memoryStore) CompleteJob(_ context.Context, id string) error {
	return s.finish(id, orchestration.StatusCompleted, "")
}

func (s *memoryStore) CancelJob(_ context.Context, id string, message string) error {
	return s.finish(id, orchestration.StatusCanceled, message)
}

func (s *memoryStore) FailJob(_ context.Context, id string, message string, retryBackoff time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	if job == nil {
		return orchestration.ErrJobNotFound
	}

	now := time.Now()
	job.Error = &message
	if job.Attempts < job.MaxAttempts {
		job.Status = orchestration.StatusPending
		job.WorkerID = nil
		job.StartedAt = nil
		job.ScheduledAt = now.Add(retryBackoff)
	} else {
		job.Status = orchestration.StatusFailed
		job.CompletedAt = &now
	}
	job.UpdatedAt = now
	return nil
}

func claimableStatus(status orchestration.Status) bool {
	return status == orchestration.StatusPending || status == orchestration.StatusBackoff
}

func (s *memoryStore) CleanupStaleJobs(_ context.Context, staleAfter time.Duration) (int64, error) {
	return 0, nil
}

func (s *memoryStore) TryAcquireLeadership(_ context.Context, workerID string, timeout time.Duration) (bool, error) {
	return true, nil
}

func (s *memoryStore) ReleaseLeadership(_ context.Context, workerID string) error {
	return nil
}

func (s *memoryStore) finish(id string, status orchestration.Status, message string) error {
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
	job.CompletedAt = &now
	job.UpdatedAt = now
	delete(s.active, memoryResourceKey(job.Resource))
	return nil
}

func (s *memoryStore) latestForResource(resource orchestration.Resource) *orchestration.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *orchestration.Job
	for _, job := range s.jobs {
		if job.Resource == resource && (latest == nil || job.CreatedAt.After(latest.CreatedAt)) {
			latest = job
		}
	}
	return cloneJob(latest)
}

func (s *memoryStore) hasActiveJobForResourceLocked(resource orchestration.Resource) bool {
	for _, job := range s.jobs {
		if job.Resource == resource && (job.Status == orchestration.StatusPending || job.Status == orchestration.StatusBackoff || job.Status == orchestration.StatusRunning) {
			return true
		}
	}
	return false
}

func (s *memoryStore) hasRunningJobForResourceLocked(resource orchestration.Resource) bool {
	for _, job := range s.jobs {
		if job.Resource == resource && job.Status == orchestration.StatusRunning {
			return true
		}
	}
	return false
}

func memoryResourceKey(resource orchestration.Resource) string {
	return resource.Type + "\x00" + resource.ID
}

func cloneJob(job *orchestration.Job) *orchestration.Job {
	if job == nil {
		return nil
	}
	cp := *job
	if job.Payload != nil {
		cp.Payload = append([]byte(nil), job.Payload...)
	}
	return &cp
}

func cloneSandbox(sandbox *Sandbox) *Sandbox {
	if sandbox == nil {
		return nil
	}
	cp := *sandbox
	if sandbox.LastJobID != nil {
		jobID := *sandbox.LastJobID
		cp.LastJobID = &jobID
	}
	return &cp
}
