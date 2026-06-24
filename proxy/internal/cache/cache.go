package cache

import (
	"bufio"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	ErrDisabled = errors.New("cache disabled")
	ErrMiss     = errors.New("cache miss")
)

// Config controls disk response caching.
type Config struct {
	Enabled      bool
	Dir          string
	MaxSizeBytes int64
	Patterns     []string
	ContentAware bool
}

// Cache is a filesystem-backed response cache with in-memory LRU metadata.
type Cache struct {
	enabled bool
	dir     string
	maxSize int64
	matcher *Matcher
	mu      sync.Mutex
	index   *lruIndex
	stats   Stats
}

// Stats contains cache counters.
type Stats struct {
	Hits        int64
	Misses      int64
	Stores      int64
	Evictions   int64
	Errors      int64
	CurrentSize int64
}

// Entry is a cached response.
type Entry struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
	Size       int64
	CachedAt   time.Time
}

type entryHeader struct {
	StatusCode int
	Headers    http.Header
	CachedAt   time.Time
}

// StreamingPut writes a cache entry while the response streams to the client.
type StreamingPut struct {
	cache      *Cache
	key        string
	path       string
	tempPath   string
	metaPath   string
	file       *os.File
	bodySize   int64
	storedSize int64
	digest     hash.Hash
	finalized  bool
}

