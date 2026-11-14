package imagecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
)

const (
	// progressInterval is how often a download in flight reports. On a ticker
	// rather than per read, so a stalled download keeps saying so and a fast
	// one does not call back once per buffer.
	progressInterval = 200 * time.Millisecond
	// downloadConcurrency is how many blobs of one image download at once. A
	// registry's CDN serves one connection well below a fast link's rate, and
	// an image is a handful of large layers.
	downloadConcurrency = 4
	// downloadAttempts is how many times one blob is tried before the image's
	// staging fails. Only the blob is retried: everything already in place
	// stays, which is what a content-addressed layout buys.
	downloadAttempts = 3
	// pruneAfter is how long an entry nothing has staged since is kept. It
	// matches ADR 0040's image retention: long enough that alternating between
	// two installed versions does not re-download either.
	pruneAfter = 24 * time.Hour
	// abandonedAge is how old an unreferenced blob or a temporary must be
	// before pruning removes it, so a download another process has in flight
	// right now — whose blobs no entry names yet — is never deleted under it.
	abandonedAge = time.Hour
)

// Progress is one report about a staging in flight, shaped for the status line
// the server's own download (serverstage.Progress) is narrated on moments
// earlier, rather than for a progress bar.
type Progress struct {
	// Image is the reference being staged, Index its position from 1, and
	// Images how many there are.
	Image  string
	Index  int
	Images int
	// Current and Total are bytes of what this image still needed when its
	// download began. Total is known from the manifests before the first byte,
	// and never grows. Current is what is on disk: a blob's transfer that ends
	// early keeps its bytes for the retry to carry on from, and one the
	// registry sends again from the start takes them back off.
	Current int64
	Total   int64
	// Layers and LayersComplete count the blobs this image still needed: how
	// many are being fetched and how many are in. Both are zero for an image
	// the layout already held, which is fetching nothing.
	Layers         int
	LayersComplete int
	// Retry, when set, is the blob most recently being fetched again after its
	// transfer ended early, for as long as it still is.
	Retry *Retry
	// Done marks the closing report, sent once every image is staged.
	Done bool
}

// Retry describes a blob being fetched again after a transfer of it ended
// early.
type Retry struct {
	// Layer is the blob's position among Progress.Layers, from 1.
	Layer int
	// Attempt is which try this is, from 2.
	Attempt int
	// Kept is how many of the blob's bytes the earlier attempts left on disk,
	// which the retry asks the registry for the rest after. Zero when they
	// left none, or left bytes that could never be the blob. A registry that
	// ignores the request for the rest sends the whole blob again, and those
	// bytes come back off Progress.Current.
	Kept int64
	// Err is what ended the last attempt.
	Err error
}

// Options configures a staging.
type Options struct {
	// Platform is the one platform staged. Empty is PoolPlatform, which is the
	// right answer for every pool on this machine.
	Platform Platform
	// Client fetches from registries. Nil is a client configured for downloads
	// that may be long.
	Client *http.Client
	// OnProgress, when set, is called while images are downloaded.
	OnProgress func(Progress)
	// Credentials, when set, is asked for a registry's credentials when that
	// registry refuses an anonymous request. Nil stages anonymously.
	Credentials func(ctx context.Context, registry string) Credentials
	// Revalidate asks the registry what a tag names even when the layout
	// already holds a complete image for it, so a tag that moved is followed.
	// Blobs already present are still not fetched again. A release's tags do
	// not move and are not revalidated; a tag a developer pushes to does.
	Revalidate bool
}

// Staged is one image a staging left in the layout.
type Staged struct {
	Reference string
	Digest    string
	// Downloaded is how many bytes this staging fetched for it: zero when the
	// layout already held all of it.
	Downloaded int64
}

// Stage makes every reference present in the layout for one platform,
// downloading only the blobs the layout does not already hold.
//
// Each reference is staged on its own, and a failure is collected rather than
// ending the staging: the images are a head start, and four of five is a
// better start than none. What completed is returned alongside the joined
// failures.
//
// Only after every reference is staged does the layout shed what nothing asked
// for (see prune), because a failed staging is no evidence that what it did not
// reach is unwanted.
func (l *Layout) Stage(ctx context.Context, references []string, opts Options) ([]Staged, error) {
	if opts.Platform == (Platform{}) {
		opts.Platform = PoolPlatform()
	}
	if err := l.init(); err != nil {
		return nil, err
	}
	s := newStager(l, opts)
	var staged []Staged
	var failures []error
	keep := make(map[string]bool, len(references))
	for i, reference := range references {
		// Parsed here rather than up front, so a reference this package will
		// not take — one naming no tag and no digest, which Docker would read
		// as :latest — costs its own image and not the other five.
		ref, err := ParseReference(reference)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		keep[ref.Name()] = true
		result, err := s.stage(ctx, ref, i+1, len(references))
		if err != nil {
			// The caller giving up is not this image failing, and trying the
			// rest against a dead context would turn one cancellation into a
			// list of them.
			if ctx.Err() != nil {
				return staged, ctx.Err()
			}
			failures = append(failures, err)
			continue
		}
		staged = append(staged, result)
	}
	if len(failures) > 0 {
		return staged, errors.Join(failures...)
	}
	s.emit(Progress{Images: len(references), Done: true})
	// Best effort: a layout that could not be tidied is still a staged one.
	_ = l.prune(keep, time.Now())
	return staged, nil
}

