package serverstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// manifestFileName is written into a staged directory, and is what makes that
// directory complete: the assets are moved into place as a set, so a directory
// that has it has every file the manifest named, verified. It is also the
// record of where the contents came from, which is otherwise unanswerable once
// the bytes are on disk.
const manifestFileName = "manifest.json"

// stallTimeout ends a download that has stopped arriving.
//
// The CLI's root context is never canceled and the launch deadline is taken
// after staging runs, so without this a connection that dies mid-body leaves a
// first `discobox run` spinning on one status line for as long as the terminal
// is open. Generous, because a slow link is not a stalled one: what is measured
// is silence, not duration.
//
// A var rather than a const only so a test can shorten it; nothing else
// assigns it.
var stallTimeout = 60 * time.Second

const (
	// progressInterval is how often a download in flight reports. On a ticker
	// rather than per read, so a stalled download keeps saying so and a fast
	// one does not call back once per buffer.
	progressInterval = 200 * time.Millisecond
	// abandonedAge is how old an interrupted staging's leftovers must be
	// before another run sweeps them. Ctrl-C skips the deferred cleanup, and
	// nothing else was looking; the age is what keeps the sweep from deleting
	// a download another process has in flight right now.
	abandonedAge = time.Hour
)

// Progress is one report about a staging in flight, shaped for a status line
// rather than for a progress bar. The caller renders it: this package does not
// know whether it is being watched by a terminal, a launcher window, or
// nothing at all.
type Progress struct {
	// Asset is the file being downloaded.
	Asset string
	// Index is its position in the manifest, from 1, and Assets the number of
	// them. A one-asset manifest is the common case and reads badly as "1 of
	// 1", so a renderer is given the numbers rather than a phrase.
	Index  int
	Assets int
	// Current and Total are bytes of this asset. Total is the size the
	// manifest declares, so it is known from the first report and never grows.
	Current int64
	Total   int64
	// Done marks the closing report, sent once every asset is staged.
	Done bool
}

// Options configures a staging.
type Options struct {
	// Root is the directory holding one subdirectory per staged version.
	Root string
	// Client fetches the assets. Nil is http.DefaultClient.
	Client *http.Client
	// Force restages even when a matching set is already on disk, which is the
	// only thing that re-verifies one. Ordinary use trusts a complete
	// directory rather than hashing a hundred megabytes in front of every
	// command that starts a server.
	Force bool
	// OnProgress, when set, is called while assets are downloaded.
	OnProgress func(Progress)
}

// Staged reports the directory holding m's assets when a complete, matching
// set is already on disk.
//
// Matching, not merely present: a version that was re-cut, or a manifest
// naming different assets under a version already staged, describes different
// contents under the same name and has to be staged again.
func Staged(root string, m Manifest) (string, bool) {
	dir := m.Dir(root)
	data, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		return "", false
	}
	var staged Manifest
	if err := json.Unmarshal(data, &staged); err != nil {
		return "", false
	}
	if !m.sameAssets(staged) {
		return "", false
	}
	for _, asset := range m.Assets {
		if _, err := os.Stat(filepath.Join(dir, asset.Name)); err != nil {
			return "", false
		}
	}
	return dir, true
}

// Stage downloads and verifies m's assets and returns the directory holding
// them.
//
// Everything is written into a temporary sibling directory and moved into
// place as a whole, once every digest matches. So a staged directory is
// complete and verified or it does not exist: an interrupted or corrupted
// download can never be mistaken for a staged version, which is the failure
// that matters, because the file it leaves behind is one that gets executed.
func Stage(ctx context.Context, m Manifest, opts Options) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	if opts.Root == "" {
		return "", errors.New("stage server: no staging directory")
	}
	// Before the staged check, not after: a set left where the alphas put it is
	// already downloaded and verified, and migrating it only once something had
	// decided to fetch it again would be paying for it twice to arrive at the
	// same file.
	migrateLegacyLayout(opts.Root)
	if !opts.Force {
		if dir, ok := Staged(opts.Root, m); ok {
			return dir, nil
		}
	}
	if err := os.MkdirAll(opts.Root, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", opts.Root, err)
	}
	// The destination's own parent, created here rather than at the rename so
	// that one directory owns it and the temporary below is genuinely a
	// sibling of what it becomes.
	dir := m.Dir(opts.Root)
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", parent, err)
	}
	// Both levels. The temporary now lives beside its destination, and the
	// alphas put it directly under the root — so an install that was
	// interrupted before this version has leftovers the new location would
	// never look at. Neither sweep can touch a platform directory: only a name
	// carrying .staging-/.replaced- matches.
	sweepAbandoned(opts.Root)
	sweepAbandoned(parent)
	// A sibling of the destination, so the move into place is a rename within
	// one directory rather than a copy that could half-finish.
	temp, err := os.MkdirTemp(parent, m.Version+".staging-")
	if err != nil {
		return "", fmt.Errorf("stage server: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temp)
		}
	}()

	reporter := newReporter(opts.OnProgress, len(m.Assets))
	for index, asset := range m.Assets {
		if err := download(ctx, opts.Client, asset, filepath.Join(temp, asset.Name), reporter.forAsset(asset.Name, index+1)); err != nil {
			return "", err
		}
	}
	record, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(temp, manifestFileName), append(record, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("stage server: %w", err)
	}

	// The directory itself, so the entries in it are on disk before the rename
	// that publishes them.
	if err := syncDir(temp); err != nil {
		return "", err
	}

	if err := commit(temp, dir); err != nil {
		// A concurrent stage is not a failure: the other one verified the same
		// digests this one did, so whichever set landed is the set that was
		// asked for.
		//
		// Not under Force, which asked for these bytes to be fetched and
		// checked again. Reporting the directory that was already there as
		// freshly staged would answer the one question --force exists to ask
		// with a set nothing re-verified.
		if !opts.Force {
			if staged, ok := Staged(opts.Root, m); ok {
				return staged, nil
			}
		}
		return "", err
	}
	committed = true
	reporter.done()
	return dir, nil
}