// New creates a cache.
func New(cfg Config) (*Cache, error) {
	matcher, err := NewMatcher(cfg.Patterns, cfg.ContentAware)
	if err != nil {
		return nil, err
	}
	c := &Cache{enabled: cfg.Enabled, dir: cfg.Dir, maxSize: cfg.MaxSizeBytes, matcher: matcher, index: newLRUIndex()}
	if !cfg.Enabled {
		return c, nil
	}
	if cfg.Dir == "" {
		return nil, fmt.Errorf("cache dir is required")
	}
	if cfg.MaxSizeBytes <= 0 {
		return nil, fmt.Errorf("cache max size must be positive")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	_ = c.loadIndex()
	return c, nil
}

// Matcher returns the cache matcher.
func (c *Cache) Matcher() *Matcher {
	if c == nil {
		return nil
	}
	return c.matcher
}

// Stats returns cache counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Get returns a cached response.
func (c *Cache) Get(key string) (*Entry, error) {
	if c == nil || !c.enabled {
		return nil, ErrDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.index.exists(key) {
		c.stats.Misses++
		return nil, ErrMiss
	}
	entry, err := c.readEntry(key)
	if err != nil {
		c.stats.Errors++
		c.index.remove(key)
		return nil, err
	}
	c.index.access(key)
	c.stats.Hits++
	return entry, nil
}

// BeginStreamingPut begins a streaming cache write.
func (c *Cache) BeginStreamingPut(key string, resp *http.Response) (*StreamingPut, error) {
	if c == nil || !c.enabled {
		return nil, ErrDisabled
	}
	headerData, err := json.Marshal(entryHeader{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), CachedAt: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	hash := cacheKey(key)
	file, err := os.CreateTemp(c.dir, hash+".tmp-*")
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(append(headerData, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return &StreamingPut{
		cache:      c,
		key:        key,
		path:       filepath.Join(c.dir, hash),
		tempPath:   file.Name(),
		metaPath:   filepath.Join(c.dir, hash+".meta"),
		file:       file,
		storedSize: int64(len(headerData) + 1),
		digest:     sha256.New(),
	}, nil
}

func (s *StreamingPut) Write(p []byte) (int, error) {
	if s == nil || s.finalized || s.file == nil {
		return 0, os.ErrClosed
	}
	n, err := s.file.Write(p)
	if n > 0 {
		_, _ = s.digest.Write(p[:n])
		s.bodySize += int64(n)
		s.storedSize += int64(n)
	}
	return n, err
}

func (s *StreamingPut) DigestHex() string {
	if s == nil {
		return ""
	}
	return hex.EncodeToString(s.digest.Sum(nil))
}

func (s *StreamingPut) Size() int64 {
	if s == nil {
		return 0
	}
	return s.bodySize
}

func (s *StreamingPut) Commit() error {
	if s == nil || s.finalized {
		return nil
	}
	s.finalized = true
	if err := s.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(s.tempPath, s.path); err != nil {
		return err
	}
	if err := os.WriteFile(s.metaPath, []byte(s.key), 0o600); err != nil {
		return err
	}
	s.cache.recordStore(s.key, s.storedSize)
	return nil
}

func (s *StreamingPut) Abort() error {
	if s == nil || s.finalized {
		return nil
	}
	s.finalized = true
	if s.file != nil {
		_ = s.file.Close()
	}
	return os.Remove(s.tempPath)
}

// RestoreResponse builds a response from cache entry.
func RestoreResponse(entry *Entry, req *http.Request) *http.Response {
	return &http.Response{
		StatusCode:    entry.StatusCode,
		Status:        fmt.Sprintf("%d %s", entry.StatusCode, http.StatusText(entry.StatusCode)),
		Header:        entry.Headers.Clone(),
		Body:          entry.Body,
		ContentLength: entry.Size,
		Request:       req,
	}
}

func (c *Cache) readEntry(key string) (*Entry, error) {
	file, err := os.Open(filepath.Join(c.dir, cacheKey(key)))
	if err != nil {
		return nil, ErrMiss
	}
	reader := bufio.NewReader(file)
	headerLine, err := reader.ReadBytes('\n')
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	var header entryHeader
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(headerLine))), &header); err != nil {
		_ = file.Close()
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	bodyOffset := int64(len(headerLine))
	if _, err := file.Seek(bodyOffset, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Entry{
		StatusCode: header.StatusCode,
		Headers:    header.Headers,
		Body:       file,
		Size:       info.Size() - bodyOffset,
		CachedAt:   header.CachedAt,
	}, nil
}

func (c *Cache) recordStore(key string, size int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index.add(key, size)
	c.stats.Stores++
	c.stats.CurrentSize += size
	for c.stats.CurrentSize > c.maxSize {
		evictKey, evictSize := c.index.evict()
		if evictKey == "" {
			break
		}
		_ = os.Remove(filepath.Join(c.dir, cacheKey(evictKey)))
		_ = os.Remove(filepath.Join(c.dir, cacheKey(evictKey)+".meta"))
		c.stats.CurrentSize -= evictSize
		c.stats.Evictions++
	}
}

func (c *Cache) loadIndex() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".meta") || strings.Contains(entry.Name(), ".tmp-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		meta, err := os.ReadFile(filepath.Join(c.dir, entry.Name()+".meta"))
		if err != nil {
			continue
		}
		key := string(meta)
		c.index.add(key, info.Size())
		c.stats.CurrentSize += info.Size()
	}
	return nil
}

func cacheKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type lruIndex struct {
	items map[string]*lruItem
	list  *list.List
}

type lruItem struct {
	key     string
	size    int64
	element *list.Element
}

func newLRUIndex() *lruIndex {
	return &lruIndex{items: map[string]*lruItem{}, list: list.New()}
}

func (i *lruIndex) add(key string, size int64) {
	if item, ok := i.items[key]; ok {
		item.size = size
		i.list.MoveToBack(item.element)
		return
	}
	item := &lruItem{key: key, size: size}
	item.element = i.list.PushBack(item)
	i.items[key] = item
}

func (i *lruIndex) access(key string) {
	if item, ok := i.items[key]; ok {
		i.list.MoveToBack(item.element)
	}
}

func (i *lruIndex) exists(key string) bool {
	_, ok := i.items[key]
	return ok
}

func (i *lruIndex) remove(key string) {
	if item, ok := i.items[key]; ok {
		i.list.Remove(item.element)
		delete(i.items, key)
	}
}

func (i *lruIndex) evict() (string, int64) {
	element := i.list.Front()
	if element == nil {
		return "", 0
	}
	item, ok := element.Value.(*lruItem)
	if !ok {
		i.list.Remove(element)
		return "", 0
	}
	i.list.Remove(element)
	delete(i.items, item.key)
	return item.key, item.size
}

// Matcher determines cacheable requests and responses.
type Matcher struct {
	patterns     []*regexp.Regexp
	contentAware bool
}

func NewMatcher(patterns []string, contentAware bool) (*Matcher, error) {
	m := &Matcher{contentAware: contentAware}
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		m.patterns = append(m.patterns, re)
	}
	return m, nil
}

func (m *Matcher) ShouldCache(req *http.Request) bool {
	if m == nil || req == nil || req.Method != http.MethodGet {
		return false
	}
	path := req.URL.Path
	if m.contentAware && strings.Contains(path, "sha256:") && hasDockerAccept(req) {
		return true
	}
	for _, pattern := range m.patterns {
		if pattern.MatchString(path) {
			return req.URL.RawQuery == "" || sha256DigestRe.FindString(path) != ""
		}
	}
	return false
}

func (m *Matcher) ShouldCacheResponse(resp *http.Response) bool {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") {
		return false
	}
	if m.contentAware {
		return isDockerResponse(resp)
	}
	return true
}

func (m *Matcher) GenerateKey(req *http.Request) string {
	return req.URL.Host + req.URL.Path
}

func (m *Matcher) VerifyDigestHex(path, actual string) error {
	matches := sha256DigestRe.FindStringSubmatch(path)
	if len(matches) == 0 {
		return nil
	}
	expected := strings.ToLower(matches[1])
	if expected == "" && len(matches) > 2 {
		expected = strings.ToLower(matches[2])
	}
	if expected != "" && expected != actual {
		return fmt.Errorf("sha256 mismatch: URL claims %s, body hashes to %s", expected, actual)
	}
	return nil
}

var sha256DigestRe = regexp.MustCompile(`sha256:([a-fA-F0-9]{64})|/([a-fA-F0-9]{64})/`)

func hasDockerAccept(req *http.Request) bool {
	accept := req.Header.Get("Accept")
	return strings.Contains(accept, "application/vnd.docker.") || strings.Contains(accept, "application/vnd.oci.")
}

func isDockerResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return resp.Header.Get("Docker-Content-Digest") != "" ||
		strings.Contains(ct, "application/vnd.docker.") ||
		strings.Contains(ct, "application/vnd.oci.") ||
		strings.HasPrefix(ct, "application/octet-stream")
}
