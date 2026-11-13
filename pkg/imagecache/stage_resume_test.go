package imagecache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/pkg/imagecache/imagecachetest"
)

// A layer whose transfer ends partway is fetched again from the bytes already
// received, with a Range from there, so the image completes having fetched
// each byte once.
func TestStageResumesALayerWhoseTransferEndedPartway(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	layer := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	image := registry.Publish("x/agent", "v1", layer)
	digest := image.Layers["amd64"][0]
	const cutAt = 300 << 10

	client := registry.Client()
	cutter := &cuttingTransport{base: client.Transport, digest: digest, cutAt: cutAt}
	client.Transport = cutter

	if _, err := Open(t.TempDir()).Stage(context.Background(), []string{image.Reference}, Options{Platform: amd64, Client: client}); err != nil {
		t.Fatal(err)
	}
	ranges, sent := cutter.result()
	if len(ranges) != 2 {
		t.Fatalf("the layer was requested %d times, want 2 (the cut transfer and its resumption)", len(ranges))
	}
	if want := fmt.Sprintf("bytes=%d-", cutAt); ranges[1] != want {
		t.Fatalf("the retry asked for Range %q, want %q", ranges[1], want)
	}
	if sent != int64(len(layer)) {
		t.Fatalf("the registry sent %d bytes of a %d-byte layer, want each byte once", sent, len(layer))
	}
}

// cuttingTransport ends the first transfer of one blob after cutAt bytes, as a
// connection that drops does, and answers a ranged request for it with the
// rest as a 206, as ghcr.io's blob storage does. It records each request's
// Range and how many of the blob's bytes it let through.
type cuttingTransport struct {
	base   http.RoundTripper
	digest string
	cutAt  int64

	mu     sync.Mutex
	ranges []string
	sent   int64
}

func (c *cuttingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasSuffix(req.URL.Path, "/blobs/"+c.digest) {
		return resp, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	requested := req.Header.Get("Range")
	c.ranges = append(c.ranges, requested)
	var body io.Reader = bytes.NewReader(data)
	switch {
	case len(c.ranges) == 1:
		body = io.MultiReader(bytes.NewReader(data[:c.cutAt]), errReader{io.ErrUnexpectedEOF})
		c.sent += c.cutAt
	case strings.HasPrefix(requested, "bytes=") && strings.HasSuffix(requested, "-"):
		from, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(requested, "bytes="), "-"), 10, 64)
		if err != nil || from >= int64(len(data)) {
			return nil, fmt.Errorf("bad range %q", requested)
		}
		resp.StatusCode, resp.Status = http.StatusPartialContent, "206 Partial Content"
		resp.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(data)-1, len(data)))
		body = bytes.NewReader(data[from:])
		c.sent += int64(len(data)) - from
	default:
		c.sent += int64(len(data))
	}
	resp.Body = io.NopCloser(body)
	resp.ContentLength = -1
	return resp, nil
}

func (c *cuttingTransport) result() ([]string, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ranges...), c.sent
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
