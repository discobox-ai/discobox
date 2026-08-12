package cache

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamingCacheStoresAndRestores(t *testing.T) {
	c, err := New(Config{Enabled: true, Dir: t.TempDir(), MaxSizeBytes: 1024, Patterns: []string{`^/v2/.*/blobs/sha256:.*`}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://registry.example.com/v2/app/blobs/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Matcher().ShouldCache(req) {
		t.Fatal("expected request to be cacheable")
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}}
	put, err := c.BeginStreamingPut(c.Matcher().GenerateKey(req), resp)
	if err != nil {
		t.Fatalf("BeginStreamingPut() error = %v", err)
	}
	if _, err := put.Write([]byte("layer")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := put.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	entry, err := c.Get(c.Matcher().GenerateKey(req))
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	defer entry.Body.Close()
	body, err := io.ReadAll(entry.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if strings.TrimSpace(string(body)) != "layer" {
		t.Fatalf("body = %q", body)
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c, err := New(Config{Enabled: true, Dir: t.TempDir(), MaxSizeBytes: 1024, Patterns: []string{`.*`}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	req1 := newCacheTestRequest(t, "https://example.com/one")
	req2 := newCacheTestRequest(t, "https://example.com/two")
	storeCacheEntry(t, c, req1, "first")

	// Room for one entry and not two. The margin is deliberately loose: each
	// entry's header embeds CachedAt, and encoding/json trims trailing zeros
	// from a timestamp's fractional seconds, so an entry is a byte or two
	// shorter whenever the clock lands on one. A margin of +1 made this test
	// depend on which nanosecond it ran — it failed roughly one run in seven —
	// while still being far smaller than the ~110 bytes a whole entry costs,
	// so storing the second one has to evict the first either way.
	const roomToSpare = 20
	c.mu.Lock()
	c.maxSize = c.stats.CurrentSize + roomToSpare
	c.mu.Unlock()

	storeCacheEntry(t, c, req2, "second")

	if _, err := c.Get(c.Matcher().GenerateKey(req1)); err == nil {
		t.Fatal("expected first entry to be evicted")
	}
	surviving, err := c.Get(c.Matcher().GenerateKey(req2))
	if err != nil {
		t.Fatalf("expected second entry to remain: %v", err)
	}
	// A hit hands back an open file. Leaving it open keeps a handle on a file
	// inside t.TempDir(), which Windows will not let the cleanup delete.
	surviving.Body.Close()
}

func TestCacheLoadsExistingIndex(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 1024, Patterns: []string{`.*`}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	req := newCacheTestRequest(t, "https://example.com/layer")
	storeCacheEntry(t, c, req, "cached")

	reopened, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 1024, Patterns: []string{`.*`}})
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	entry, err := reopened.Get(reopened.Matcher().GenerateKey(req))
	if err != nil {
		t.Fatalf("Get() after reopen error = %v", err)
	}
	defer entry.Body.Close()
	body, err := io.ReadAll(entry.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(body) != "cached" {
		t.Fatalf("body = %q", body)
	}
}

func TestCacheCorruptEntryReturnsMissAndRemovesIndex(t *testing.T) {
	dir := t.TempDir()
	c, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 1024, Patterns: []string{`.*`}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	req := newCacheTestRequest(t, "https://example.com/corrupt")
	key := c.Matcher().GenerateKey(req)
	hash := cacheKey(key)
	if err := os.WriteFile(filepath.Join(dir, hash), []byte("not-json\nbody"), 0o600); err != nil {
		t.Fatalf("write corrupt cache entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, hash+".meta"), []byte(key), 0o600); err != nil {
		t.Fatalf("write corrupt cache meta: %v", err)
	}
	reopened, err := New(Config{Enabled: true, Dir: dir, MaxSizeBytes: 1024, Patterns: []string{`.*`}})
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	if _, err := reopened.Get(key); err == nil {
		t.Fatal("expected corrupt entry miss")
	}
	if reopened.index.exists(key) {
		t.Fatal("corrupt entry remained in index")
	}
}

func newCacheTestRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func storeCacheEntry(t *testing.T, c *Cache, req *http.Request, body string) {
	t.Helper()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/plain"}}}
	put, err := c.BeginStreamingPut(c.Matcher().GenerateKey(req), resp)
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