// Fetch makes one reference present in the layout for one platform and returns
// it, downloading only the blobs the layout does not already hold.
//
// It is Stage for one image, for a caller that wants the image rather than a
// report — a provider that boots what is in it (ADR 0113 §5). Nothing is
// pruned: one image is no evidence about the rest.
func (l *Layout) Fetch(ctx context.Context, reference string, opts Options) (*Image, error) {
	if opts.Platform == (Platform{}) {
		opts.Platform = PoolPlatform()
	}
	ref, err := ParseReference(reference)
	if err != nil {
		return nil, err
	}
	if err := l.init(); err != nil {
		return nil, err
	}
	s := newStager(l, opts)
	staged, err := s.stage(ctx, ref, 1, 1)
	if err != nil {
		return nil, err
	}
	// Closed only when it was opened: an image already on disk reported
	// nothing, and a closing report for it would be a download that never was.
	if s.reported {
		s.emit(Progress{
			Image: ref.Name(), Index: 1, Images: 1,
			Current: staged.Downloaded, Total: staged.Downloaded,
			Layers: s.blobs, LayersComplete: s.blobsComplete, Done: true,
		})
	}
	return l.Lookup(ref.Name(), opts.Platform)
}

// init creates the layout's skeleton.
//
// The oci-layout file is written under the index lock: two fetches starting on
// an empty cache would otherwise both find it missing and both rename one into
// place, and Windows refuses a rename over a file another has just put there.
func (l *Layout) init() error {
	if err := os.MkdirAll(l.blobDir(), 0o700); err != nil {
		return fmt.Errorf("create image cache: %w", err)
	}
	path := filepath.Join(l.dir, layoutFileName)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	lock, err := l.lockIndex()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return fsdurable.WriteFile(path, []byte(layoutVersion), 0o600)
}

type stager struct {
	layout     *Layout
	registry   *registry
	platform   Platform
	revalidate bool
	report     func(Progress)
	// reported records that an image went to the registry and said so.
	reported bool
	// blobs and blobsComplete are the last image's fetch, for the closing
	// report to carry the counts the rest of them carried.
	blobs         int
	blobsComplete int
}

func newStager(l *Layout, opts Options) *stager {
	return &stager{
		layout:     l,
		registry:   newRegistry(opts.Client, opts.Credentials),
		platform:   opts.Platform,
		revalidate: opts.Revalidate,
		report:     opts.OnProgress,
	}
}

func (s *stager) emit(progress Progress) {
	if s.report != nil {
		s.reported = true
		s.report(progress)
	}
}

