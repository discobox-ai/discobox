package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/internal/filelock"
)

// singletonLockName is the lock file inside the data directory. The data
// directory, not the listen endpoint, is the singleton boundary: the database
// is the resource two servers actually corrupt each other over. Endpoint-scoped
// locking would let a second server started with a different
// DISCOBOX_SERVER_LISTEN run against the same database, which is precisely the
// case that produced dueling pool reconcilers.
const singletonLockName = "server.lock"

// singletonWaitInterval is how long to wait between attempts to take the lock
// from an incumbent server.
const singletonWaitInterval = time.Second

// acquireSingleton blocks until this process is the only server running against
// dataDir, and returns a release function.
//
// A unix socket bind cannot report EADDRINUSE — endpoint.Listen unlinks the
// path first — so binding the endpoint proves nothing about who else is
// running. Before the listen set defaulted to a unix socket, a duplicate server
// was stopped by the kernel refusing the second TCP bind (see the reclaim loop
// in listenWithReclaim). This restores that guarantee at the resource that
// matters, independent of transport.
//
// This never gives up. An incumbent is asked to shut down on every pass and the
// wait is logged each time, so a server that will not leave is visible in the
// log instead of being silently displaced — the failure this replaces was two
// servers running with nothing reporting it.
func acquireSingleton(ctx context.Context, dataDir string, endpoints []string) (func(), error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory is required to lock the server singleton")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, singletonLockName)
	for attempt := 0; ; attempt++ {
		lock, err := filelock.TryAcquire(path)
		if err == nil {
			return func() { _ = lock.Release() }, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, err
		}
		// The incumbent is reachable only by endpoint, so a server holding the
		// same data directory on a different endpoint cannot be asked to leave
		// — it is only waited out. Re-request on every pass: the first request
		// races a server that has not finished starting.
		shutdownExistingLocalServer(ctx, endpoints)
		log.Printf("waiting for %s to release %s before starting", describeSingletonHolder(path), path)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(singletonWaitInterval):
		}
	}
}

// describeSingletonHolder names the process holding the lock for a log message.
func describeSingletonHolder(path string) string {
	pid, ok := filelock.HolderPID(path)
	if !ok {
		return "another discobox-server"
	}
	return fmt.Sprintf("another discobox-server (pid %d)", pid)
}
