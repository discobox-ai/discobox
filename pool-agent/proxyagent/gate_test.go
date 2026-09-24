package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/proxy"
)

// gateCall is what the fake control plane saw of one forwarded call.
type gateCall struct {
	method, uri, auth, pool, sandbox, body string
}

// A sandbox's call to the discobox API goes on to the control plane only while
// it carries a live use of the discobox credential activated for it, with its
// own credential removed and the pool's word for who is calling put in its
// place (ADR 0140 §§2–3).
func TestTheGateForwardsALiveUseAsThePoolsWord(t *testing.T) {
	withTestRoot(t)
	var seen []gateCall
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowJudgingAsk(w, r) {
			return
		}
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
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlane.URL, "pool-token"); err != nil {
		t.Fatal(err)
	}
	live := newActivations()
	resolver := newSecretResolver(testProjectID, testPoolID, live)
	use, err := live.mint("sb-1", "STABLE", "use-1", GateHost(), "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	call := func(sandboxID, sentinel string) (proxy.SecretGateAdmission, error) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
			"https://"+GateHost()+"/projects/default/sandboxes?wait=false", strings.NewReader(`{"config":{"name":"worker"}}`))
		req.Header.Set("Authorization", "Bearer "+sentinel)
		// A sandbox cannot speak for another: whatever it claims is replaced.
		req.Header.Set(poolauth.ForwardedSandboxHeader, "sb-somebody-else")
		return resolver.Gate(context.Background(), proxy.SecretGateRequest{ClientID: sandboxID, Request: req})
	}

	admitted, err := call("sb-1", use.Sentinel)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	resp := admitted.Response
	_ = resp.Body.Close()
	if admitted.UseID != "use-1" {
		t.Fatalf("admitted under %q, want the use the sentinel was minted for", admitted.UseID)
	}
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
	if strings.Contains(got.auth, use.Sentinel) {
		t.Fatal("the sandbox's sentinel went on to the control plane")
	}

	// Anything but a live use of the discobox credential for this sandbox is
	// refused, and never reaches the control plane.
	github, err := live.mint("sb-1", "STABLE-GH", "use-2", "github.com", "ghp_{base62:36}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for name, tc := range map[string]struct{ sandboxID, sentinel string }{
		"no sentinel":                 {"sb-1", ""},
		"a sentinel nobody minted":    {"sb-1", "not-a-sentinel"},
		"another sandbox's use":       {"sb-2", use.Sentinel},
		"a use of another credential": {"sb-1", github.Sentinel},
	} {
		t.Run(name, func(t *testing.T) {
			admitted, err := call(tc.sandboxID, tc.sentinel)
			if admitted.Response != nil {
				_ = admitted.Response.Body.Close()
			}
			var refusal *proxy.SecretGateRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want the call refused", err)
			}
		})
	}
	if len(seen) != 1 {
		t.Fatalf("control plane saw %d calls, want only the admitted one", len(seen))
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

// A call the gate let in keeps the use it went under when the control plane
// then does not answer: the request may have reached it.
func TestAGateCallThatFailsKeepsItsUse(t *testing.T) {
	withTestRoot(t)
	controlPlane := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := controlPlane.URL
	controlPlane.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, unreachable, "pool-token"); err != nil {
		t.Fatal(err)
	}
	live := newActivations()
	resolver := newSecretResolver(testProjectID, testPoolID, live)
	// This is about the hop after the gate let the call in, so there is no
	// judge in the way. With one, the ask to an unreachable control plane
	// would be answered by nobody — which on a pool that has never had a
	// verdict allows the call rather than refusing it, and the test would pass
	// for a reason that has nothing to do with what it is checking.
	resolver.judge = nil
	use, err := live.mint("sb-1", "STABLE", "use-1", GateHost(), "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+GateHost()+"/projects/default/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer "+use.Sentinel)
	admitted, err := resolver.Gate(context.Background(), proxy.SecretGateRequest{ClientID: "sb-1", Request: req})
	var refusal *proxy.SecretGateRefusal
	if err == nil || errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the gate failing, not refusing", err)
	}
	if admitted.UseID != "use-1" {
		t.Fatalf("admission names %q, want the use the call went under", admitted.UseID)
	}
}

