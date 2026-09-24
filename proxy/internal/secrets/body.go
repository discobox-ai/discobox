package secrets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// MaxCapturedBody is the most of a request body an authorizer can be handed.
// It is larger than any judge is shown (judge.MaxBodyBytes) because showing a
// body can need all of it first: a JSON document is parsed whole before it is
// written back, and a compressed one is decoded before any of it is text. It
// is small because the request is held while it is read, and whatever is read
// is held until the request is sent.
const MaxCapturedBody = 64 << 10

// RequestBody is a request's body as an authorizer may read it: once, up to
// MaxCapturedBody, and without changing what is sent (ADR 26-09-22-838 §6).
//
// Nothing is read unless Capture is called, because most requests are decided
// without their body and reading one holds the request until it arrives.
// Whatever Capture reads, Reader hands back in front of the rest, so the
// upstream — and the audit spool behind it — receive exactly the bytes the
// sandbox sent.
type RequestBody struct {
	source io.ReadCloser

	start sync.Once
	// done closes when the read Capture started has finished: at EOF, at the
	// cap, or on an error.
	done chan struct{}
	// data is what was read, which may be one byte past the cap: that byte is
	// how a body exactly MaxCapturedBody long is told from a longer one.
	data []byte
	eof  bool
	err  error
	// released is a body handed on before anybody captured it, which nothing
	// may read now: the bytes belong to whoever it was handed to.
	released bool
}

// NewRequestBody wraps body for an authorizer. A request with no body has
// nothing to capture, and gets nil.
func NewRequestBody(body io.ReadCloser) *RequestBody {
	if body == nil || body == http.NoBody {
		return nil
	}
	return &RequestBody{source: body, done: make(chan struct{})}
}

// Capture reads the start of the body, up to MaxCapturedBody, and reports
// whether that was all of it. It reads once: a later call waits for the same
// read and returns the same bytes.
//
// The read runs on its own and ctx bounds only the wait, because a body
// arrives at the pace the sandbox sends it and the proxy cannot stop a read
// part way without losing the bytes it was reading. A wait that gives up
// leaves the read running, bounded by the cap, and Reader still returns every
// byte in order.
func (b *RequestBody) Capture(ctx context.Context) (data []byte, complete bool, err error) {
	if b == nil {
		return nil, true, nil
	}
	b.start.Do(func() {
		go func() {
			defer close(b.done)
			b.data, b.err = io.ReadAll(io.LimitReader(b.source, MaxCapturedBody+1))
			b.eof = b.err == nil && len(b.data) <= MaxCapturedBody
		}()
	})
	select {
	case <-b.done:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	if b.released {
		return nil, false, errors.New("the body was sent on before it was read")
	}
	if b.err != nil {
		return nil, false, b.err
	}
	if len(b.data) > MaxCapturedBody {
		return b.data[:MaxCapturedBody], false, nil
	}
	return b.data, b.eof, nil
}

// Reader is the body to send: whatever Capture read, then the rest, then the
// error the read stopped on if it stopped on one. A body nobody captured is
// the one the request arrived with. Closing it closes that.
func (b *RequestBody) Reader() io.ReadCloser {
	if b == nil {
		return http.NoBody
	}
	started := true
	b.start.Do(func() { started = false })
	if !started {
		// Nothing was read, and nothing now will be: the once is spent.
		b.released = true
		close(b.done)
		return b.source
	}
	return &capturedReader{body: b}
}

// capturedReader waits for the capture to finish before its first read, so a
// transport that reads early cannot take bytes out from under it.
type capturedReader struct {
	body   *RequestBody
	once   sync.Once
	reader io.Reader
}

func (r *capturedReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		<-r.body.done
		rest := io.Reader(r.body.source)
		if r.body.err != nil {
			rest = errorReader{r.body.err}
		} else if r.body.eof {
			rest = http.NoBody
		}
		r.reader = io.MultiReader(bytes.NewReader(r.body.data), rest)
	})
	return r.reader.Read(p)
}

func (r *capturedReader) Close() error { return r.body.source.Close() }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
