package proxyagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/proxy"
)

// The gate: a sandbox's calls to the discobox API, which the proxy never sends
// to the internet and hands here instead (ADR 0140 §2).

// gateHTTPTimeout bounds one forwarded call. It is longer than a resolve's,
// because it is the sandbox's own call to the API rather than a lookup on the
// way to somewhere else.
const gateHTTPTimeout = 2 * time.Minute

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
	out.Header.Set("Authorization", "Bearer "+rc.Token)
	out.Header.Set(poolauth.ForwardingPoolHeader, rc.PoolID)
	out.Header.Set(poolauth.ForwardedSandboxHeader, req.ClientID)
	//nolint:bodyclose // Handed to the proxy, which writes it to the sandbox and closes it.
	resp, err := r.gateClient.Do(out)
	if err != nil {
		// Let in, and then the control plane did not answer: the request may
		// have reached it.
		return proxy.SecretGateAdmission{}, err
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
	if url := strings.TrimSpace(os.Getenv(envControlPlaneURL)); url != "" {
		if _, resolved, err := wire.HTTPClient(url, gateHTTPTimeout); err == nil {
			client = resolved
		}
	}
	return client
}
