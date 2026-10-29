package proxy

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"testing"
)

func deflated(t *testing.T, text string, wrapped bool) []byte {
	t.Helper()
	var out bytes.Buffer
	var writer io.WriteCloser
	if wrapped {
		writer = zlib.NewWriter(&out)
	} else {
		var err error
		if writer, err = flate.NewWriter(&out, flate.DefaultCompression); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(writer, text); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// countingBody counts what is read from it, so a test can tell whether
// anything was read ahead.
type countingBody struct {
	io.Reader
	read int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err
}

func (b *countingBody) Close() error { return nil }

// The body a route reads is decoded, read only when asked for and once, and
// put back so the request still sends all of it. One the route cannot read
// matches no protocol block.
func TestRouteBody(t *testing.T) {
	const text = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_issue"}}`
	for _, tc := range []struct {
		name     string
		body     []byte
		length   int64
		header   http.Header
		want     string
		readable bool
	}{
		{name: "identity", body: []byte(text), want: text, readable: true},
		{name: "named identity", body: []byte(text), header: http.Header{"Content-Encoding": {"identity"}}, want: text, readable: true},
		{name: "zlib deflate", body: deflated(t, text, true), header: http.Header{"Content-Encoding": {"deflate"}}, want: text, readable: true},
		{name: "raw deflate", body: deflated(t, text, false), header: http.Header{"Content-Encoding": {"deflate"}}, want: text, readable: true},
		{name: "gzip that is not", body: []byte(text), header: http.Header{"Content-Encoding": {"gzip"}}},
		{name: "two codings", body: []byte(text), header: http.Header{"Content-Encoding": {"gzip", "gzip"}}},
		{name: "another coding", body: []byte(text), header: http.Header{"Content-Encoding": {"zstd"}}},
		{name: "an upgrade", body: []byte(text), header: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}},
		{name: "undeclared length", body: []byte(text), length: -1},
		{name: "declared past the limit", body: []byte(text), length: routeBodyLimit + 1},
		{name: "past the limit", body: []byte(strings.Repeat("x", routeBodyLimit+1)), length: routeBodyLimit},
	} {
		source := &countingBody{Reader: bytes.NewReader(tc.body)}
		req := &http.Request{Method: http.MethodPost, Header: tc.header, Body: source, ContentLength: int64(len(tc.body))}
		if req.Header == nil {
			req.Header = http.Header{}
		}
		if tc.length != 0 {
			req.ContentLength = tc.length
		}
		read := routeBody(req)
		if source.read != 0 {
			t.Fatalf("%s: the body was read before the route asked for it", tc.name)
		}
		got, ok := read()
		if ok != tc.readable || (ok && string(got) != tc.want) {
			t.Fatalf("%s: body = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.readable)
		}
		if tc.length < 0 && source.read != 0 {
			t.Fatalf("%s: a body of undeclared length was read ahead of sending it", tc.name)
		}
		readAgain := source.read
		if again, okAgain := read(); okAgain != ok || !bytes.Equal(again, got) || source.read != readAgain {
			t.Fatalf("%s: a second call read the body again", tc.name)
		}
		sent, err := io.ReadAll(req.Body)
		if err != nil || !bytes.Equal(sent, tc.body) {
			t.Fatalf("%s: the request would send %d bytes (%v), want the %d it arrived with", tc.name, len(sent), err, len(tc.body))
		}
	}
}

// A gzip body that decodes to more than the limit is not read past it.
func TestRouteBodyRefusesADecodedBodyPastTheLimit(t *testing.T) {
	body := gzipped(t, strings.Repeat("x", routeBodyLimit+1))
	req := &http.Request{Header: http.Header{"Content-Encoding": {"gzip"}}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
	if _, ok := routeBody(req)(); ok {
		t.Fatal("a body decoding past the limit was readable")
	}
}

// A request without a body has an empty one to read.
func TestRouteBodyOfNoBody(t *testing.T) {
	got, ok := routeBody(&http.Request{Method: http.MethodGet, Header: http.Header{}, Body: http.NoBody})()
	if !ok || len(got) != 0 {
		t.Fatalf("body = %q, %v; want empty and readable", got, ok)
	}
}