// stage makes one reference complete in the layout.
func (s *stager) stage(ctx context.Context, ref Reference, index, count int) (Staged, error) {
	// Already complete: nothing to fetch, and nothing to ask the registry. A
	// release tag does not move, so an entry whose every blob is present is the
	// image the reference names — unless the caller asked for a tag to be asked
	// about again. A digest cannot move, so it never is.
	if revalidating := s.revalidate && ref.Digest == ""; !revalidating {
		if image, err := s.layout.Lookup(ref.Name(), s.platform); err == nil {
			if err := s.layout.record(ref, image.Top, time.Now()); err != nil {
				return Staged{}, err
			}
			return Staged{Reference: ref.Name(), Digest: image.Digest()}, nil
		}
	}
	progress := Progress{Image: ref.Name(), Index: index, Images: count}
	s.emit(progress)

	topData, top, err := s.registry.manifest(ctx, ref, ref.pinned())
	if err != nil {
		return Staged{}, err
	}
	manifest, manifestData := top, topData
	if isIndex(top.MediaType) {
		child, err := selectPlatform(topData, s.platform)
		if err != nil {
			return Staged{}, fmt.Errorf("%s: %w", ref.Name(), err)
		}
		if manifestData, manifest, err = s.registry.manifest(ctx, ref, child.Digest); err != nil {
			return Staged{}, err
		}
		if !isManifest(manifest.MediaType) {
			return Staged{}, fmt.Errorf("%s: its %s entry is a %s, not an image manifest", ref.Name(), s.platform, manifest.MediaType)
		}
	}
	var m imageManifest
	if err := json.Unmarshal(manifestData, &m); err != nil {
		return Staged{}, fmt.Errorf("%s: read manifest: %w", ref.Name(), err)
	}
	// The manifests go in first. They are small, and they are what a later
	// Lookup reads to learn which blobs make the image.
	if err := s.layout.putBlob(top, topData); err != nil {
		return Staged{}, err
	}
	if err := s.layout.putBlob(manifest, manifestData); err != nil {
		return Staged{}, err
	}

	blobs := append([]Descriptor{m.Config}, m.Layers...)
	var missing []Descriptor
	seen := map[string]bool{}
	for _, blob := range blobs {
		if _, err := s.layout.blobPath(blob.Digest); err != nil {
			return Staged{}, fmt.Errorf("%s: %w", ref.Name(), err)
		}
		if seen[blob.Digest] || s.layout.hasBlob(blob) {
			continue
		}
		seen[blob.Digest] = true
		missing = append(missing, blob)
		progress.Total += blob.Size
	}
	progress.Layers = len(missing)
	downloaded, err := s.download(ctx, ref, missing, progress)
	s.blobs, s.blobsComplete = len(missing), len(missing)
	if err != nil {
		return Staged{}, fmt.Errorf("stage %s: %w", ref.Name(), err)
	}
	// Assembled from the layout rather than from what was just fetched, so what
	// is recorded is exactly what a Lookup will find — and a single-platform
	// image's platform, which only its config states, is checked here.
	if _, err := s.layout.image(ref, top, s.platform); err != nil {
		return Staged{}, err
	}
	if err := s.layout.record(ref, top, time.Now()); err != nil {
		return Staged{}, err
	}
	return Staged{Reference: ref.Name(), Digest: top.Digest, Downloaded: downloaded}, nil
}

// record names top as the reference's entry in the index, stamped with when it
// was staged. The entry is written only once every blob beneath it is present,
// so an entry is a claim that the image is complete.
func (l *Layout) record(ref Reference, top Descriptor, now time.Time) error {
	entry := Descriptor{
		MediaType: top.MediaType,
		Digest:    top.Digest,
		Size:      top.Size,
		Annotations: map[string]string{
			AnnotationImageName: ref.Name(),
			annotationStaged:    now.UTC().Format(time.RFC3339),
		},
	}
	if ref.Tag != "" {
		entry.Annotations[AnnotationRefName] = ref.Tag
	}
	return l.updateIndex(func(idx *imageIndex) {
		if at, ok := findEntry(*idx, ref.Name()); ok {
			idx.Manifests[at] = entry
			return
		}
		idx.Manifests = append(idx.Manifests, entry)
	})
}

