package assetcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/proxy"
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakeServer is the control plane's asset API: it answers what it holds,
// counts each path it is asked, and can hold blob answers until released.
type fakeServer struct {
	t       *testing.T
	content map[string][]byte
	// corrupt names digests answered with other bytes.
	corrupt map[string]bool
	hold    chan struct{}
	mu      sync.Mutex
	asked   map[string]int
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		f.t.Errorf("request %s carried no pool assertion", r.URL.Path)
	}
	f.mu.Lock()
	f.asked[r.URL.Path]++
	f.mu.Unlock()
	digest := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if strings.Contains(r.URL.Path, "/blobs/") && f.hold != nil {
		<-f.hold
	}
	data, ok := f.content[digest]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.corrupt[digest] {
		data = []byte("not the blob")
	}
	_, _ = w.Write(data)
}

func (f *fakeServer) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked[path]
}

func (f *fakeServer) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.asked {
		n += c
	}
	return n
}

type bundle struct {
	config, layer, manifest []byte
}

func (f *fakeServer) add(b ...[]byte) {
	for _, data := range b {
		f.content[digestOf(data)] = data
	}
}

func newBundle(t *testing.T, seed string) bundle {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","seed":"` + seed + `"}`)
	layer := []byte("layer of " + seed)
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     imagecache.MediaTypeOCIManifest,
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digestOf(config), "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": digestOf(layer), "size": len(layer)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bundle{config: config, layer: layer, manifest: manifest}
}

func indexOf(t *testing.T, manifests ...[]byte) []byte {
	t.Helper()
	var entries []any
	for _, m := range manifests {
		entries = append(entries, map[string]any{"mediaType": imagecache.MediaTypeOCIManifest, "digest": digestOf(m), "size": len(m)})
	}
	data, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": imagecache.MediaTypeOCIIndex, "manifests": entries})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type fixture struct {
	cache     *Cache
	server    *fakeServer
	sandboxes string
	handler   http.Handler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fake := &fakeServer{t: t, content: map[string][]byte{}, corrupt: map[string]bool{}, asked: map[string]int{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	sandboxes := filepath.Join(dir, "sandboxes")
	cache := New(Config{
		Dir:       filepath.Join(dir, "build", "assets"),
		Sandboxes: sandboxes,
		Fetch: func(ctx context.Context, path string) (io.ReadCloser, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/pools/pool_1/assets"+path, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer pool-assertion")
			resp, err := srv.Client().Do(req)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("status %d", resp.StatusCode)
			}
			return resp.Body, nil
		},
	})
	return &fixture{cache: cache, server: fake, sandboxes: sandboxes, handler: cache.Handler()}
}

func (f *fixture) pin(t *testing.T, sandboxID string, pins ...Pin) {
	t.Helper()
	if err := WriteRecord(filepath.Join(f.sandboxes, sandboxID), pins); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) get(sandboxID, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, "http://assets.discobox.internal"+path, nil)
	if sandboxID != "" {
		req.Header.Set(proxy.PoolClientHeader, sandboxID)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

const serverPrefix = "/api/pools/pool_1/assets"

// Concurrent reads of one blob, by two sandboxes pinning it, share one fetch
// from the server, and every one of them is answered the blob.
func TestConcurrentSandboxesShareOneFetch(t *testing.T) {
	f := newFixture(t)
	harness := newBundle(t, "harness")
	f.server.add(harness.config, harness.layer, harness.manifest)
	pin := Pin{Kind: KindHarness, Name: "discobox-harness-claude-code", Digest: digestOf(harness.manifest)}
	f.pin(t, "sbx_a", pin)
	f.pin(t, "sbx_b", pin)
	f.server.hold = make(chan struct{})

	const readers = 8
	var wg sync.WaitGroup
	var ok atomic.Int32
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sandbox := []string{"sbx_a", "sbx_b"}[i%2]
			rec := f.get(sandbox, http.MethodGet, "/blobs/"+digestOf(harness.layer))
			if rec.Code == http.StatusOK && rec.Body.String() == string(harness.layer) {
				ok.Add(1)
			}
		}()
	}
	// Let every reader reach the shared fetch before the server answers it.
	deadline := time.Now().Add(5 * time.Second)
	for f.server.count(serverPrefix+"/blobs/"+digestOf(harness.layer)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(f.server.hold)
	wg.Wait()

	if ok.Load() != readers {
		t.Fatalf("%d of %d readers were answered the blob", ok.Load(), readers)
	}
	if got := f.server.count(serverPrefix + "/blobs/" + digestOf(harness.layer)); got != 1 {
		t.Fatalf("server was asked for the blob %d times, want once", got)
	}
	if got := f.server.count(serverPrefix + "/discobox-harness-claude-code/manifests/" + digestOf(harness.manifest)); got != 1 {
		t.Fatalf("server was asked for the manifest %d times, want once", got)
	}
}

