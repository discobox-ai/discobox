package proxyagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/proxy"
)

// The gate: a sandbox's calls to the discobox API, which the proxy never sends
// to the internet and hands here instead (ADR 0140 §2).

// gateHTTPTimeout bounds one forwarded call, its body included. It is longer
// than a resolve's, because it is the sandbox's own call to the API rather
// than a lookup on the way to somewhere else. A sandbox reading the event
// stream through here is cut off at it, so the discobox-box monitor holds the
// stream for less (its maxHold, sandbox-agent/monitors/discoboxbox): lowering
// this below that cuts every run short.
const gateHTTPTimeout = 2 * time.Minute

// Only connection establishment is retried: the request body has not been
// sent yet. Keep this budget inside the forwarded call's overall timeout.
const gateConnectWait = 10 * time.Second
const gateConnectRetry = 100 * time.Millisecond

// GateHost is the host a sandbox reaches the discobox API at.
const GateHost = "api.discobox.internal"

// gateForwardedHeaders are the headers a sandbox's call does not carry on to
// the control plane: its own credential and hop-by-hop headers, and any claim
// of its own about who is calling, which is the pool's to make.
var gateForwardedHeaders = []string{
	"Authorization", "Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	poolauth.ForwardedSandboxHeader, poolauth.ForwardingPoolHeader,
}

// Gate answers a sandbox's call to the discobox API from the control plane.
// The pool forwards every call, and the control plane refuses one from a
// sandbox without DiscoboxAPI (ADR 26-10-27-880 §9, as amended): a sandbox
// without the flag reaches the control plane under this pool's
// ScopeSandboxForward assertion naming it (ADR 0140 §3) and is refused there
// by the sandbox role, so the pool is never sent the flag. Whatever credential
// the sandbox sent is removed, never forwarded.
func (r *secretResolver) Gate(ctx context.Context, req proxy.SecretGateRequest) (proxy.SecretGateAdmission, error) {
	in := req.Request
	rc, err := readResolveContext(r.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		return proxy.SecretGateAdmission{}, &proxy.SecretGateRefusal{Reason: "this pool cannot reach the control plane yet"}
	}
	target, err := gateTarget(rc.ControlPlaneURL, in.URL)
	if err != nil {
		return proxy.SecretGateAdmission{}, &proxy.SecretGateRefusal{Reason: err.Error()}
	}
	out, err := http.NewRequestWithContext(ctx, in.Method, target, in.Body)
	if err != nil {
		return proxy.SecretGateAdmission{}, err
	}
	out.Header = in.Header.Clone()
	for _, name := range gateForwardedHeaders {
		out.Header.Del(name)
	}
	out.ContentLength = in.ContentLength
	stream := gateStream(in)
	client := r.gateClient
	var lifetime *gateLifetime
	if stream {
		copyClient := *client
		copyClient.Timeout = 0
		client = &copyClient
		streamCtx, cancel := context.WithCancel(ctx)
		lifetime = &gateLifetime{cancel: cancel}
		lifetime.timer = time.AfterFunc(gateHTTPTimeout, cancel)
		out = out.WithContext(streamCtx)
		if out.Body != nil {
			out.Body = &gateBody{ReadCloser: out.Body, lifetime: lifetime}
		}
		if strings.EqualFold(in.Header.Get("Upgrade"), "websocket") {
			out.Header.Set("Connection", "Upgrade")
			out.Header.Set("Upgrade", "websocket")
		}
	}

	out.Header.Set("Authorization", "Bearer "+rc.Token)
	out.Header.Set(poolauth.ForwardingPoolHeader, rc.PoolID)
	out.Header.Set(poolauth.ForwardedSandboxHeader, req.ClientID)
	//nolint:bodyclose // Handed to the proxy, which writes it to the sandbox and closes it.
	resp, err := client.Do(out)
	if err != nil {
		// Let in, and then the control plane did not answer: the request may
		// have reached it.
		if lifetime != nil {
			lifetime.close()
		}
		return proxy.SecretGateAdmission{}, err
	}
	if lifetime != nil {
		lifetime.touch()
		if rw, ok := resp.Body.(io.ReadWriteCloser); ok && resp.StatusCode == http.StatusSwitchingProtocols {
			resp.Body = &gateDuplex{rw, lifetime}
		} else {
			resp.Body = &gateResponseBody{gateBody{resp.Body, lifetime}}
		}
	}
	return proxy.SecretGateAdmission{Response: resp}, nil
}

