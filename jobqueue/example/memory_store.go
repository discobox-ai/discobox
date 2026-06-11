package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/obot-platform/disco2/jobqueue"
)

type memoryStore struct {
	mu       sync.Mutex
	jobs     map[string]*jobqueue.Job
	active   map[string]string
	leader   string
	leaderAt time.Time
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]*jobqueue.Job{}, active: map[string]string{}}
}

func (s *memoryStore) EnsureActiveJobForPayload(ctx context.Context, payload jobqueue.Payload, cfg jobqueue.QueueConfig) (*jobqueue.Job, bool, error) {
	resource := payload.Resource()
	if resource.Type != "" && resource.ID != "" {
		if existing := s.latestPendingForResource(resource); existing != nil {
			return existing, false, nil
		}
	}
	job, err := jobqueue.JobFromPayload(payload, cfg)
	if err != nil {
		return nil, false, err
	}
	var opts []jobqueue.CreateJobOption
	if resource.Type != "" && resource.ID != "" {
		opts = append(opts, jobqueue.WithUniqueResource())
	}
	if err := s.CreateJob(ctx, job, opts...); err != nil {
		if errors.Is(err, jobqueue.ErrJobAlreadyExists) && resource.Type != "" && resource.ID != "" {
			return s.latestPendingForResource(resource), false, nil
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
	s.jobs[job.ID] = cloneJob(job)
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
	if job := s.latestForResource(resource); job != nil {
		return job, nil
	}
	return nil, jobqueue.ErrJobNotFound
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
		if best == nil || job.Priority > best.Priority || (job.Priority == best.Priority && job.CreatedAt.Before(best.CreatedAt)) {
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
	} else {
		job.Status = jobqueue.StatusFailed
		job.CompletedAt = &now
	}
	job.UpdatedAt = now
	return nil
}

func (s *memoryStore) CleanupStaleJobs(_ context.Context, staleAfter time.Duration) (int64, error) {
	return 0, nil
}
func (s *memoryStore) TryAcquireLeadership(_ context.Context, workerID string, timeout time.Duration) (bool, error) {
	return true, nil
}
func (s *memoryStore) ReleaseLeadership(_ context.Context, workerID string) error { return nil }

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

func (s *memoryStore) latestForResource(resource jobqueue.Resource) *jobqueue.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *jobqueue.Job
	for _, job := range s.jobs {
		if job.Resource == resource && (latest == nil || job.CreatedAt.After(latest.CreatedAt)) {
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
	return &cp
}
