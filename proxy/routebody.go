package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"sync"
)

// routeBodyLimit is the most of a request body the authorizer reads to match
// a route's protocol block or a `_method` a route's methods must allow (ADR
// 26-10-27-880 §4), before and after it is decoded. The audit spools a body
// whole, with no limit of its own to borrow, so this is the limit: a GraphQL
// document, a JSON-RPC call or a form is far smaller. A body past it answers
// false; what a route makes of that is secretroute's rule (its REVIEW.md,
// "unreadable, never unparsable").
const routeBodyLimit = 1 << 20

// routeBody is a request's body as a route reads it
// (secrets.AuthorizeRequest.Body). Nothing is read until it is called, which
// the authorizer does only when a candidate route has a protocol block, or
// has methods and the body may carry a `_method`, and then at most once.
//
// What it reads it puts back in front of what it did not, so the request is
// sent — and the retry buffer and the audit spool, which wrap req.Body after
// it, see — the body exactly as the sandbox sent it.
//
// It answers false, matching no open route's block, for a request that
// upgrades its connection, whose body is past routeBodyLimit (declared or
// read), whose length is undeclared, that fails to read, or that is in an
// encoding other than gzip, deflate or identity. A body of undeclared length
// is never read ahead of sending it: over HTTP/2 every body is a stream, and
// one whose sender waits on the response would never end (REVIEW.md).
func routeBody(req *http.Request) func() ([]byte, bool) {
	var (
		once sync.Once
		body []byte
		ok   bool
	)
	return func() ([]byte, bool) {
		once.Do(func() { body, ok = readRouteBody(req) })
		return body, ok
	}
}

func readRouteBody(req *http.Request) ([]byte, bool) {
	if headerContainsToken(req.Header, "Connection", "Upgrade") || req.Header.Get("Upgrade") != "" {
		return nil, false
	}
	if req.Body == nil || req.Body == http.NoBody {
		return decodeRouteBody(req.Header, nil)
	}
	if req.ContentLength < 0 || req.ContentLength > routeBodyLimit {
		return nil, false
	}
	original := req.Body
	read, err := io.ReadAll(io.LimitReader(original, routeBodyLimit+1))
	// Hand back exactly what the transport would have read: what came out
	// of the body so far, then the rest of it (or the error it stopped on).
	req.Body = joinedBody{Reader: io.MultiReader(bytes.NewReader(read), original), closer: original}
	if err != nil || len(read) > routeBodyLimit {
		return nil, false
	}
	return decodeRouteBody(req.Header, read)
}

// retryRouteBody is routeBody over a retry's buffered body, which the retry
// already holds whole.
func retryRouteBody(header http.Header, body []byte) func() ([]byte, bool) {
	return func() ([]byte, bool) {
		if headerContainsToken(header, "Connection", "Upgrade") || header.Get("Upgrade") != "" || len(body) > routeBodyLimit {
			return nil, false
		}
		return decodeRouteBody(header, body)
	}
}

// decodeRouteBody undoes the request's Content-Encoding: gzip, deflate
// (zlib-wrapped, as RFC 9110 names it, or raw, as some clients send it) or
// none. Any other coding, or more than one, is a body the route cannot read.
func decodeRouteBody(header http.Header, body []byte) ([]byte, bool) {
	codings := header.Values("Content-Encoding")
	if len(codings) > 1 {
		return nil, false
	}
	coding := ""
	if len(codings) == 1 {
		coding = strings.ToLower(strings.TrimSpace(codings[0]))
	}
	switch coding {
	case "", "identity":
		return body, true
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false
		}
		return readDecoded(reader)
	case "deflate":
		if reader, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
			if decoded, ok := readDecoded(reader); ok {
				return decoded, true
			}
		}
		return readDecoded(flate.NewReader(bytes.NewReader(body)))
	default:
		return nil, false
	}
}

// readDecoded reads a decoder to its end, refusing more than routeBodyLimit
// of what it decodes to, so a small body cannot expand into a large one.
func readDecoded(reader io.ReadCloser) ([]byte, bool) {
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(io.LimitReader(reader, routeBodyLimit+1))
	if err != nil || len(decoded) > routeBodyLimit {
		return nil, false
	}
	return decoded, true
}
