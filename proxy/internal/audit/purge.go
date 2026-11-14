package audit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PurgeResult reports what a purge of one client's audit removed.
type PurgeResult struct {
	HTTPRows  int64 `json:"httpRows"`
	SOCKSRows int64 `json:"socksRows"`
	DNSRows   int64 `json:"dnsRows"`
	Files     int64 `json:"files"`
	Bytes     int64 `json:"bytes"`
}

// purgeRequest is a purge on its way through the recorder's writer, which runs
// it after every event queued before it, so no row of the client's written
// before the purge was asked for is left behind it.
type purgeRequest struct {
	ctx      context.Context
	clientID string
	done     chan purgeOutcome
}

type purgeOutcome struct {
	result PurgeResult
	err    error
}

// PurgeClient removes everything the audit holds of one client: its HTTP,
// SOCKS and DNS rows, and its body and stream spools (ADR 26-11-14-896 §5).
// Closing a spool does not end its exchange, whose row is written when the
// exchange ends, so the writer also drops the client's rows that arrive after
// the purge, for purgedClientMemory (dropPurged).
// It is what a purge of a sandbox takes from the pool proxy; the age sweep
// and the spool budget remain what reclaims everything else.
//
// It runs on the recorder's one writer, as every row write does, because the
// writer is what keeps the database and the spools' running total consistent:
// a second writer would race both. A spool of the client's still open is
// closed first, so nothing writes into a file after it is gone. Rows go before
// files, the order the age sweep keeps, so nothing is left naming a file that
// is not there. A client with nothing recorded purges to an empty result, so a
// repeated purge succeeds.
//
// The response cache is not touched: its entries are content-addressed and
// shared by the pool, and the rows that referred to one are deleted with the
// rest.
func (r *Recorder) PurgeClient(ctx context.Context, clientID string) (PurgeResult, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || clientID == "." || clientID == ".." || strings.ContainsAny(clientID, `/\`) {
		return PurgeResult{}, fmt.Errorf("invalid audit client %q", clientID)
	}
	if r == nil || !r.enabled {
		return PurgeResult{}, nil
	}
	r.closeClientSpools(clientID)

	request := purgeRequest{ctx: ctx, clientID: clientID, done: make(chan purgeOutcome, 1)}
	r.enqueueMu.RLock()
	if r.closed.Load() {
		r.enqueueMu.RUnlock()
		return PurgeResult{}, errors.New("audit recorder closed")
	}
	// Never dropped as an event is when the queue is full: a purge that did
	// not happen must say so, and waiting for room is what the caller asked.
	select {
	case r.ch <- request:
		r.enqueueMu.RUnlock()
	case <-ctx.Done():
		r.enqueueMu.RUnlock()
		return PurgeResult{}, ctx.Err()
	}
	select {
	case outcome := <-request.done:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return PurgeResult{}, ctx.Err()
	}
}

// closeClientSpools closes every spool of the client's still being written: an
// upgraded stream the proxy has not yet seen end. Each closer is taken out
// from under the lock before it runs, since closing releases the spool's hold,
// which takes the lock itself.
func (r *Recorder) closeClientSpools(clientID string) {
	prefixes := []string{
		spoolKey(filepath.Join("bodies", clientID)) + string(filepath.Separator),
		spoolKey(filepath.Join("streams", clientID)) + string(filepath.Separator),
	}
	var closers []io.Closer
	r.spoolMu.Lock()
	for key, closer := range r.openSpool {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) && closer != nil {
				closers = append(closers, closer)
				break
			}
		}
	}
	r.spoolMu.Unlock()
	for _, closer := range closers {
		_ = closer.Close()
	}
}

// purgedClientMemory is how long the writer drops a purged client's rows.
// What it catches is an exchange already under way when its client was purged,
// whose row is written when it ends: after a purge its sandbox's network is
// gone, so an upgraded stream ends only when its upstream closes or a write to
// it times out. A day is longer than any of those, and a row that outlived it
// would still go with the age sweep.
const purgedClientMemory = 24 * time.Hour

// dropPurged reports whether event is a row of a purged client's, which the
// writer then drops instead of writing, removing whatever spool it names: the
// purge closed its spool and deleted its file, and nothing else would. Purges
// older than purgedClientMemory are forgotten as it goes.
func (r *Recorder) dropPurged(event any) bool {
	if len(r.purged) == 0 {
		return false
	}
	var client string
	var files []string
	switch e := event.(type) {
	case HTTPEvent:
		client, files = e.ClientID, []string{e.RequestBodyFile, e.ResponseBodyFile, e.StreamFile}
	case SOCKSEvent:
		client = e.ClientID
	case DNSEvent:
		client = e.ClientID
	default:
		return false
	}
	purgedAt, ok := r.purged[client]
	if !ok {
		return false
	}
	if time.Since(purgedAt) > purgedClientMemory {
		delete(r.purged, client)
		return false
	}
	for i, file := range files {
		dir := r.bodyDir
		if i == 2 {
			dir = r.streamDir
		}
		removeSpool(dir, file)
	}
	return true
}

// removeSpool removes one spool file a dropped row named, through an os.Root
// as every removal in the spool trees is.
func removeSpool(dir, relativePath string) {
	if dir == "" || relativePath == "" {
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	_ = root.Remove(filepath.FromSlash(relativePath))
}

// purge is the writer's half of PurgeClient. The DNS rows still queued are
// written first: the writer takes DNS only when nothing else waits, so one of
// the client's could otherwise land after its purge.
func (r *Recorder) purge(request purgeRequest) {
	for pending := len(r.dnsCh); pending > 0; pending-- {
		select {
		case event, ok := <-r.dnsCh:
			if !ok {
				pending = 0
				continue
			}
			r.write(event)
		default:
			pending = 0
		}
	}
	result, err := r.purgeClient(request.ctx, request.clientID)
	if err == nil {
		// Remembered only once it has succeeded: a failed purge is retried,
		// and the rows it left must still be there to delete.
		if r.purged == nil {
			r.purged = map[string]time.Time{}
		}
		for client, purgedAt := range r.purged {
			if time.Since(purgedAt) > purgedClientMemory {
				delete(r.purged, client)
			}
		}
		r.purged[request.clientID] = time.Now()
	}
	request.done <- purgeOutcome{result: result, err: err}
}

func (r *Recorder) purgeClient(ctx context.Context, clientID string) (PurgeResult, error) {
	var result PurgeResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	db := r.db.WithContext(ctx)
	res := db.Where("client_id = ?", clientID).Delete(&HTTPExchange{})
	if res.Error != nil {
		return result, fmt.Errorf("delete http audit rows of %s: %w", clientID, res.Error)
	}
	result.HTTPRows = res.RowsAffected
	res = db.Where("client_id = ?", clientID).Delete(&SOCKSConnect{})
	if res.Error != nil {
		return result, fmt.Errorf("delete socks audit rows of %s: %w", clientID, res.Error)
	}
	result.SOCKSRows = res.RowsAffected
	res = db.Where("client_id = ?", clientID).Delete(&DNSQuery{})
	if res.Error != nil {
		return result, fmt.Errorf("delete dns audit rows of %s: %w", clientID, res.Error)
	}
	result.DNSRows = res.RowsAffected

	var errs []error
	for _, tree := range []struct{ dir, kind string }{{r.bodyDir, "bodies"}, {r.streamDir, "streams"}} {
		files, bytes, err := removeClientSpools(tree.dir, filepath.Join(tree.kind, clientID))
		result.Files += files
		result.Bytes += bytes
		if err != nil {
			errs = append(errs, err)
		}
	}
	// The running total is an estimate the next budget pass corrects; taking
	// the purged bytes off it now keeps that pass from waking for bytes that
	// are gone.
	if result.Bytes > 0 {
		if total := r.spoolBytes.Add(-result.Bytes); total < 0 {
			r.spoolBytes.Store(0)
		}
	}
	return result, errors.Join(errs...)
}

// removeClientSpools removes one client's directory of a spool tree, counting
// what it held. It goes through an os.Root, as the age sweep does, so a
// symlink planted in the tree cannot make it delete outside it. A client that
// never spooled here has no directory, which is nothing to remove.
func removeClientSpools(dir, clientDir string) (int64, int64, error) {
	if dir == "" {
		return 0, 0, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("open spool dir: %w", err)
	}
	defer func() { _ = root.Close() }()

	var files, bytes int64
	walkErr := fs.WalkDir(root.FS(), filepath.ToSlash(clientDir), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // What cannot be read is still removed below.
		}
		if entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			files++
			bytes += info.Size()
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return files, bytes, fmt.Errorf("walk %s spools: %w", clientDir, walkErr)
	}
	if err := root.RemoveAll(clientDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return files, bytes, fmt.Errorf("remove %s spools: %w", clientDir, err)
	}
	return files, bytes, nil
}
