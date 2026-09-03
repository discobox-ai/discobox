package guestimage

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
)

// countedImage wraps an image so the compressed bytes its layers fetch are
// counted, and starts a goroutine reporting the running totals until stop is
// called.
//
// Counting happens at Compressed() rather than at the flattened tar, because
// the flattened stream is uncompressed and its size is nowhere in the manifest:
// there would be a byte counter with no denominator. partial.CompressedToLayer
// then rebuilds Uncompressed() on top of the counted stream — it sniffs the
// layer's compression itself, so this stays correct for a gzip, zstd, or
// already-uncompressed layer without this package knowing which it got.
//
// go-containerregistry's own remote.WithProgress is not this: it is wired into
// pushes only, and reports nothing for a pull.
func countedImage(image v1.Image, reference string, report ProgressFunc) (v1.Image, func(), error) {
	layers, err := image.Layers()
	if err != nil {
		return nil, nil, err
	}
	counter := &fetchCounter{reference: reference, report: report, layers: len(layers)}
	for _, layer := range layers {
		size, err := layer.Size()
		if err != nil {
			return nil, nil, err
		}
		counter.total += size
	}

	counted := make([]v1.Layer, 0, len(layers))
	for _, layer := range layers {
		wrapped, err := partial.CompressedToLayer(&countedLayer{Layer: layer, counter: counter})
		if err != nil {
			return nil, nil, err
		}
		counted = append(counted, wrapped)
	}
	// Once before the first byte, so a status line names the fetch and its size
	// while the first layer's request is still in flight.
	counter.emit(false)
	return &countedImageLayers{Image: image, layers: counted}, counter.start(), nil
}

// countedImageLayers is the image with its layers replaced. Only Layers() is
// overridden: mutate.Extract reads the image through it, and everything else —
// config, manifest, digest — is the original's to answer.
type countedImageLayers struct {
	v1.Image
	layers []v1.Layer
}

func (i *countedImageLayers) Layers() ([]v1.Layer, error) { return i.layers, nil }

// countedLayer counts what its compressed stream yields. It satisfies
// partial.CompressedLayer through the embedded layer, and partial.WithDiffID
// too, so rebuilding the layer costs no extra fetch to compute a DiffID.
type countedLayer struct {
	v1.Layer
	counter *fetchCounter
}

func (l *countedLayer) Compressed() (io.ReadCloser, error) {
	stream, err := l.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	return &countedReader{ReadCloser: stream, counter: l.counter}, nil
}

// countedReader adds what it reads to the counter, and counts its layer
// complete when the stream ends. Close is not that signal: a layer's stream is
// closed on the way out of a failed extraction too.
type countedReader struct {
	io.ReadCloser
	counter *fetchCounter
	ended   bool
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.counter.add(int64(n))
	}
	if err != nil && !r.ended {
		r.ended = true
		if err == io.EOF {
			r.counter.layerDone()
		}
	}
	return n, err
}

// fetchCounter is the running total behind one fetch's reports.
type fetchCounter struct {
	reference string
	report    ProgressFunc
	total     int64
	layers    int

	current       atomic.Int64
	layersDone    atomic.Int64
	stopOnce      sync.Once
	stopReporting chan struct{}
}

func (c *fetchCounter) add(n int64) { c.current.Add(n) }
func (c *fetchCounter) layerDone()  { c.layersDone.Add(1) }

// start begins the ticker and returns the stop func, which emits the closing
// report. Reporting on a ticker rather than per read is what keeps a stalled
// download restating its phase, and what keeps a fast one from calling the
// reporter once per 32 KiB.
func (c *fetchCounter) start() func() {
	c.stopReporting = make(chan struct{})
	go func() {
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.stopReporting:
				return
			case <-ticker.C:
				c.emit(false)
			}
		}
	}()
	return func() {
		c.stopOnce.Do(func() {
			close(c.stopReporting)
			c.emit(true)
		})
	}
}

func (c *fetchCounter) emit(done bool) {
	c.report(Progress{
		Reference:      c.reference,
		Current:        c.current.Load(),
		Total:          c.total,
		Layers:         c.layers,
		LayersComplete: int(c.layersDone.Load()),
		Done:           done,
	})
}
