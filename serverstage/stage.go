package serverstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// progressInterval is how often a download in flight reports. On a ticker
// rather than per read, so a stalled download keeps saying so and a fast one
// does not call back once per buffer.
const progressInterval = 200 * time.Millisecond

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
	if !opts.Force {
		if dir, ok := Staged(opts.Root, m); ok {
			return dir, nil
		}
	}
	if err := os.MkdirAll(opts.Root, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", opts.Root, err)
	}
	// A sibling of the destination, so the move into place is a rename within
	// one filesystem rather than a copy that could half-finish.
	temp, err := os.MkdirTemp(opts.Root, m.Version+".staging-")
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

	dir := m.Dir(opts.Root)
	if err := commit(temp, dir); err != nil {
		// A concurrent stage of the same version is not a failure: the other
		// one verified the same digests this one did, so whichever set landed
		// is the set that was asked for.
		if staged, ok := Staged(opts.Root, m); ok {
			return staged, nil
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
func commit(temp, dir string) error {
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
	if aside != "" {
		_ = os.RemoveAll(aside)
	}
	return nil
}

// download fetches one asset, hashing it as it is written. The digest is
// compared before the file is anything but a temporary name, so a mismatch
// leaves nothing behind that could be run.
func download(ctx context.Context, client *http.Client, asset Asset, path string, report func(current, total int64)) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s: %s", asset.Name, asset.URL, resp.Status)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, assetMode(asset))
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	digest := sha256.New()
	counted := &countingWriter{
		to: io.MultiWriter(file, digest),
		// The manifest's size, not the response's. GitHub serves a release
		// asset with no Content-Length at all, so a download that asked the
		// transport how big the file was got -1 and could only count upwards —
		// which is what the first alpha's progress line actually did.
		total:  asset.Size,
		report: report,
	}
	stop := counted.start()
	_, copyErr := io.Copy(counted, resp.Body)
	stop()
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("download %s: %w", asset.Name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("download %s: %w", asset.Name, closeErr)
	}
	// Size before digest, because it is the more legible complaint for the case
	// that actually happens — a truncated download, or a URL that now serves
	// something else entirely — and a digest mismatch says only that the bytes
	// differ.
	if got := counted.current.Load(); got != asset.Size {
		return fmt.Errorf("%s from %s is %d bytes, not the %d this build expects", asset.Name, asset.URL, got, asset.Size)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, asset.SHA256) {
		return fmt.Errorf("%s from %s has digest %s, not the %s this build expects", asset.Name, asset.URL, got, asset.SHA256)
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

// countingWriter reports the bytes passing through it, on a ticker.
type countingWriter struct {
	to     io.Writer
	total  int64
	report func(current, total int64)

	current atomic.Int64
	stop    chan struct{}
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.to.Write(p)
	w.current.Add(int64(n))
	return n, err
}

// start begins reporting and returns the func that ends it, which emits this
// asset's closing count.
func (w *countingWriter) start() func() {
	if w.report == nil {
		return func() {}
	}
	w.stop = make(chan struct{})
	finished := make(chan struct{})
	// A report before the first tick, so a download that is about to be slow
	// says what it is doing immediately rather than after the interval.
	w.report(0, w.total)
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ticker.C:
				w.report(w.current.Load(), w.total)
			}
		}
	}()
	return func() {
		close(w.stop)
		<-finished
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
