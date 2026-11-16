package server

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"syscall"
	"time"
)

// sandboxPoolOutageWindow is how long an attach waits out a pool it cannot
// reach, counted from its first try. A hop between the server and a remote
// pool (boxd's edge) can cut every connection to the pool at once and come
// back within seconds (#1218); an attach made in that gap is retried rather
// than failed. It is far inside the attach's own wait for its sandbox (ADR
// 0039 tier 1), and the caller's context cuts it short.
//
// A var so a test can run it out without waiting half a minute.
var sandboxPoolOutageWindow = 30 * time.Second

// sandboxPoolOutageRetryInterval is the wait between tries.
var sandboxPoolOutageRetryInterval = time.Second

// poolOutageRetry is the transport of an exec attach's proxy: it sends a
// request with no body again while the pool's path answers that it is not
// there (poolOutage), for up to sandboxPoolOutageWindow; a try started
// within it runs to its own end. A request with a body goes once: its body is
// the client's stream, and is read by the first try.
type poolOutageRetry struct {
	base http.RoundTripper
}

func (t poolOutageRetry) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.Body != http.NoBody {
		return t.base.RoundTrip(req)
	}
	ctx := req.Context()
	deadline := time.Now().Add(sandboxPoolOutageWindow)
	waited := false
	for {
		resp, err := t.base.RoundTrip(req)
		if !poolOutage(resp, err) || ctx.Err() != nil || !time.Now().Add(sandboxPoolOutageRetryInterval).Before(deadline) {
			if waited && !poolOutage(resp, err) {
				slog.InfoContext(ctx, "reached the pool again; attaching", "path", req.URL.Path)
			}
			return resp, err
		}
		if !waited {
			slog.InfoContext(ctx, "the pool could not be reached; retrying the attach", "path", req.URL.Path, "window", sandboxPoolOutageWindow, "error", outageCause(resp, err))
			waited = true
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sandboxPoolOutageRetryInterval):
		}
	}
}

// poolOutage reports whether a try did not reach the pool: a connection that
// could not be made or was cut, or a front's answer that what is behind it is
// not there. An answer in the API's JSON error shape is the pool agent's or
// the sandbox agent's own (the pool agent cannot reach the sandbox, an attach
// that failed), and is handed on at once; so is an error that is no
// connection's, such as a token that could not be minted.
func poolOutage(resp *http.Response, err error) bool {
	if err != nil {
		return connectionFailed(err)
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return false
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return media != "application/json"
}

// connectionFailed reports whether err is a connection to the pool that
// failed: one that could not be dialed, was refused or reset, or ended
// before an answer.
func connectionFailed(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED)
}

func outageCause(resp *http.Response, err error) string {
	if err != nil {
		return err.Error()
	}
	return resp.Status
}
