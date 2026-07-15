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
			s.purgeDeletedWorkers(ctx)
			s.purgeSpentWorkerBootstrapTokens(ctx)
		}
	}
}

func (s *ControlPlane) purgeDeletedWorkers(ctx context.Context) {
	cutoff := time.Now().Add(-deletedWorkerPurgeAge)
	n, err := s.store.PurgeDeletedWorkers(ctx, cutoff)
	if err != nil {
		log.Printf("deleted worker cleanup: %v", err)
	} else if n > 0 {
		log.Printf("deleted worker cleanup: purged %d worker(s) deleted before %s", n, cutoff.Format(time.RFC3339))
	}
}

// purgeSpentWorkerBootstrapTokens bounds the bootstrap token table. Tokens are
// single-use and minted per worker-runtime creation; once expired, used, or
// revoked, nothing reads them again, and nothing else deletes them.
func (s *ControlPlane) purgeSpentWorkerBootstrapTokens(ctx context.Context) {
	n, err := s.store.PurgeSpentWorkerBootstrapTokens(ctx, time.Now())
	if err != nil {
		log.Printf("worker bootstrap token cleanup: %v", err)
	} else if n > 0 {
		log.Printf("worker bootstrap token cleanup: purged %d spent token(s)", n)
	}
}
