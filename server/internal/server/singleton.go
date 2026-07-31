package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

// errLockBusy reports that another process holds the lock. Platform files
// translate their native "would block" error to this.
var errLockBusy = errors.New("lock held by another process")

// acquireSingleton blocks until this process is the only server running against
// dataDir, and returns a release function.
//
// A unix socket bind cannot report EADDRINUSE — localipc.Listen unlinks the
// path first — so binding the endpoint proves nothing about who else is
// running. Before the listen set defaulted to a unix socket, a duplicate server
// was stopped by the kernel refusing the second TCP bind (see the reclaim loop
// in listenWithReclaim). This restores that guarantee at the resource that
// matters, independent of transport.
//
// The lock is an advisory file lock rather than a PID file because the kernel
// releases it when the holder dies, including on SIGKILL: a crashed server
// cannot leave a lock that blocks every future start.
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
		release, err := tryAcquireSingleton(path)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, errLockBusy) {
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

// tryAcquireSingleton takes the lock without blocking, recording this process's
// PID in the file so a waiting server can name the holder. It returns
// errLockBusy when another process holds it.
func tryAcquireSingleton(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server lock: %w", err)
	}
	if err := lockFileNB(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errLockBusy) {
			return nil, errLockBusy
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Best-effort: the PID only feeds the waiting server's log message, so a
	// failure to record it must not fail an otherwise good lock.
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
		_ = file.Sync()
	}
	return func() {
		_ = unlockFile(file)
		_ = file.Close()
	}, nil
}

// describeSingletonHolder names the process holding the lock for a log message.
// The PID is read without the lock, so it is advisory: it may be stale or
// absent, and the caller must not depend on it.
func describeSingletonHolder(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "another discobox-server"
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return "another discobox-server"
	}
	return fmt.Sprintf("another discobox-server (pid %d)", pid)
}
