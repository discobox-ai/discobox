package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/pkg/endpoint"
	"github.com/discobox-ai/discobox/pkg/health"
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
// During a known shutdown handoff this waits for provider cleanup. Otherwise
// discovery is bounded; the lock remains the sole authority to proceed.
// The wait is logged each pass; no timeout displaces an incumbent.
func acquireSingleton(ctx context.Context, dataDir string, endpoints []string) (func(), error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory is required to lock the server singleton")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, singletonLockName)
	started := time.Now()
	handoff := false
	for _, raw := range endpoints {
		if raw != "" && os.Getenv("DISCOBOX_SERVER_HANDOFF") == raw {
			handoff = true
		}
	}
	var starting health.Status
	for {
		lock, err := filelock.TryAcquire(path)
		if err == nil {
			record := filepath.Join(dataDir, "server-holder.json")
			if err := publishSingleton(record, endpoints); err != nil {
				_ = lock.Release()
				return nil, err
			}
			return func() { _ = os.Remove(record); _ = lock.Release() }, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, err
		}
		// The incumbent is reachable only by endpoint, so a server holding the
		// same data directory on a different endpoint cannot be asked to leave
		// — it is only waited out. Re-request on every pass: the first request
		// races a server that has not finished starting.
		probeDeadline := time.Now().Add(2 * time.Second)
		if !handoff && !starting.Starting() && started.Add(3*time.Second).Before(probeDeadline) {
			probeDeadline = started.Add(3 * time.Second)
		}
		probeCtx, cancel := context.WithDeadline(ctx, probeDeadline)
		type observation struct {
			accepted bool
			status   health.Status
		}
		observations := make(chan observation, len(endpoints))
		for _, raw := range endpoints {
			go func() {
				// Observe health first, independently for every selected endpoint. A
				// stalled shutdown must not hide a valid starting/503 response.
				status, ok := singletonStarting(probeCtx, raw)
				if !ok {
					status = health.Status{}
				}
				observations <- observation{accepted: requestEndpointShutdown(probeCtx, raw), status: status}
			}()
		}
		for range endpoints {
			observed := <-observations
			handoff = handoff || observed.accepted
			if observed.status.Starting() {
				starting = observed.status
			}
		}
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !handoff && !starting.Starting() && time.Since(started) >= 3*time.Second {
			return nil, singletonUnreachable(dataDir, path, endpoints)
		}
		if time.Since(started) >= 5*time.Minute {
			if handoff {
				return nil, fmt.Errorf("shutdown handoff waiting for %s to release %q exceeded its allowance", describeSingletonHolder(path), path)
			}
			return nil, fmt.Errorf("server holding %q did not finish starting (last step: %q)", path, starting.Phase)
		}
		log.Printf("waiting for %s to release %q before starting (last startup step: %q)", describeSingletonHolder(path), path, starting.Phase)
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
		if record, err := readSingletonRecord(filepath.Join(filepath.Dir(path), "server-holder.json")); err == nil {
			pid, ok = record.PID, record.PID > 0
		}
		if !ok {
			return "another discobox-server"
		}
	}
	return fmt.Sprintf("another discobox-server (pid %d)", pid)
}

// singletonRecord is advisory and never establishes permission to take the lock
// or shut down an endpoint the caller did not select.
type singletonRecord struct {
	PID       int      `json:"pid"`
	Endpoints []string `json:"endpoints"`
}

func publishSingleton(path string, endpoints []string) error {
	data, err := json.Marshal(singletonRecord{PID: os.Getpid(), Endpoints: endpoints})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".server-holder-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func singletonUnreachable(dataDir, path string, endpoints []string) error {
	holder := describeSingletonHolder(path)
	address := "address unavailable; consult the local server log"
	record, err := readSingletonRecord(filepath.Join(dataDir, "server-holder.json"))
	if err == nil && record.PID > 0 {
		pid, known := filelock.HolderPID(path)
		if !known || pid == record.PID {
			holder = fmt.Sprintf("another discobox-server (pid %d)", record.PID)
			if len(record.Endpoints) > 0 {
				address = fmt.Sprintf("addresses %q; use --server %q to contact the holder", record.Endpoints, record.Endpoints[0])
			}
		}
	}

	return fmt.Errorf("unreachable %s holds %q: %s; attempted %q; stop the holder before starting another server", holder, path, address, endpoints)
}

func singletonStarting(ctx context.Context, raw string) (health.Status, bool) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	parsed, err := endpoint.Parse(raw)
	if err != nil {
		return health.Status{}, false
	}
	base, client, err := endpoint.HTTPClient(parsed, nil)
	if err != nil {
		return health.Status{}, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+health.Path, nil)
	if err != nil {
		return health.Status{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return health.Status{}, false
	}
	defer resp.Body.Close()
	var status health.Status
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&status)
	return status, err == nil && (resp.StatusCode == 503 || resp.StatusCode == 200) && status.Starting()
}

func readSingletonRecord(path string) (singletonRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return singletonRecord{}, err
	}
	defer f.Close()
	var record singletonRecord
	err = json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&record)
	return record, err
}
