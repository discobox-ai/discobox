package proxyagent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/proxy"
)

// gateCall is what the fake control plane saw of one forwarded call.
type gateCall struct {
	method, uri, auth, pool, sandbox, body string
}

// A sandbox's call to the discobox API goes on to the control plane with its
// own credential removed and the pool's word for who is calling put in its
// place (ADR 0140 §§2–3). Whether the sandbox may call the API is the control
// plane's to decide (ADR 26-10-27-880 §9), so the gate forwards every call.
func TestTheGateForwardsEveryCallAsThePoolsWord(t *testing.T) {
	root := withTestRoot(t)
	var seen []gateCall
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, gateCall{
			method: r.Method, uri: r.URL.RequestURI(), auth: r.Header.Get("Authorization"),
			pool: r.Header.Get(poolauth.ForwardingPoolHeader), sandbox: r.Header.Get(poolauth.ForwardedSandboxHeader),
			body: string(body),
		})
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"sbx_worker"}`)
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(root, testProjectID, testPoolID, controlPlane.URL, "pool-token"); err != nil {
		t.Fatal(err)
	}
	resolver := newSecretResolver(root, testProjectID, testPoolID)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://"+GateHost+"/projects/default/sandboxes?wait=false", strings.NewReader(`{"config":{"name":"worker"}}`))
	req.Header.Set("Authorization", "Bearer something-the-sandbox-sent")
	// A sandbox cannot speak for another: whatever it claims is replaced.
	req.Header.Set(poolauth.ForwardedSandboxHeader, "sb-somebody-else")
	admitted, err := resolver.Gate(context.Background(), proxy.SecretGateRequest{ClientID: "sb-1", Request: req})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	resp := admitted.Response
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || len(seen) != 1 {
		t.Fatalf("status = %d, calls = %d; want the control plane's answer to one call", resp.StatusCode, len(seen))
	}
	got := seen[0]
	if got.method != http.MethodPost || got.uri != "/projects/default/sandboxes?wait=false" || got.body != `{"config":{"name":"worker"}}` {
		t.Fatalf("forwarded %+v, want the call as the sandbox made it", got)
	}
	if got.auth != "Bearer pool-token" || got.pool != testPoolID || got.sandbox != "sb-1" {
		t.Fatalf("forwarded as %+v, want the pool's assertion naming sb-1", got)
	}
}

// A pool that has no control-plane credential yet refuses the call rather than
// forwarding it unasserted.
func TestTheGateRefusesWithoutAControlPlaneCredential(t *testing.T) {
	root := withTestRoot(t)
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+GateHost+"/projects/default/sandboxes", nil)
	_, err := resolver.Gate(context.Background(), proxy.SecretGateRequest{ClientID: "sb-1", Request: req})
	var refusal *proxy.SecretGateRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the call refused", err)
	}
}

// A gate call carries the pool's credential onward, so where it goes is the
// control plane's host and a path on it, whatever the sandbox wrote. A
// request-target that is not a path could only be an attempt to send the
// pool's credential somewhere else, and is refused.
func TestTheGateSendsOnlyAPathToTheControlPlane(t *testing.T) {
	for _, tc := range []struct {
		name, base, target, want string
	}{
		{"origin form", "http://127.0.0.1:3002", "/projects/default/sandboxes?wait=1", "http://127.0.0.1:3002/projects/default/sandboxes?wait=1"},
		{"absolute form, named for the gate", "http://127.0.0.1:3002", "http://api.discobox.internal/projects/default/sandboxes", "http://127.0.0.1:3002/projects/default/sandboxes"},
		{"absolute form, naming another host", "http://127.0.0.1:3002", "http://evil.example/x", "http://127.0.0.1:3002/x"},
		{"a base with a path", "http://127.0.0.1:3002/api/", "/projects", "http://127.0.0.1:3002/api/projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			got, err := gateTarget(tc.base, in)
			if err != nil || got != tc.want {
				t.Fatalf("gateTarget(%q) = %q, %v; want %q", tc.target, got, err, tc.want)
			}
		})
	}
	for _, target := range []string{"http:@evil.example/x", "http://user@evil.example/x", "*"} {
		t.Run("refuses "+target, func(t *testing.T) {
			in, err := url.Parse(target)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := gateTarget("http://127.0.0.1:3002", in); err == nil {
				t.Fatalf("gateTarget(%q) = %q, want it refused", target, got)
			}
		})
	}
}

// A call the gate let in that the control plane then does not answer is a
// failure, not a refusal: the request may have reached it.
func TestAGateCallThatFailsIsNoRefusal(t *testing.T) {
	root := withTestRoot(t)
	controlPlane := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := controlPlane.URL
	controlPlane.Close()
	if err := WriteResolveContext(root, testProjectID, testPoolID, unreachable, "pool-token"); err != nil {
		t.Fatal(err)
	}
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+GateHost+"/projects/default/sandboxes", nil)
	_, err := resolver.Gate(context.Background(), proxy.SecretGateRequest{ClientID: "sb-1", Request: req})
	var refusal *proxy.SecretGateRefusal
	if err == nil || errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the gate failing, not refusing", err)
	}
}