// commit moves a verified set into place.
//
// Anything already at the destination is a set that either failed to match what
// was asked for or is being restaged deliberately, so it goes. It is moved
// aside rather than deleted in place: on Windows a running server's file cannot
// be deleted at all, and a rename can still get it out of the way — and if the
// move into place then fails, what was there is put back rather than lost.
//
// That a Windows rename of a directory holding a running executable succeeds is
// the premise here and in migrateLegacyLayout, and it is not something the
// tests establish: the mode bits they provoke the failure with do not exist
// there. If it turns out to be false, both sites need the same fix.
func commit(temp, dir string) error {
	parent := filepath.Dir(dir)
	var aside string
	if _, err := os.Stat(dir); err == nil {
		aside = fmt.Sprintf("%s.replaced-%d", dir, time.Now().UnixNano())
		if err := os.Rename(dir, aside); err != nil {
			return fmt.Errorf("replace %s: %w", dir, err)
		}
	}
	if err := os.Rename(temp, dir); err != nil {
		if aside != "" {
			_ = os.Rename(aside, dir)
		}
		return fmt.Errorf("stage server into %s: %w", dir, err)
	}
	// The rename itself, so a directory that is there after a power loss is
	// one whose contents are there too.
	if err := syncDir(parent); err != nil {
		return err
	}
	if aside != "" {
		_ = os.RemoveAll(aside)
	}
	return nil
}