// gateTarget is where a gate call goes on the control plane: its base URL, with
// the request's path and query and nothing else of what the sandbox sent. The
// request carries this pool's credential from here, so the sandbox must not be
// able to choose the host it goes to — only a path on the control plane. A
// request-target that is not a path (an opaque URL, one with userinfo) is
// refused rather than read, since it can only be an attempt to point it
// somewhere else.
func gateTarget(controlPlaneURL string, in *url.URL) (string, error) {
	if in == nil || in.Opaque != "" || in.User != nil || !strings.HasPrefix(in.Path, "/") {
		return "", errors.New("the call names no path on the discobox API")
	}
	base := controlPlaneURL
	if endpoint, err := wire.Parse(base); err == nil {
		base = endpoint.BaseURL()
	}
	target, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("control-plane URL: %w", err)
	}
	target.Path = strings.TrimSuffix(target.Path, "/") + in.Path
	target.RawPath = ""
	target.RawQuery = in.RawQuery
	target.Fragment = ""
	return target.String(), nil
}

// gateHTTPClient reaches the control plane the way the resolver does, with a
// forwarded call's longer timeout.
func gateHTTPClient() *http.Client {
	client := &http.Client{Timeout: gateHTTPTimeout}
	endpoint, err := wire.Parse(strings.TrimSpace(os.Getenv(envControlPlaneURL)))
	if err != nil {
		return client
	}
	dial, err := endpoint.DialContext()
	if err != nil {
		return client
	}
	client.Transport = &http.Transport{DialContext: gateDialContext(dial)}
	return client
}

// gateDialContext tolerates a listener being unlinked or not accepting yet
// during control-plane replacement. It never retries a transmitted request or
// other dial failures (permissions, name resolution, TLS, and so on).
func gateDialContext(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, gateConnectWait)
		defer cancel()
		for {
			conn, err := dial(ctx, network, address)
			if err == nil {
				return conn, nil
			}
			if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
				return nil, err
			}
			timer := time.NewTimer(gateConnectRetry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// Only the files transfer and exec attach routes receive a progress-based
// lifetime. All other API calls keep the ordinary bounded request timeout.
func gateStream(r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) > 0 && parts[0] == "api" {
		parts = parts[1:]
	}
	if len(parts) != 6 && len(parts) != 7 {
		return false
	}
	if parts[0] != "projects" || parts[2] != "sandboxes" {
		return false
	}
	if len(parts) == 6 && parts[4] == "files" && (parts[5] == "content" || parts[5] == "archive") {
		return r.Method == http.MethodGet || r.Method == http.MethodPut
	}
	return len(parts) == 7 && parts[4] == "execs" && parts[6] == "attach" && (r.Method == http.MethodGet || r.Method == http.MethodPost)
}

type gateLifetime struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel context.CancelFunc
	closed bool
}

func (l *gateLifetime) touch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.timer.Reset(gateHTTPTimeout)
	}
}
func (l *gateLifetime) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.timer.Stop()
		l.cancel()
	}
}

type gateBody struct {
	io.ReadCloser
	lifetime *gateLifetime
}

func (b *gateBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if n > 0 {
		b.lifetime.touch()
	}
	return n, e
}

// Closing the upload is not closing the response lifetime: net/http closes
// request bodies as soon as sending finishes, before the response arrives.
func (b *gateBody) Close() error { return b.ReadCloser.Close() }

type gateDuplex struct {
	io.ReadWriteCloser
	lifetime *gateLifetime
}

func (b *gateDuplex) Read(p []byte) (int, error) {
	n, e := b.ReadWriteCloser.Read(p)
	if n > 0 {
		b.lifetime.touch()
	}
	return n, e
}
func (b *gateDuplex) Write(p []byte) (int, error) {
	n, e := b.ReadWriteCloser.Write(p)
	if n > 0 {
		b.lifetime.touch()
	}
	return n, e
}
func (b *gateDuplex) Close() error { b.lifetime.close(); return b.ReadWriteCloser.Close() }

type gateResponseBody struct{ gateBody }

func (b *gateResponseBody) Close() error { b.lifetime.close(); return b.ReadCloser.Close() }