// download fetches blobs into the layout, several at a time, reporting the
// bytes as they arrive.
func (s *stager) download(ctx context.Context, ref Reference, blobs []Descriptor, progress Progress) (int64, error) {
	if len(blobs) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var current, complete atomic.Int64
	var retry atomic.Pointer[Retry]
	stopReporting := s.reportWhile(progress, &current, &complete, &retry)
	defer stopReporting()

	type job struct {
		blob  Descriptor
		layer int
	}
	work := make(chan job)
	var once sync.Once
	var first error
	var wg sync.WaitGroup
	for range min(downloadConcurrency, len(blobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range work {
				// Only this blob's own retry is cleared when it ends: another
				// blob's may have replaced it since.
				var mine *Retry
				onRetry := func(r Retry) {
					r.Layer = job.layer
					mine = &r
					retry.Store(mine)
				}
				err := s.fetch(ctx, ref, job.blob, &current, onRetry)
				if mine != nil {
					retry.CompareAndSwap(mine, nil)
				}
				if err == nil {
					complete.Add(1)
				} else {
					// The first failure is the one worth reporting; the rest are
					// the cancellation it caused.
					once.Do(func() { first = err })
					cancel()
				}
			}
		}()
	}
feed:
	for i, blob := range blobs {
		select {
		case work <- job{blob: blob, layer: i + 1}:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return current.Load(), first
}

// reportWhile reports progress now and on every tick until the returned func
// is called, which reports once more.
func (s *stager) reportWhile(progress Progress, current, complete *atomic.Int64, retry *atomic.Pointer[Retry]) func() {
	if s.report == nil {
		return func() {}
	}
	emit := func() {
		progress.Current = current.Load()
		progress.LayersComplete = int(complete.Load())
		progress.Retry = retry.Load()
		s.report(progress)
	}
	emit()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				emit()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
		emit()
	}
}

// fetch downloads one blob, trying again after a failure. Every attempt writes
// into the same temporary, so one whose transfer ended early leaves what it
// received for the next to carry on from. The blob is renamed into place only
// when its digest and its size both match, so a blob's name is a claim about
// its content. held counts its bytes on disk; onRetry is told before each
// attempt after the first.
func (s *stager) fetch(ctx context.Context, ref Reference, blob Descriptor, held *atomic.Int64, onRetry func(Retry)) error {
	if s.layout.hasBlob(blob) {
		// Another staging finished it meanwhile.
		return nil
	}
	part, err := s.layout.createPartial(blob.Digest, blob.Size, held)
	if err != nil {
		return fmt.Errorf("download %s: %w", blob.Digest, err)
	}
	defer part.discard()
	for attempt := 1; ; attempt++ {
		var err error
		if !part.verified {
			err = s.registry.blob(ctx, ref, blob, part)
		}
		if err == nil {
			err = part.commit()
		}
		if err == nil {
			return nil
		}
		err = fmt.Errorf("download %s: %w", blob.Digest, err)
		if ctx.Err() != nil || attempt == downloadAttempts {
			return err
		}
		if !part.verified {
			// A blob that is in hand and only failed to be renamed is not
			// fetched again, so there is no retry of its transfer to report.
			// A temporary that holds every byte and is still not the blob (its
			// sync failed) is fetched again from the first byte: nothing kept.
			kept := part.written
			if kept >= blob.Size {
				kept = 0
			}
			onRetry(Retry{Attempt: attempt + 1, Kept: kept, Err: err})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
		if s.layout.hasBlob(blob) {
			return nil
		}
	}
}

// prune drops the entries nothing staged recently, then the blobs no remaining
// entry reaches.
//
// An entry survives if the staging that just finished asked for it, or if any
// staging asked for it within pruneAfter. A blob survives if a surviving entry
// reaches it, or if it was written within abandonedAge, which is what keeps the
// blobs another process has just downloaded — and not yet named with an entry —
// out of this.
//
// That window is the whole of the guarantee, and it is not the same as "a
// download in flight is safe": a staging writes each blob as it finishes, so an
// image set large enough for its first blob to age past abandonedAge before its
// entry is written can have that blob deleted under it. What that costs is the
// blob again: the staging fails at layout.image, reports that image, and the
// next one re-fetches it.
//
// Nothing acts on what an entry claims without re-checking it, which is what
// makes that the whole cost. Blobs are deleted outside the index lock, so a
// base layer two images share can go after the second's entry was written;
// Lookup re-checks every blob before handing an Image back and reports
// ErrNotStaged, and WriteArchive re-hashes each one on the way out, so such an
// entry reads as not staged and its image is pulled rather than loaded out of a
// hole. Temporaries left by an interrupted download go on the same age.
func (l *Layout) prune(keep map[string]bool, now time.Time) error {
	var kept []Descriptor
	if err := l.updateIndex(func(idx *imageIndex) {
		kept = make([]Descriptor, 0, len(idx.Manifests))
		for _, entry := range idx.Manifests {
			staged, _ := time.Parse(time.RFC3339, entry.Annotations[annotationStaged])
			if keep[entry.Annotations[AnnotationImageName]] || now.Sub(staged) < pruneAfter {
				kept = append(kept, entry)
			}
		}
		idx.Manifests = kept
	}); err != nil {
		return err
	}
	reachable := map[string]bool{}
	for _, entry := range kept {
		l.reach(entry, reachable)
	}
	entries, err := os.ReadDir(l.blobDir())
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if reachable["sha256:"+name] {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < abandonedAge {
			continue
		}
		if err := os.Remove(filepath.Join(l.blobDir(), name)); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// reach marks every blob beneath d that the layout holds: d itself, and under
// an index each child manifest present — only ever the staged platform's —
// with its config and layers.
func (l *Layout) reach(d Descriptor, reachable map[string]bool) {
	reachable[d.Digest] = true
	data, err := l.readBlob(d, MaxManifestBytes)
	if err != nil {
		return
	}
	switch {
	case isIndex(d.MediaType):
		var idx imageIndex
		if json.Unmarshal(data, &idx) != nil {
			return
		}
		for _, child := range idx.Manifests {
			if l.hasBlob(child) {
				l.reach(child, reachable)
			}
		}
	case isManifest(d.MediaType):
		var m imageManifest
		if json.Unmarshal(data, &m) != nil {
			return
		}
		reachable[m.Config.Digest] = true
		for _, layer := range m.Layers {
			reachable[layer.Digest] = true
		}
	}
}