// What a call to the discobox API does is mostly in its body — which discobox
// to create, and what it is given — so a judge asks to see it, is shown it,
// and the control plane still receives the call exactly as it was made.
func TestTheGateShowsTheJudgeTheBodyItAsksFor(t *testing.T) {
	withTestRoot(t)
	const sent = `{"prompt":"fix issue 43","grants":[{"id":"com.github.api","uses":[{"description":"push branch fix-43"}]}]}`
	var shown atomic.Value
	var forwarded atomic.Value
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/judge") {
			var ask judgeAsk
			_ = json.NewDecoder(r.Body).Decode(&ask)
			w.Header().Set("Content-Type", "application/json")
			if ask.Round == 1 {
				_, _ = io.WriteString(w, `{"reason":"which discobox, granted what, is in the body","need":{"body":"json"}}`)
				return
			}
			shown.Store(ask.Request.Body.Content)
			_, _ = io.WriteString(w, `{"allow":true,"reason":"one discobox for issue 43, granted a push to its branch"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		forwarded.Store(string(body))
		w.WriteHeader(http.StatusCreated)
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlane.URL, "pool-token"); err != nil {
		t.Fatal(err)
	}
	live := newActivations()
	resolver := newSecretResolver(testProjectID, testPoolID, live)
	use, err := live.mint("sb-1", "STABLE", "use-1", GateHost(), "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://"+GateHost()+"/projects/default/sandboxes", strings.NewReader(sent))
	req.Header.Set("Authorization", "Bearer "+use.Sentinel)
	req.Header.Set("Content-Type", "application/json")
	admitted, err := resolver.Gate(context.Background(), proxy.SecretGateRequest{ClientID: "sb-1", Request: req})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	_ = admitted.Response.Body.Close()
	if got, _ := shown.Load().(string); got != sent {
		t.Fatalf("the judge was shown %q, want the body it asked for", got)
	}
	if got, _ := forwarded.Load().(string); got != sent {
		t.Fatalf("the control plane received %q, want the call as the sandbox made it", got)
	}
}

// A sandbox's call to the discobox API spends an approved use like any other
// credential-bearing request, so it is judged, and a refusal is what the
// sandbox is told. The gate reaches the judge by a different path than the
// proxy's swap does, so it is worth its own test.
func TestTheGateRefusesACallTheJudgeDoesNot(t *testing.T) {
	withTestRoot(t)
	// Written by the server's goroutine and read by the test, which is a race
	// precisely when the test is about to fail.
	var reached atomic.Bool
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/judge") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"allow":false,"reason":"deleting a discobox is not what that use is for"}`)
			return
		}
		reached.Store(true)
	}))
	defer controlPlane.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, controlPlane.URL, "pool-token"); err != nil {
		t.Fatal(err)
	}
	live := newActivations()
	resolver := newSecretResolver(testProjectID, testPoolID, live)
	use, err := live.mint("sb-1", "STABLE", "use-1", GateHost(), "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
		"https://"+GateHost()+"/projects/default/sandboxes/sbx_other", nil)
	req.Header.Set("Authorization", "Bearer "+use.Sentinel)
	_, err = resolver.Gate(context.Background(), proxy.SecretGateRequest{ClientID: "sb-1", Request: req})

	var refusal *proxy.SecretGateRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the gate refusing the call", err)
	}
	if refusal.Reason != "deleting a discobox is not what that use is for" {
		t.Fatalf("reason = %q, want the judge's own words", refusal.Reason)
	}
	if refusal.UseID != "use-1" {
		t.Fatalf("useID = %q, want the use it was refused under", refusal.UseID)
	}
	if reached.Load() {
		t.Fatal("a call the judge refused reached the control plane")
	}
}
