package guestimage

import (
	"context"
	"crypto/rand"
	"io"
	golog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A layer whose transfer is cut partway is fetched again, and while it is the
// guest image's report says so. Without it the status line shows a count that
// pauses for the retry's backoff, or steps back, with nothing saying why.
func TestResolveReportsALayerFetchedAgain(t *testing.T) {
	const cutAt = 256 << 10
	root := make([]byte, 1<<20)
	if _, err := rand.Read(root); err != nil {
		t.Fatalf("random root: %v", err)
	}
	reference := pushCutGuestImage(t, map[string][]byte{"root.ext4": root}, cutAt)
	resolver, err := New(Config{
		Reference: reference,
		Images:    imagecache.Open(t.TempDir()),
		CacheDir:  t.TempDir(),
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "root.ext4"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	var reports []Progress
	if _, err := resolver.Resolve(context.Background(), func(progress Progress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, progress)
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var retried *imagecache.Retry
	for _, report := range reports {
		if report.Retry != nil {
			retried = report.Retry
			break
		}
	}
	if retried == nil {
		t.Fatalf("no report of %d named the layer being fetched again", len(reports))
	}
	if retried.Layer < 1 || retried.Attempt != 2 || retried.Kept <= 0 || retried.Kept > cutAt {
		t.Errorf("retry = %+v, want a layer from 1, attempt 2, and at most %d bytes kept", retried, cutAt)
	}
	if last := reports[len(reports)-1]; !last.Done || last.Retry != nil {
		t.Errorf("closing report = %+v, want done and no retry left on it", last)
	}
}

// pushCutGuestImage is pushGuestImage with a registry that ends the first
// transfer of any blob bigger than cutAt after cutAt bytes, as a connection
// that drops mid-layer does.
//
// It also ignores Range, sending the blob whole again, which is the retry that
// steps the count back. That is a registry the store has to handle, and the
// only one this test can have: go-containerregistry's in-process registry
// refuses the open-ended range a resumption asks for.
func pushCutGuestImage(t *testing.T, contents map[string][]byte, cutAt int) string {
	t.Helper()
	handler := registry.New(registry.Logger(golog.New(io.Discard, "", 0)))
	var armed atomic.Bool
	var cut sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		if armed.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			r.Header.Del("Range")
			if _, done := cut.LoadOrStore(r.URL.Path, true); !done {
				handler.ServeHTTP(&cuttingWriter{ResponseWriter: w, left: cutAt}, r)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	host, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}
	image, err := crane.Image(contents)
	if err != nil {
		t.Fatalf("build guest image: %v", err)
	}
	tag, err := name.NewTag(host.Host + "/discobox-guest:test")
	if err != nil {
		t.Fatalf("build tag: %v", err)
	}
	if err := remote.Write(tag, image); err != nil {
		t.Fatalf("push guest image: %v", err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("image digest: %v", err)
	}
	armed.Store(true)
	return tag.Repository.Name() + "@" + digest.String()
}

// cuttingWriter passes through the first left bytes of a response and then
// aborts it, so the client reads a short body against its Content-Length. A
// response no bigger than left goes through whole.
type cuttingWriter struct {
	http.ResponseWriter
	left int
}

func (c *cuttingWriter) Write(p []byte) (int, error) {
	if len(p) <= c.left {
		c.left -= len(p)
		return c.ResponseWriter.Write(p)
	}
	_, _ = c.ResponseWriter.Write(p[:c.left])
	if flusher, ok := c.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	panic(http.ErrAbortHandler)
}

// Both VM drivers report the fetch through PoolPull, so a retry it drops is
// one no pool's status line can show.
func TestPoolPullCarriesTheRetry(t *testing.T) {
	pull := Progress{
		Reference:      "ghcr.io/discobox-ai/discobox-vm:v1",
		Current:        120 << 20,
		Total:          400 << 20,
		Layers:         9,
		LayersComplete: 2,
		Retry:          &imagecache.Retry{Layer: 3, Attempt: 2, Kept: 172 << 20, Err: io.ErrUnexpectedEOF},
	}.PoolPull()
	want := sandbox.PoolPullProgress{
		Image:          "ghcr.io/discobox-ai/discobox-vm:v1",
		Current:        120 << 20,
		Total:          400 << 20,
		Layers:         9,
		LayersComplete: 2,
		Retry:          &sandbox.PoolPullRetry{Layer: 3, Attempt: 2, Kept: 172 << 20},
	}
	if pull.Retry == nil || *pull.Retry != *want.Retry {
		t.Fatalf("PoolPull retry = %+v, want %+v", pull.Retry, want.Retry)
	}
	pull.Retry, want.Retry = nil, nil
	if *pull != want {
		t.Fatalf("PoolPull = %+v, want %+v", *pull, want)
	}
	if done := (Progress{Reference: "x", Done: true}).PoolPull(); done.Retry != nil || !done.Done {
		t.Fatalf("PoolPull of a closing report = %+v, want done and no retry", done)
	}
}