// syncDir flushes a directory's own entries, which is what makes a rename
// durable rather than merely visible to this boot.
//
// Windows cannot open a directory as a file and has no equivalent call, so
// there it is a no-op rather than an error: NTFS orders metadata for itself,
// and failing a staging over a call the platform does not have would be worse
// than the guarantee is worth.
func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// sweepAbandoned removes what an interrupted staging left behind.
//
// Ctrl-C during a download kills the process outright — the CLI installs no
// signal handler — so the deferred cleanup never runs and a temporary
// directory keeps whatever had arrived, which for a server is most of a
// hundred megabytes. Nothing else was ever going to look for it.
//
// Only leftovers old enough to be nobody's: a staging in flight right now has
// a directory of exactly this shape, and deleting it out from under the
// process that is filling it would turn a slow download into a failed one.
// Best effort throughout — a sweep that cannot run is not a reason to refuse
// to stage.
func sweepAbandoned(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, ".staging-") && !strings.Contains(name, ".replaced-") {
			continue
		}
		path := filepath.Join(dir, name)
		if time.Since(lastTouched(path)) < abandonedAge {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

// lastTouched is the most recent modification anywhere directly inside path,
// or of path itself when it holds nothing.
//
// The directory's own timestamp is not enough. It stops moving as soon as the
// entries exist, and appending to a file does not touch the directory that
// holds it — so a download still arriving an hour later looked exactly as old
// as one abandoned an hour ago, and the sweep would have deleted it out from
// under the process filling it. The file being written is the thing whose
// timestamp keeps moving, so that is what is asked.
func lastTouched(path string) time.Time {
	newest := time.Time{}
	if info, err := os.Stat(path); err == nil {
		newest = info.ModTime()
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return newest
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// migrateLegacyLayout moves a set staged before the layout carried a platform
// into the place this version looks for it.
//
// v0.6.0-alpha.1 and .2 staged into <root>/<version>; a set is now keyed by
// platform too, because two platforms of one version used to evict each other.
// Without this, an alpha user's ~94 MB server is a directory nothing addresses
// or removes again — and the guidelines are explicit that persisted state gets
// an upgrade path rather than being abandoned where it lies.
//
// A legacy directory is one directly under the root that holds a manifest.json;
// a platform directory holds version directories and no manifest of its own,
// which is what tells the two apart. The manifest inside says which platform it
// was for, so the move needs nothing this process has to guess. Best effort
// throughout, and never destructive: a set that cannot be moved is left exactly
// where it is for the next run to try again.
func migrateLegacyLayout(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		legacy := filepath.Join(root, entry.Name())
		record, err := os.ReadFile(filepath.Join(legacy, manifestFileName))
		if err != nil {
			continue
		}
		// A plain decode, not ParseManifest. That is the validator for a
		// manifest about to be staged — it refuses unknown fields and applies
		// rules invented since — and this record was written by an older
		// version, which is the whole reason it is here. v0.6.0-alpha.1 wrote
		// no size, so validating it would fail and delete a complete, verified
		// server: the exact release this exists to rescue. All that is needed
		// to move a set is where it belongs. Staged() reads the same file the
		// same way.
		var staged Manifest
		if err := json.Unmarshal(record, &staged); err != nil {
			continue
		}
		// Checked because they are about to be path components, not because
		// the record is distrusted.
		if !platformPattern.MatchString(staged.OS) || !platformPattern.MatchString(staged.Arch) ||
			!versionPattern.MatchString(staged.Version) {
			continue
		}
		dir := staged.Dir(root)
		if _, err := os.Stat(dir); err == nil {
			// Already staged where it belongs, so this copy is a duplicate and
			// its bytes are not worth keeping. Renamed aside rather than
			// deleted in place, which is what commit does with the set it
			// replaces and for the same reason: a RemoveAll over a running
			// discobox-server.exe takes the children it can and stops at the
			// locked one, and a directory left holding the exe without its
			// manifest.json is invisible to this function — no manifest to
			// identify it — and unmatched by the sweep. Under a .replaced-
			// name the sweep keeps retrying it until it goes.
			aside := fmt.Sprintf("%s.replaced-%d", legacy, time.Now().UnixNano())
			if err := os.Rename(legacy, aside); err != nil {
				continue
			}
			_ = os.RemoveAll(aside)
			continue
		}
		// A move that fails is left for the next run, never deleted. The
		// reasons it fails — a full disk, a file in the way, a locked
		// executable — say nothing about whether the set is wanted, and the
		// compounding case is the bad one: delete on ENOSPC and the download
		// that replaces it fails on ENOSPC too, leaving a machine with no
		// server and, if it is offline, no way to get one. On Windows it is
		// worse than losing bytes: RemoveAll deletes what it can before failing
		// on a running discobox-server.exe, and a directory left holding the
		// exe without its manifest.json is invisible to this function forever.
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			continue
		}
		_ = os.Rename(legacy, dir)
	}
}

// download fetches one asset, hashing it as it is written. The digest is
// compared before the file is anything but a temporary name, so a mismatch
// leaves nothing behind that could be run.
// download fetches an asset, trying its URLs in order and stopping at the
// first whose bytes match the manifest's digest (ADR 0106 §2).
//
// Order is the manifest's, so which source is preferred is a decision the
// release made rather than one taken here. A source that fails costs the time
// it took to fail; the digest decides success either way, so falling through
// to the next one is never a fall back to something less verified.
func download(ctx context.Context, client *http.Client, asset Asset, path string, report func(current, total int64)) error {
	var failures []error
	for _, source := range asset.URLs {
		err := downloadFrom(ctx, client, asset, source, path, report)
		if err == nil {
			return nil
		}
		failures = append(failures, err)
		// The caller giving up is not this source failing, and trying the rest
		// against a dead context would turn one cancellation into a list of
		// them.
		if ctx.Err() != nil {
			break
		}
		// A failed attempt leaves a partial file and the next one creates with
		// O_EXCL. Nothing here resumes: a new source is a new file, because a
		// digest over two sources' bytes spliced together means nothing.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
			break
		}
	}
	return errors.Join(failures...)
}

