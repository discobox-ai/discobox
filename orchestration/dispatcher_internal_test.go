package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDispatcherExecuteJobWithoutRegisteredExecutorFailsJob(t *testing.T) {
	store := &missingExecutorStore{}
	dispatcher := NewDispatcher(store, DispatcherConfig{SingleNode: true})
	dispatcher.ctx = context.Background()

	dispatcher.executeJob(&Job{ID: "job-1", Type: "missing"})

	if store.failedID != "job-1" {
		t.Fatalf("failed id = %q, want job-1", store.failedID)
	}
	if store.message != ErrExecutorNotRegistered.Error() {
		t.Fatalf("message = %q, want %q", store.message, ErrExecutorNotRegistered.Error())
	}
}

func TestDispatcherNotifyNewJobDisabledAndFull(t *testing.T) {
	disabled := NewDispatcher(&missingExecutorStore{}, DispatcherConfig{ImmediateExecution: false})
	disabled.NotifyNewJob()
	if got := len(disabled.notifyCh); got != 0 {
		t.Fatalf("disabled notify channel length = %d, want 0", got)
	}

	enabled := NewDispatcher(&missingExecutorStore{}, DispatcherConfig{ImmediateExecution: true})
	for i := 0; i < cap(enabled.notifyCh); i++ {
		enabled.NotifyNewJob()
	}
	enabled.NotifyNewJob()
	if got := len(enabled.notifyCh); got != cap(enabled.notifyCh) {
		t.Fatalf("full notify channel length = %d, want %d", got, cap(enabled.notifyCh))
	}
}

func TestDispatcherWorkerIDReturnsConfiguredID(t *testing.T) {
	dispatcher := NewDispatcher(&missingExecutorStore{}, DispatcherConfig{WorkerID: "worker-1"})
	if got := dispatcher.WorkerID(); got != "worker-1" {
		t.Fatalf("worker id = %q, want worker-1", got)
	}
}

func TestDispatcherLeadershipErrorClearsLeader(t *testing.T) {
	store := &missingExecutorStore{leadershipErr: errors.New("leadership failed")}
	dispatcher := NewDispatcher(store, DispatcherConfig{SingleNode: false})
	dispatcher.ctx = context.Background()
	dispatcher.setLeader(true)

	dispatcher.tryAcquireLeadership()

	if dispatcher.IsLeader() {
		t.Fatal("expected leadership error to clear leader state")
	}
}

func TestDispatcherDrainAndStopReturnsTimeout(t *testing.T) {
	dispatcher := NewDispatcher(&missingExecutorStore{}, DispatcherConfig{SingleNode: true})
	dispatcher.jobWg.Add(1)
	defer dispatcher.jobWg.Done()

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := dispatcher.DrainAndStop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v, want deadline exceeded", err)
	}
}

func TestDispatcherDrainAndStopReturnsReleaseLeadershipError(t *testing.T) {
	releaseErr := errors.New("release failed")
	store := &missingExecutorStore{releaseErr: releaseErr}
	dispatcher := NewDispatcher(store, DispatcherConfig{SingleNode: false})
	dispatcher.setLeader(true)

	if err := dispatcher.DrainAndStop(context.Background()); !errors.Is(err, releaseErr) {
		t.Fatalf("drain error = %v, want release error", err)
	}
}

func TestDispatcherEffectiveStaleJobTimeoutFallback(t *testing.T) {
	dispatcher := NewDispatcher(&missingExecutorStore{}, DispatcherConfig{
		JobTimeout:      time.Minute,
		StaleJobTimeout: time.Second,
	})
	want := time.Minute + time.Minute
	if got := dispatcher.effectiveStaleJobTimeout(); got != want {
		t.Fatalf("effective stale timeout = %s, want %s", got, want)
	}
}

type missingExecutorStore struct {
	failedID      string
	message       string
	leadershipErr error
	releaseErr    error
}

func (s *missingExecutorStore) CreateJob(context.Context, *Job, ...CreateJobOption) error {
	return errors.New("not implemented")
}

func (s *missingExecutorStore) GetJob(context.Context, string) (*Job, error) {
	return nil, errors.New("not implemented")
}

func (s *missingExecutorStore) GetLatestJobForResource(context.Context, Resource) (*Job, error) {
	return nil, errors.New("not implemented")
}

func (s *missingExecutorStore) HasActiveJobForResource(context.Context, Resource) (bool, error) {
	return false, errors.New("not implemented")
}

func (s *missingExecutorStore) ClaimJob(context.Context, []Type, string) (*Job, error) {
	return nil, errors.New("not implemented")
}

func (s *missingExecutorStore) CompleteJob(context.Context, string) error {
	return errors.New("not implemented")
}

func (s *missingExecutorStore) CancelJob(context.Context, string, string) error {
	return errors.New("not implemented")
}

func (s *missingExecutorStore) FailJob(_ context.Context, id string, message string, _ time.Duration) error {
	s.failedID = id
	s.message = message
	return nil
}

func (s *missingExecutorStore) CleanupStaleJobs(context.Context, time.Duration) (int64, error) {
	return 0, errors.New("not implemented")
}

func (s *missingExecutorStore) TryAcquireLeadership(context.Context, string, time.Duration) (bool, error) {
	if s.leadershipErr != nil {
		return false, s.leadershipErr
	}
	return false, errors.New("not implemented")
}

func (s *missingExecutorStore) ReleaseLeadership(context.Context, string) error {
	if s.releaseErr != nil {
		return s.releaseErr
	}
	return errors.New("not implemented")
}