// A sandbox is answered what its own record pins, under an index too, and
// refused another sandbox's manifest and blobs without the server being asked.
func TestASandboxIsRefusedAnotherSandboxsBundle(t *testing.T) {
	f := newFixture(t)
	platform, harness := newBundle(t, "platform"), newBundle(t, "harness")
	index := indexOf(t, platform.manifest)
	f.server.add(platform.config, platform.layer, platform.manifest, index, harness.config, harness.layer, harness.manifest)
	f.pin(t, "sbx_a", Pin{Kind: KindPlatform, Name: "discobox-platform", Digest: digestOf(index)})
	f.pin(t, "sbx_b", Pin{Kind: KindHarness, Name: "discobox-harness-shell", Digest: digestOf(harness.manifest)})

	// sbx_a reads its platform: the name, the index's manifest, its layer.
	rec := f.get("sbx_a", http.MethodGet, "/discobox-platform")
	if rec.Code != http.StatusOK || rec.Body.String() != string(index) || rec.Header().Get("Content-Type") != imagecache.MediaTypeOCIIndex ||
		rec.Header().Get("Docker-Content-Digest") != digestOf(index) {
		t.Fatalf("name read = %d %s %q", rec.Code, rec.Header(), rec.Body.String())
	}
	if rec := f.get("sbx_a", http.MethodGet, "/discobox-platform/manifests/"+digestOf(platform.manifest)); rec.Code != http.StatusOK || rec.Body.String() != string(platform.manifest) {
		t.Fatalf("listed manifest read = %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.get("sbx_a", http.MethodHead, "/blobs/"+digestOf(platform.layer)); rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD layer = %d len %d", rec.Code, rec.Body.Len())
	}

	asked := f.server.total()
	refused := []string{
		"/discobox-platform",
		"/discobox-platform/manifests/" + digestOf(index),
		"/discobox-platform/manifests/" + digestOf(platform.manifest),
		"/blobs/" + digestOf(platform.layer),
		"/blobs/" + digestOf(platform.config),
	}
	for _, path := range refused {
		if rec := f.get("sbx_b", http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("sbx_b GET %s = %d, want 404", path, rec.Code)
		}
	}
	// sbx_b's own manifest is read to walk its scope; nothing of sbx_a's is
	// asked of the server.
	if got := f.server.total() - asked; got != 1 || f.server.count(serverPrefix+"/discobox-harness-shell/manifests/"+digestOf(harness.manifest)) != 1 {
		t.Fatalf("refusing sbx_b asked the server %d more times: %v", got, f.server.asked)
	}
	// A sandbox with no record, and a request naming none, are answered nothing.
	if rec := f.get("sbx_c", http.MethodGet, "/blobs/"+digestOf(platform.layer)); rec.Code != http.StatusNotFound {
		t.Errorf("unrecorded sandbox = %d, want 404", rec.Code)
	}
	if rec := f.get("", http.MethodGet, "/blobs/"+digestOf(platform.layer)); rec.Code != http.StatusForbidden {
		t.Errorf("no sandbox named = %d, want 403", rec.Code)
	}
	if rec := f.get("../sbx_a", http.MethodGet, "/blobs/"+digestOf(platform.layer)); rec.Code != http.StatusNotFound {
		t.Errorf("a sandbox ID that is a path = %d, want 404", rec.Code)
	}
	if rec := f.get("sbx_a", http.MethodPost, "/blobs/"+digestOf(platform.layer)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
}

// Bytes from the server that are not the digest asked for are refused, and
// nothing is stored under it.
func TestServerBytesThatAreNotTheDigestAreRefused(t *testing.T) {
	f := newFixture(t)
	harness := newBundle(t, "harness")
	f.server.add(harness.config, harness.layer, harness.manifest)
	f.server.corrupt[digestOf(harness.layer)] = true
	f.pin(t, "sbx_a", Pin{Kind: KindHarness, Name: "h", Digest: digestOf(harness.manifest)})

	if rec := f.get("sbx_a", http.MethodGet, "/blobs/"+digestOf(harness.layer)); rec.Code != http.StatusBadGateway {
		t.Fatalf("corrupt blob = %d, want 502", rec.Code)
	}
	if f.cache.has(digestOf(harness.layer)) {
		t.Fatal("a blob that is not its digest was stored")
	}
	delete(f.server.corrupt, digestOf(harness.layer))
	if rec := f.get("sbx_a", http.MethodGet, "/blobs/"+digestOf(harness.layer)); rec.Code != http.StatusOK || rec.Body.String() != string(harness.layer) {
		t.Fatalf("blob after the server is right = %d", rec.Code)
	}
}

// The sweep removes what no record reaches, once it is old enough, and keeps
// what a record still pins.
func TestSweepRemovesWhatNoRecordReaches(t *testing.T) {
	f := newFixture(t)
	kept, dropped := newBundle(t, "kept"), newBundle(t, "dropped")
	f.server.add(kept.config, kept.layer, kept.manifest, dropped.config, dropped.layer, dropped.manifest)
	f.pin(t, "sbx_a", Pin{Kind: KindHarness, Name: "kept", Digest: digestOf(kept.manifest)})
	f.pin(t, "sbx_b", Pin{Kind: KindHarness, Name: "dropped", Digest: digestOf(dropped.manifest)})
	for _, read := range []struct{ sandbox, path string }{
		{"sbx_a", "/blobs/" + digestOf(kept.layer)},
		{"sbx_a", "/blobs/" + digestOf(kept.config)},
		{"sbx_b", "/blobs/" + digestOf(dropped.layer)},
	} {
		if rec := f.get(read.sandbox, http.MethodGet, read.path); rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d", read.sandbox, read.path, rec.Code)
		}
	}
	f.pin(t, "sbx_b") // its record is emptied

	// Too new to go: a fetch for a create since the sweep read the records.
	if removed, err := f.cache.Sweep(); err != nil || removed != 0 {
		t.Fatalf("Sweep() within the grace = %d, %v", removed, err)
	}
	f.cache.now = func() time.Time { return time.Now().Add(2 * sweepGrace) }
	removed, err := f.cache.Sweep()
	if err != nil || removed != 2 {
		t.Fatalf("Sweep() = %d, %v; want the dropped manifest and layer", removed, err)
	}
	for _, data := range [][]byte{kept.manifest, kept.layer, kept.config} {
		if !f.cache.has(digestOf(data)) {
			t.Errorf("kept %s was removed", digestOf(data))
		}
	}
	for _, data := range [][]byte{dropped.manifest, dropped.layer} {
		if f.cache.has(digestOf(data)) {
			t.Errorf("dropped %s was kept", digestOf(data))
		}
	}
}