func downloadFrom(ctx context.Context, client *http.Client, asset Asset, source, path string, report func(current, total int64)) error {
	if client == nil {
		client = defaultClient()
	}
	// Canceled by the stall watchdog as well as by the caller, which is what
	// ends a body that stopped arriving: closing the response is not enough,
	// because the read is already blocked inside it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s: %s", asset.Name, source, resp.Status)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, assetMode(asset))
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	digest := sha256.New()
	counted := &countingWriter{
		to:      io.MultiWriter(file, digest),
		stalled: &stalled,
		// The manifest's size, not the response's. GitHub serves a release
		// asset with no Content-Length at all, so a download that asked the
		// transport how big the file was got -1 and could only count upwards —
		// which is what the first alpha's progress line actually did.
		total:  asset.Size,
		report: report,
	}
	stop := counted.start(cancel)
	// One byte past the declared size, which is enough to tell "exactly right"
	// from "more than promised" and stops a URL that has started serving
	// something else from filling the disk before the size check rejects it.
	_, copyErr := io.Copy(counted, io.LimitReader(resp.Body, asset.Size+1))
	stop()
	// Sync before Close: the rename that installs this set is what makes it
	// visible, and on ext4 or xfs that rename can reach the journal ahead of
	// these data blocks. Without this a power loss just after a first
	// `discobox run` leaves a complete-looking directory holding a truncated
	// binary — and nothing re-verifies a staged set, so the next command runs
	// it.
	syncErr := file.Sync()
	closeErr := file.Close()
	switch {
	case copyErr != nil:
		if stalled.Load() {
			return fmt.Errorf("download %s: %s stopped sending after %s", asset.Name, source, stallTimeout)
		}
		return fmt.Errorf("download %s: %w", asset.Name, copyErr)
	case syncErr != nil:
		return fmt.Errorf("download %s: %w", asset.Name, syncErr)
	case closeErr != nil:
		return fmt.Errorf("download %s: %w", asset.Name, closeErr)
	}
	// Size before digest, because it is the more legible complaint for the case
	// that actually happens — a truncated download, or a URL that now serves
	// something else entirely — and a digest mismatch says only that the bytes
	// differ.
	if got := counted.current.Load(); got != asset.Size {
		return fmt.Errorf("%s from %s is %d bytes, not the %d this build expects", asset.Name, source, got, asset.Size)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, asset.SHA256) {
		return fmt.Errorf("%s from %s has digest %s, not the %s this build expects", asset.Name, source, got, asset.SHA256)
	}
	// The mode again, because the umask took bits off the create. An asset is
	// this user's alone: everything under the state directory is, and this one
	// is a file that gets executed.
	if err := os.Chmod(path, assetMode(asset)); err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	return nil
}

func assetMode(asset Asset) os.FileMode {
	if asset.Executable {
		return 0o700
	}
	return 0o600
}

// defaultClient is what fetches an asset when a caller supplies no client.
//
// No Client.Timeout: that bounds the whole exchange including the body, and a
// hundred megabytes over a slow link is a long exchange that is working fine.
// What is bounded is every step where silence means failure — connecting,
// negotiating TLS, waiting for the response to begin — with the body itself
// covered by the stall watchdog, which measures silence rather than duration.
func defaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// countingWriter reports the bytes passing through it, on a ticker, and ends a
// download that has stopped arriving.
type countingWriter struct {
	to     io.Writer
	total  int64
	report func(current, total int64)
	// stalled records that the watchdog is what ended this download, so the
	// error can say so rather than reporting the cancellation it caused.
	stalled *atomic.Bool

	current atomic.Int64
	stop    chan struct{}
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.to.Write(p)
	w.current.Add(int64(n))
	return n, err
}

// start begins reporting and watching, and returns the func that ends both.
// The returned func emits this asset's closing count.
//
// The ticker is already running for the reports, so the stall watch rides on
// it rather than arming a timer of its own: every tick that finds the byte
// count where the last one left it is a tick of silence, and enough of them
// cancels the request.
func (w *countingWriter) start(cancel context.CancelFunc) func() {
	w.stop = make(chan struct{})
	finished := make(chan struct{})
	// A report before the first tick, so a download that is about to be slow
	// says what it is doing immediately rather than after the interval.
	w.emit()
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		last, since := w.current.Load(), time.Now()
		for {
			select {
			case <-w.stop:
				return
			case now := <-ticker.C:
				current := w.current.Load()
				if current != last {
					last, since = current, now
					w.emit()
					continue
				}
				w.emit()
				if now.Sub(since) >= stallTimeout {
					if w.stalled != nil {
						w.stalled.Store(true)
					}
					cancel()
					return
				}
			}
		}
	}()
	return func() {
		close(w.stop)
		<-finished
		w.emit()
	}
}

func (w *countingWriter) emit() {
	if w.report != nil {
		w.report(w.current.Load(), w.total)
	}
}

// reporter turns per-asset byte counts into the Progress a caller sees.
type reporter struct {
	on     func(Progress)
	assets int
}

func newReporter(on func(Progress), assets int) *reporter {
	return &reporter{on: on, assets: assets}
}

func (r *reporter) forAsset(name string, index int) func(current, total int64) {
	if r == nil || r.on == nil {
		return nil
	}
	return func(current, total int64) {
		r.on(Progress{
			Asset:   name,
			Index:   index,
			Assets:  r.assets,
			Current: current,
			Total:   total,
		})
	}
}

// done is the closing report for the staging as a whole, sent once the assets
// are in place rather than once the last byte arrived.
func (r *reporter) done() {
	if r == nil || r.on == nil {
		return
	}
	r.on(Progress{Assets: r.assets, Done: true})
}
