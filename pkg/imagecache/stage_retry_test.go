package imagecache

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/pkg/imagecache/imagecachetest"
)

// While a layer whose transfer ended partway is fetched again, the reports say
// so, and count only the bytes on disk, never more than the total, whether the
// registry resumes it or sends all of it again.
func TestStageReportsARetryAndNeverCountsPastWhatIsOnDisk(t *testing.T) {
	for _, test := range []struct {
		name string
		// ignoresRange is a registry that answers a ranged request with the
		// whole blob, as a 200.
		ignoresRange bool
	}{
		{name: "resumed"},
		{name: "sent again whole", ignoresRange: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := imagecachetest.NewRegistry(t)
			layer := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
			image := registry.Publish("x/agent", "v1", layer)
			digest := image.Layers["amd64"][0]
			const cutAt = 300 << 10

			client := registry.Client()
			cutter := &cuttingTransport{base: client.Transport, digest: digest, cutAt: cutAt}
			client.Transport = cutter
			if test.ignoresRange {
				client.Transport = rangeStripper{base: cutter}
			}
			var mu sync.Mutex
			var reports []Progress
			opts := Options{Platform: amd64, Client: client, OnProgress: func(p Progress) {
				mu.Lock()
				defer mu.Unlock()
				reports = append(reports, p)
			}}
			if _, err := Open(t.TempDir()).Stage(context.Background(), []string{image.Reference}, opts); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			defer mu.Unlock()
			var retried *Retry
			retriedShort := false
			var last Progress
			for _, p := range reports {
				if p.Done {
					continue
				}
				if p.Current > p.Total {
					t.Fatalf("a report counted %d bytes of %d", p.Current, p.Total)
				}
				if p.Retry != nil {
					retried = p.Retry
					// Reports land on a ticker, and the retry waits a second
					// before asking again, so some report sees it while the
					// layer is still short. (One between the layer's last byte
					// and its rename may be at the total: that is on disk.)
					retriedShort = retriedShort || p.Current < p.Total
				}
				last = p
			}
			// The config is the first blob, so the shared layer is the second.
			if retried == nil || retried.Layer != 2 || retried.Attempt != 2 || retried.Kept != cutAt || retried.Err == nil {
				t.Fatalf("retry reported = %+v, want layer 2, attempt 2, %d bytes kept, and why", retried, cutAt)
			}
			if !retriedShort {
				t.Fatal("no report named the retry while the layer was still short")
			}
			if last.Retry != nil || last.Current != last.Total || last.LayersComplete != last.Layers {
				t.Fatalf("last report = %+v, want every byte and layer in, and no retry", last)
			}
		})
	}
}

// rangeStripper drops a request's Range, which is what a registry that does not
// serve ranges does with it.
type rangeStripper struct{ base http.RoundTripper }

func (s rangeStripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Range") != "" {
		req = req.Clone(req.Context())
		req.Header.Del("Range")
	}
	return s.base.RoundTrip(req)
}

// A 206 must start where it was asked to; anything else is not the rest of
// this blob.
func TestContentRangeStart(t *testing.T) {
	for value, want := range map[string]int64{
		"bytes 300-1023/1024": 300,
		"bytes 0-9/*":         0,
		"bytes */1024":        -1,
		"300-1023/1024":       -1,
		"bytes -5-9/10":       -1,
		"":                    -1,
	} {
		got, ok := contentRangeStart(value)
		if !ok {
			got = -1
		}
		if got != want {
			t.Errorf("contentRangeStart(%q) = %d, want %d", value, got, want)
		}
	}
}
