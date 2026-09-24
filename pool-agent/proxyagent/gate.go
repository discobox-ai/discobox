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

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/pool-agent/wire"
	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/wellknown"
)

// The gate: a sandbox's calls to the discobox API, which the proxy never sends
// to the internet and hands here instead (ADR 0140 §2).

// gateHTTPTimeout bounds one forwarded call. It is longer than a resolve's,
// because it is the sandbox's own call to the API rather than a lookup on the
// way to somewhere else.
const gateHTTPTimeout = 2 * time.Minute

// GateHost is the host a sandbox reaches the discobox API at. It is the
// registry's, so the proxy, the credential, and the ask for it cannot disagree.
func GateHost() string {
	known, _ := wellknown.Lookup(wellknown.DiscoboxSandbox)
	return known.Host()
}

// gateForwardedHeaders are the headers a sandbox's call does not carry on to
// the control plane: its own credential and hop-by-hop headers, and any claim
// of its own about who is calling, which is the pool's to make.
var gateForwardedHeaders = []string{
	"Authorization", "Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
	poolauth.ForwardedSandboxHeader, poolauth.ForwardingPoolHeader,
}

// Gate admits a sandbox's call to the discobox API and answers it from the
// control plane. It admits only a call carrying the sentinel of a live use of
// ai.discobox.sandbox, activated for this sandbox for this host, that the judge
// allows. The sentinel is removed, not swapped: nothing stands behind it. The
// call goes on with this pool's assertion, which is what the control plane
// takes as the sandbox's identity (ADR 0140 §3).
func (r *secretResolver) Gate(ctx context.Context, req proxy.SecretGateRequest) (proxy.SecretGateAdmission, error) {
	in := req.Request
	rc, err := readResolveContext(r.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		return proxy.SecretGateAdmission{}, &proxy.SecretGateRefusal{Reason: "this pool cannot reach the control plane yet"}
	}
	sentinel := strings.TrimSpace(strings.TrimPrefix(in.Header.Get("Authorization"), "Bearer "))
	record, ok := r.gateActivation(req.ClientID, sentinel)
	if !ok {
		return proxy.SecretGateAdmission{}, &proxy.SecretGateRefusal{Reason: fmt.Sprintf(
			"the call carries no live use of %s; run it under one: discobox-access run --use <id> -- discobox …", wellknown.DiscoboxSandbox)}
	}
	verdict, err := r.Authorize(ctx, proxy.SecretAuthorizeRequest{
		ClientID:  req.ClientID,
		Sentinels: []string{sentinel},
		Method:    in.Method,
		// The host without its port, which is what the contract says this
		// field is and what the proxy's own caller states. The gate reaches
		// Authorize directly rather than through the swapper, so the one
		// normalization there is this one.
		Host:   hostscope.Normalize(in.Host),
		URL:    in.URL.String(),
		Header: in.Header,
	})
	if err != nil {
		return proxy.SecretGateAdmission{}, &proxy.SecretGateRefusal{Reason: "the judge could not decide: " + err.Error(), UseID: record.UseID}
	}
	if !verdict.Allow {
		// A verdict that refuses and says nothing still owes the caller a
		// sentence: a refusal nobody can read is indistinguishable from a
		// broken credential (ADR 0148 §1).
		reason := verdict.Reason
		if reason == "" {
			reason = "not an approved use of this credential"
		}
		return proxy.SecretGateAdmission{}, &proxy.SecretGateRefusal{Reason: reason, UseID: record.UseID}
	}

	target, err := gateTarget(rc.ControlPlaneURL, in.URL)
	if err != nil {
		return proxy.SecretGateAdmission{}, &proxy.SecretGateRefusal{Reason: err.Error(), UseID: record.UseID}
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
		// have reached it, so the use it went under stays on the record.
		return proxy.SecretGateAdmission{UseID: record.UseID}, err
	}
	return proxy.SecretGateAdmission{Response: resp, UseID: record.UseID}, nil
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

// gateActivation is the live activation a gate call carries: one this process
// minted for this sandbox, for exactly the gate host. Only an ask by the
// discobox credential's ID can produce one: the control plane refuses any
// other ask for that host.
func (r *secretResolver) gateActivation(sandboxID, sentinel string) (activation, bool) {
	if r.activations == nil || sentinel == "" {
		return activation{}, false
	}
	record, ok := r.activations.lookup(sentinel)
	if !ok || record.SandboxID != sandboxID || !strings.EqualFold(record.Host, GateHost()) {
		return activation{}, false
	}
	return record, true
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
