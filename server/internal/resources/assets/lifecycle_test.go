package assets

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// stalledSource resolves nothing until its context ends, and then writes the
// blob store anyway, as a registry answering late would: what Stop must have
// waited out.
type stalledSource struct {
	started  chan struct{}
	returned chan struct{}
}

func (stalledSource) fetch(context.Context, pin, string, bool, *imagecache.Layout) error {
	return errNotHere
}

func (s stalledSource) resolve(ctx context.Context, _ string, _ bool, blobs *imagecache.Layout) (string, error) {
	defer close(s.returned)
	close(s.started)
	<-ctx.Done()
	data := []byte("late")
	return "", errors.Join(ctx.Err(), blobs.PutBlob(digestOf(data), bytes.NewReader(data)))
}

// Stop ends a resolution the service started on its own, which no read
// waits for and which outlives Start's context, and returns only once it has
// returned; after it, nothing fetches into the blob store. A test's TempDir is
// removed after the test, and Windows cannot remove a file still open in it.
func TestStopWaitsForTheReleaseResolution(t *testing.T) {
	f := newFixture(t)
	src := stalledSource{started: make(chan struct{}), returned: make(chan struct{})}
	f.service = newService(store.New(f.db.Write, f.db.Read), Options{Dir: f.dir, PlatformBundle: platformRef}, []source{src}, []source{src})
	moves := 0
	f.service.OnReleaseMoved(func(context.Context) { moves++ })

	f.service.Start(context.Background())
	select {
	case <-src.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not resolve the release")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.service.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-src.returned:
	default:
		t.Fatal("Stop returned before the resolution it started")
	}
	before := blobFiles(t, f.dir)

	// Stopped, it starts nothing: neither its background work nor a move.
	f.service.Start(context.Background())
	f.service.OnReleaseMoved(func(context.Context) { moves++ })
	f.service.platform.moved(digestOf([]byte("moved")))
	if err := f.service.Stop(stopCtx); err != nil {
		t.Fatalf("stop again: %v", err)
	}
	if moves != 0 {
		t.Fatalf("a stopped service told %d moves", moves)
	}
	// Nor does a read, a reconcile or any other caller fetch or resolve: what
	// it would write could outlive Stop.
	if _, err := f.service.platform.pins(context.Background(), true); !errors.Is(err, errStopped) {
		t.Fatalf("resolution on a stopped service = %v, want %v", err, errStopped)
	}
	if err := f.service.ensure(context.Background(), pin{ref: platformRef}, digestOf([]byte("absent")), false); !errors.Is(err, errStopped) {
		t.Fatalf("fetch on a stopped service = %v, want %v", err, errStopped)
	}
	if after := blobFiles(t, f.dir); after != before {
		t.Fatalf("the blob store changed after Stop: %d files, then %d", before, after)
	}
}

// Stop waits, too, for a resolution a read started, which runs detached from
// that read: the read's caller may have returned already.
func TestStopWaitsForAReadsResolution(t *testing.T) {
	f := newFixture(t)
	src := stalledSource{started: make(chan struct{}), returned: make(chan struct{})}
	f.service = newService(store.New(f.db.Write, f.db.Read), Options{Dir: f.dir, PlatformBundle: platformRef}, []source{src}, []source{src})

	readCtx, cancelRead := context.WithCancel(context.Background())
	go func() { _, _ = f.service.CurrentRelease(readCtx) }()
	select {
	case <-src.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not resolve the release")
	}
	cancelRead()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.service.Stop(stopCtx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-src.returned:
	default:
		t.Fatal("Stop returned before the resolution a read started")
	}
}

func blobFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
