package cache

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cacheableRequest(t *testing.T, digest string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://registry.example.com/v2/app/blobs/sha256:"+strings.Repeat(digest, 64), nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func storeEntry(t *testing.T, c *Cache, key string, body string) {
	t.Helper()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}}
	put, err := c.BeginStreamingPut(key, resp)
	if err != nil {
		t.Fatalf("BeginStreamingPut() error = %v", err)
	}
	if _, err := put.Write([]byte(body)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := put.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
}

// An entry file without its sidecar is unreachable: the index is keyed by the
// cache key, which only the sidecar holds, and the filename is a hash of that
// key. Left alone it can never be found, counted, or evicted — it is a
// permanent leak that the size ceiling cannot see.
func TestStartupReclaimsEntriesWithNoSidecar(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 4096, ContentAware: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	key := c.Matcher().GenerateKey(cacheableRequest(t, "a"))
	storeEntry(t, c, key, "layer-bytes")
	if err := os.Remove(filepath.Join(dir, cacheKey(key)+metaSuffix)); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	reopened, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 4096, ContentAware: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, cacheKey(key))); !os.IsNotExist(err) {
		t.Fatal("an entry with no sidecar survived startup")
	}
	if size := reopened.Stats().CurrentSize; size != 0 {
		t.Fatalf("CurrentSize = %d, want 0 after the unreachable entry was reclaimed", size)
	}
}

func TestStartupReclaimsAbandonedTempFilesAndStraySidecars(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "deadbeef"+tempMarker+"123456")
	stray := filepath.Join(dir, "cafebabe"+metaSuffix)
	if err := os.WriteFile(temp, []byte("half a layer"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := os.WriteFile(stray, []byte("some-key"), 0o600); err != nil {
		t.Fatalf("write stray sidecar: %v", err)
	}

	if _, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 4096, ContentAware: true}); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for _, path := range []string{temp, stray} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived startup", filepath.Base(path))
		}
	}
}

func TestStartupKeepsCompleteEntries(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 4096, ContentAware: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	key := c.Matcher().GenerateKey(cacheableRequest(t, "b"))
	storeEntry(t, c, key, "layer-bytes")

	reopened, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 4096, ContentAware: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	entry, err := reopened.Get(key)
	if err != nil {
		t.Fatalf("Get() after restart error = %v", err)
	}
	defer entry.Body.Close()
}

// Commit publishes the sidecar before the entry, so a crash between them leaks
// a sidecar of a few dozen bytes rather than the whole body.
func TestCommitWritesTheSidecarBeforePublishingTheEntry(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 4096, ContentAware: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	key := c.Matcher().GenerateKey(cacheableRequest(t, "c"))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}}
	put, err := c.BeginStreamingPut(key, resp)
	if err != nil {
		t.Fatalf("BeginStreamingPut() error = %v", err)
	}
	if _, err := put.Write([]byte("layer")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// Publishing into an occupied name is the only way to fail the rename from
	// here, so the entry path is made a non-empty directory first.
	if err := os.MkdirAll(filepath.Join(dir, cacheKey(key), "occupied"), 0o700); err != nil {
		t.Fatalf("occupy entry path: %v", err)
	}
	if err := put.Commit(); err == nil {
		t.Fatal("Commit() succeeded over an occupied entry path")
	}
	if _, err := os.Stat(filepath.Join(dir, cacheKey(key)+metaSuffix)); !os.IsNotExist(err) {
		t.Fatal("a failed Commit left its sidecar behind")
	}
}

// A ceiling that was lowered between runs has to be honored on the way in: the
// eviction that would notice may never come.
func TestStartupEvictsDownToALoweredCeiling(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 4096, ContentAware: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	storeEntry(t, c, c.Matcher().GenerateKey(cacheableRequest(t, "d")), strings.Repeat("x", 512))
	storeEntry(t, c, c.Matcher().GenerateKey(cacheableRequest(t, "e")), strings.Repeat("y", 512))

	reopened, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 600, ContentAware: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if size := reopened.Stats().CurrentSize; size > 600 {
		t.Fatalf("CurrentSize = %d, want the lowered ceiling of 600 honored at startup", size)
	}
}
