package workers

import (
	"context"
	"log"
	"time"
)

const (
	deletedWorkerPurgeInterval = time.Hour
	deletedWorkerPurgeAge      = 24 * time.Hour
)

func (s *ControlPlane) StartDeletedWorkerCleanup(ctx context.Context) {
	go s.deletedWorkerCleanupLoop(ctx)
}

func (s *ControlPlane) deletedWorkerCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(deletedWorkerPurgeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-deletedWorkerPurgeAge)
			n, err := s.store.PurgeDeletedWorkers(ctx, cutoff)
			if err != nil {
				log.Printf("deleted worker cleanup: %v", err)
			} else if n > 0 {
				log.Printf("deleted worker cleanup: purged %d worker(s) deleted before %s", n, cutoff.Format(time.RFC3339))
			}
		}
	}
}
