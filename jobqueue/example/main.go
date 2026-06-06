package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/obot-platform/disco2/jobqueue"
	"github.com/obot-platform/disco2/jobqueue/gormstore"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir, err := os.MkdirTemp("", "jobqueue-example-*")
	if err != nil {
		log.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	store, err := gormstore.Open(gormstore.Config{DSN: filepath.Join(dir, "example.db")})
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("migrate database: %v", err)
	}

	app, err := NewApp(store, newMemorySandboxService())
	if err != nil {
		log.Fatalf("create app: %v", err)
	}

	if err := app.Dispatcher.Start(ctx); err != nil {
		log.Fatalf("start dispatcher: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := app.Dispatcher.DrainAndStop(stopCtx); err != nil {
			log.Printf("stop dispatcher: %v", err)
		}
	}()

	provisionJob, err := ProvisionSandbox(ctx, app.Queue, "project-1", "sandbox-1")
	if err != nil {
		log.Fatalf("enqueue provision: %v", err)
	}
	fmt.Printf("enqueued %s job %s\n", provisionJob.Type, provisionJob.ID)

	if err := waitForStatus(ctx, store, provisionJob.ID, jobqueue.StatusCompleted); err != nil {
		log.Fatalf("wait provision: %v", err)
	}
	fmt.Printf("completed %s job %s\n", provisionJob.Type, provisionJob.ID)

	deleteJob, err := DeleteSandboxLater(ctx, app.Queue, "project-1", "sandbox-1", time.Now().Add(500*time.Millisecond))
	if err != nil {
		log.Fatalf("enqueue delete: %v", err)
	}
	fmt.Printf("enqueued %s job %s\n", deleteJob.Type, deleteJob.ID)

	if err := waitForStatus(ctx, store, deleteJob.ID, jobqueue.StatusCompleted); err != nil {
		log.Fatalf("wait delete: %v", err)
	}
	fmt.Printf("completed %s job %s\n", deleteJob.Type, deleteJob.ID)
}

type memorySandboxService struct {
	mu        sync.Mutex
	sandboxes map[string]string
}

func newMemorySandboxService() *memorySandboxService {
	return &memorySandboxService{sandboxes: make(map[string]string)}
}

func (s *memorySandboxService) Provision(ctx context.Context, projectID, sandboxID string) error {
	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxes[sandboxID] = projectID
	fmt.Printf("provisioned sandbox %s in project %s\n", sandboxID, projectID)
	return nil
}

func (s *memorySandboxService) Delete(ctx context.Context, projectID, sandboxID string) error {
	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existingProject, ok := s.sandboxes[sandboxID]; !ok || existingProject != projectID {
		return fmt.Errorf("sandbox %s was not provisioned in project %s", sandboxID, projectID)
	}
	delete(s.sandboxes, sandboxID)
	fmt.Printf("deleted sandbox %s in project %s\n", sandboxID, projectID)
	return nil
}

func waitForStatus(ctx context.Context, store jobqueue.Store, jobID string, want jobqueue.Status) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		if job.Status == want {
			return nil
		}
		if job.Status == jobqueue.StatusFailed {
			if job.Error != nil {
				return fmt.Errorf("job failed: %s", *job.Error)
			}
			return fmt.Errorf("job failed")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