func TestRecordRoundTripsAndRefusesBadPins(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sbx_a")
	pins := []Pin{{Kind: KindPlatform, Name: "discobox-platform", Digest: digestOf([]byte("p"))}, {Kind: KindHarness, Name: "discobox-harness-shell", Digest: digestOf([]byte("h"))}}
	if err := WriteRecord(root, pins); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRecord(root)
	if err != nil || len(got) != 2 || got[0] != pins[0] || got[1] != pins[1] {
		t.Fatalf("ReadRecord() = %v, %v", got, err)
	}
	for _, bad := range []Pin{{Kind: KindHarness, Name: "../x", Digest: pins[0].Digest}, {Kind: KindHarness, Name: "x", Digest: "sha256:nope"}, {Kind: KindHarness, Digest: pins[0].Digest}, {Name: "x", Digest: pins[0].Digest}} {
		if err := WriteRecord(root, []Pin{bad}); err == nil {
			t.Errorf("WriteRecord(%v) accepted", bad)
		}
	}
	if got, _ := ReadRecord(root); len(got) != 2 {
		t.Fatalf("a refused write changed the record: %v", got)
	}
	if err := WriteRecord(root, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRecord(root); err != nil || got != nil {
		t.Fatalf("an emptied record = %v, %v", got, err)
	}
}
