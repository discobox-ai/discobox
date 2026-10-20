package targets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/discobox-ai/discobox/server/internal/publicnet"
	"github.com/discobox-ai/discobox/server/internal/targetdb"
)

// fetchTimeout bounds one download. GitHub's REST description is 13.5 MB.
const fetchTimeout = 5 * time.Minute

// dialTimeout bounds connecting to a document's host.
const dialTimeout = 30 * time.Second

// errNoCache is a document to fetch on a server that has nowhere to keep it.
var errNoCache = errors.New("this server keeps no target documents")

// documentCache holds fetched documents in a directory, each in a file named
// by its digest, so a document is downloaded once per server and two entries
// naming the same text share it. What it answers is always the text its
// digest names: a download that does not match is discarded, and a file
// already there is checked the first time this process opens it.
type documentCache struct {
	dir    string
	client *http.Client

	fetches singleflight.Group
	mu      sync.Mutex
	checked map[string]bool
}

// newDocumentCache keeps documents in dir, fetching them with client — nil
// for the default transport. Whatever client is given, a fetch follows no
// redirect and reaches only a public address (publicnet): a document's URL is
// a person's to write, and the server reaching where they cannot is a reach
// they never had.
func newDocumentCache(dir string, client *http.Client) *documentCache {
	fetching := http.Client{Timeout: fetchTimeout}
	if client != nil {
		fetching.Transport = client.Transport
	}
	fetching.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &documentCache{dir: dir, client: &fetching, checked: map[string]bool{}}
}

// open returns the text of d, which has a URL, fetching it first if the cache
// lacks it.
func (c *documentCache) open(ctx context.Context, d targetdb.Document) (*os.File, error) {
	if c.dir == "" {
		return nil, errNoCache
	}
	path := filepath.Join(c.dir, d.SHA256)
	if f, ok := c.openChecked(path, d.SHA256); ok {
		return f, nil
	}
	// One download per digest however many ask at once. It is not the
	// asker's to cancel: the others are waiting on it too.
	_, err, _ := c.fetches.Do(d.SHA256, func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		return nil, c.fetch(fetchCtx, d, path)
	})
	if err != nil {
		return nil, err
	}
	if f, ok := c.openChecked(path, d.SHA256); ok {
		return f, nil
	}
	return nil, fmt.Errorf("document %s: fetched, but not in the cache", d.Name)
}

// openChecked opens the cached file at path, if it is there and holds the
// text sum names. A file that does not is removed.
func (c *documentCache) openChecked(path, sum string) (*os.File, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	c.mu.Lock()
	checked := c.checked[sum]
	c.mu.Unlock()
	if checked {
		return f, true
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || hex.EncodeToString(h.Sum(nil)) != sum {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, false
	}
	c.mu.Lock()
	c.checked[sum] = true
	c.mu.Unlock()
	return f, true
}

// fetch downloads d into path, keeping it only when its digest is d's.
func (c *documentCache) fetch(ctx context.Context, d targetdb.Document, path string) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return fmt.Errorf("document %s: %w", d.Name, err)
	}
	if err := publicnet.RefuseHost(ctx, req.URL.Hostname()); err != nil {
		return fmt.Errorf("document %s: %w: a document is only fetched from a public address", d.Name, err)
	}
	resp, err := publicnet.Client(c.client, req.URL, dialTimeout).Do(req)
	if err != nil {
		return fmt.Errorf("document %s: fetch %s: %w", d.Name, d.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A redirect is not followed: where it points is a second URL nobody
		// checked.
		return fmt.Errorf("document %s: fetch %s: %s", d.Name, d.URL, resp.Status)
	}

	tmp, err := os.CreateTemp(c.dir, ".fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, targetdb.MaxDocumentBytes+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("document %s: fetch %s: %w", d.Name, d.URL, err)
	}
	if n > targetdb.MaxDocumentBytes {
		return fmt.Errorf("document %s: %s answered more than %d bytes", d.Name, d.URL, targetdb.MaxDocumentBytes)
	}
	if hex.EncodeToString(h.Sum(nil)) != d.SHA256 {
		return fmt.Errorf("document %s: %s did not answer the text of digest %s", d.Name, d.URL, d.SHA256)
	}
	return os.Rename(tmp.Name(), path)
}
