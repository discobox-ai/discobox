package proxyagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
)

const (
	testEphemeral = "sk-ant-oat01-EPHEMERALSENTINEL00000000000000"
	testStable    = "sk-ant-oat01-STABLESENTINEL0000000000000000"
)

// judgingPool stands up a resolver with one live activation and a control
// plane that answers judging asks with whatever the test set.
func judgingPool(t *testing.T, answer any, status int) (*secretResolver, func() []judgeAsk) {
	t.Helper()
	return judgingPoolFunc(t, func() response { return response{status: status, body: answer} })
}

// response is one answer from the stub control plane. contentType overrides
// what it would otherwise send, for the cases about something answering that
// is not the control plane.
type response struct {
	status      int
	body        any
	contentType string
}

// judgingPoolFunc is judgingPool with an answer the test can change between
// requests, for the cases that are about what a pool has learned.
func judgingPoolFunc(t *testing.T, answer func() response) (*secretResolver, func() []judgeAsk) {
	t.Helper()
	var mu sync.Mutex
	var asked []judgeAsk
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ask judgeAsk
		if err := json.NewDecoder(r.Body).Decode(&ask); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// The handler runs on the server's goroutine, so what it records is
		// shared with the test that reads it — on both sides, which is why the
		// reader below is a copy taken under the same lock.
		mu.Lock()
		asked = append(asked, ask)
		mu.Unlock()
		said := answer()
		// The content type the control plane actually uses, which is how a
		// pool tells this server's own refusal from an ingress's.
		if said.status >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		if said.contentType != "" {
			w.Header().Set("Content-Type", said.contentType)
		}
		w.WriteHeader(said.status)
		_ = json.NewEncoder(w).Encode(said.body)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	contextPath := dir + "/resolve.json"
	if err := writeJSONAtomic(contextPath, resolveContext{
		ControlPlaneURL: server.URL, PoolID: "pool-1", Token: "token",
	}); err != nil {
		t.Fatal(err)
	}
	live := newActivations()
	live.byEphemeral[testEphemeral] = activation{
		SandboxID: "sandbox-1",
		Sentinel:  testEphemeral,
		Stable:    testStable,
		UseID:     "use_abc",
		Host:      "api.github.com",
		ExpiresAt: time.Now().Add(time.Minute),
	}
	plane := &controlPlaneCredentials{contextPath: contextPath, client: server.Client()}
	return &secretResolver{
			contextPath: contextPath,
			client:      server.Client(),
			activations: live,
			judge:       &judgeClient{plane: plane},
		}, func() []judgeAsk {
			mu.Lock()
			defer mu.Unlock()
			return append([]judgeAsk(nil), asked...)
		}
}

func authorizeRequest() proxy.SecretAuthorizeRequest {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+testEphemeral)
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", "412")
	header.Set("User-Agent", "gh/2.0")
	return proxy.SecretAuthorizeRequest{
		ClientID:  "sandbox-1",
		Sentinels: []string{testEphemeral},
		Method:    http.MethodPost,
		Host:      "api.github.com",
		URL:       "https://api.github.com/repos/org/repo/pulls",
		Header:    header,
	}
}

// A request spending an approved use is put to the judge, and what the judge
// says is the answer. Nothing about what the use allows is sent: the pool
// names the discobox and the use, and the control plane reads the rest.
func TestAJudgedRequestIsAskedAboutAndRefusedOnADeny(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{
		"allow": false, "reason": "deleting a repository is not opening a pull request",
	}, http.StatusOK)

	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if verdict.Allow {
		t.Fatalf("verdict = %+v, want the judge's refusal", verdict)
	}
	if verdict.Reason != "deleting a repository is not opening a pull request" {
		t.Fatalf("reason = %q, want the judge's own words", verdict.Reason)
	}
	if len(verdict.UseIDs) != 1 || verdict.UseIDs[0] != "use_abc" {
		t.Fatalf("useIDs = %v, want the use it was refused under", verdict.UseIDs)
	}
	if len(asked()) != 1 {
		t.Fatalf("asked %d times, want once", len(asked()))
	}
	ask := asked()[0]
	if ask.SandboxID != "sandbox-1" || ask.UseID != "use_abc" || ask.Round != 1 {
		t.Fatalf("ask = %+v, want the discobox, the use and a first round", ask)
	}
}

// An allow is an allow, and the use it was allowed under travels with it.
func TestAnAllowedRequestCarriesTheUse(t *testing.T) {
	resolver, _ := judgingPool(t, map[string]any{"allow": true, "reason": "that is the approved use"}, http.StatusOK)

	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !verdict.Allow || len(verdict.UseIDs) != 1 || verdict.UseIDs[0] != "use_abc" {
		t.Fatalf("verdict = %+v, want an allow naming the use", verdict)
	}
}

// Nothing that stands for a credential reaches the judge. The sentinel is not
// a credential, but it is the string that stands for one, and the header it
// travels in is where a real one would be.
func TestTheJudgeIsShownNoCredentialAndNoSentinel(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": true}, http.StatusOK)

	req := authorizeRequest()
	// A sentinel travels base64-encoded as readily as it travels literally, so
	// a string replace of its own would miss this one.
	encoded := base64.StdEncoding.EncodeToString([]byte(testEphemeral))
	req.URL = "https://api.github.com/repos/org/repo/pulls?token=" + testEphemeral + "&state=" + encoded
	// A credential the discobox brought itself, in a header nobody thought to
	// name and in a query parameter that is a well-known place to put one.
	req.Header.Set("Private-Token", "glpat-itsownsecret")
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	shown, err := json.Marshal(asked()[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{testEphemeral, testStable, encoded, "glpat-itsownsecret"} {
		if strings.Contains(string(shown), secret) {
			t.Fatalf("%q reached the judge: %s", secret, shown)
		}
	}
	evidence := asked()[0].Request
	if got := evidence.Headers["Authorization"]; len(got) != 1 || got[0] != redactedValue {
		t.Fatalf("Authorization = %v, want it redacted", got)
	}
	// A header whose value is not shown still says it was there: the judge
	// weighs what was sent, and "there was one" is part of that.
	if got := evidence.Headers["Private-Token"]; len(got) != 1 || got[0] != redactedValue {
		t.Fatalf("Private-Token = %v, want its name kept and its value gone", got)
	}
	if got := evidence.Headers["User-Agent"]; len(got) != 1 || got[0] != "gh/2.0" {
		t.Fatalf("User-Agent = %v, want what the request carried", got)
	}
	if evidence.Body == nil || evidence.Body.MediaType != "application/json" || evidence.Body.Length != 412 {
		t.Fatalf("body = %+v, want it described by what the request declared", evidence.Body)
	}
}

// A body the request did not measure is not described at all. The contract
// says a length in bytes and has no way to spell "some unknown number of
// them", so describing a chunked upload here would report it as empty — which
// is worse than saying nothing, because the judge would weigh it.
func TestABodyWithNoDeclaredLengthIsNotDescribed(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": true}, http.StatusOK)

	req := authorizeRequest()
	req.Header.Del("Content-Length")
	req.Header.Set("Transfer-Encoding", "chunked")
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if body := asked()[0].Request.Body; body != nil {
		t.Fatalf("body = %+v, want nothing said about a body nobody measured", body)
	}
}

// A judge asking to be shown the body has not allowed anything. The round that
// answers it is not implemented, and a question left unanswered is not
// permission.
func TestAskingForTheBodyIsNotAnAllow(t *testing.T) {
	resolver, _ := judgingPool(t, map[string]any{"reason": "show me the body", "need": map[string]any{"body": "json"}}, http.StatusOK)

	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if verdict.Allow {
		t.Fatalf("verdict = %+v, want a request that is not allowed", verdict)
	}
}

// A server that does not judge is not a refusal. The pool takes that answer,
// allows the request, and stops asking for a while rather than putting a
// control-plane call in front of every credential its discoboxes spend.
func TestAServerThatDoesNotJudgeIsNotARefusal(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{
		"type": judgingDisabledKind, "status": 503, "detail": "this server does not judge credential use",
	}, http.StatusServiceUnavailable)

	for i := range 3 {
		verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
		if err != nil {
			t.Fatalf("Authorize() error = %v", err)
		}
		if !verdict.Allow {
			t.Fatalf("request %d: verdict = %+v, want it allowed where there is nobody to ask", i, verdict)
		}
	}
	if len(asked()) != 1 {
		t.Fatalf("asked %d times, want the answer remembered after the first", len(asked()))
	}
}

// unreachablePool is a resolver whose control plane is not there at all: the
// case where nobody answers, as opposed to answering with a refusal.
func unreachablePool(t *testing.T) *secretResolver {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := dead.URL
	dead.Close()

	dir := t.TempDir()
	contextPath := dir + "/resolve.json"
	if err := writeJSONAtomic(contextPath, resolveContext{
		ControlPlaneURL: address, PoolID: "pool-1", Token: "token",
	}); err != nil {
		t.Fatal(err)
	}
	live := newActivations()
	live.byEphemeral[testEphemeral] = activation{
		SandboxID: "sandbox-1", Sentinel: testEphemeral, Stable: testStable,
		UseID: "use_abc", Host: "api.github.com", ExpiresAt: time.Now().Add(time.Minute),
	}
	return &secretResolver{
		contextPath: contextPath,
		activations: live,
		judge: &judgeClient{plane: &controlPlaneCredentials{
			contextPath: contextPath,
			client:      &http.Client{Timeout: time.Second},
		}},
	}
}

// Nobody answering, on a pool that has never been told this server judges, is
// not a refusal. A server that never turned judging on must not be able to
// break its discoboxes through a feature it is not using — a control plane
// that is restarting is not evidence that judging is required.
func TestNobodyAnsweringBeforeEverHavingAnAnswerIsNotARefusal(t *testing.T) {
	resolver := unreachablePool(t)

	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err != nil || !verdict.Allow {
		t.Fatalf("verdict = %+v, err = %v; want the request allowed", verdict, err)
	}
}

// Once this pool has had an answer from a server that judges, nobody answering
// is a request that goes nowhere. The server said it judges; a link that went
// down afterwards does not unsay it.
func TestNobodyAnsweringAfterHavingHadOneRefuses(t *testing.T) {
	resolver := unreachablePool(t)
	resolver.judge.answered()

	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err == nil && verdict.Allow {
		t.Fatalf("verdict = %+v, err = %v; want a judge that has stopped answering to refuse", verdict, err)
	}
}

// A control plane that answers has demonstrably not been turned off, so its
// refusal is a refusal whatever this pool has learned so far — and it is what
// teaches the pool that this server judges.
//
// Without that, a discobox could turn judging off for the whole pool: the
// evidence carries the request's own headers and URL, so a big enough one is
// refused by the control plane on its size, and treating that as "nobody is
// judging" would allow it and silence the next thirty seconds of asks for
// every sandbox here.
func TestAControlPlaneThatAnswersRefusesEvenBeforeTheFirstVerdict(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{
		"status": 400, "detail": "the evidence for one job exceeds what a judge may be shown",
	}, http.StatusBadRequest)

	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err == nil && verdict.Allow {
		t.Fatalf("verdict = %+v, err = %v; want the control plane's refusal to be one", verdict, err)
	}
	// And the next request is asked about rather than waved through: being
	// refused is not being told there is nobody to ask.
	if _, err := resolver.Authorize(context.Background(), authorizeRequest()); err == nil {
		t.Fatal("the request after a refusal was allowed without asking")
	}
	if len(asked()) != 2 {
		t.Fatalf("asked %d times, want every request asked about", len(asked()))
	}
}

// A sentinel spending no approved use is not judged at all: there is no
// sentence to judge it against. That is the ordinary injected harness
// credential, and it is the hot path.
func TestASentinelWithNoUseIsNotJudged(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": false, "reason": "no"}, http.StatusOK)

	req := authorizeRequest()
	req.Sentinels = []string{"sk-ant-oat01-INJECTEDHARNESSCREDENTIAL000000"}
	verdict, err := resolver.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !verdict.Allow {
		t.Fatalf("verdict = %+v, want a sentinel with no use left alone", verdict)
	}
	if len(asked()) != 0 {
		t.Fatalf("asked %d times about a sentinel spending no use", len(asked()))
	}
}

// allowJudgingAsk answers a judging ask with an allow, for the tests whose
// subject is something else. A use-scoped request is judged now, so a stub
// control plane that does not answer one refuses every request that spends an
// approved use — including a sandbox's own calls to the discobox API.
func allowJudgingAsk(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasSuffix(r.URL.Path, "/judge") {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"allow":true,"reason":"that is the approved use"}`))
	return true
}

// What the judge is shown of a URL is what was sent. Redaction rewrites the
// query in place, because parsing and re-encoding it drops segments Go will
// not parse — and a discobox that can make the judge lose a parameter it still
// sends upstream can hide the operative half of its request.
func TestTheJudgeSeesTheQueryAsItWasSent(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a semicolon segment survives",
			"https://api.github.com/x?token=abc&mode=destroy;pad=1",
			"https://api.github.com/x?token=%3Credacted%3E&mode=destroy;pad=1"},
		{"a bad escape survives",
			"https://api.github.com/x?token=abc&danger=1%zz",
			"https://api.github.com/x?token=%3Credacted%3E&danger=1%zz"},
		{"order is kept",
			"https://api.github.com/x?z=1&api_key=abc&a=2",
			"https://api.github.com/x?z=1&api_key=%3Credacted%3E&a=2"},
		{"a valueless parameter is left alone",
			"https://api.github.com/x?verbose&token=abc",
			"https://api.github.com/x?verbose&token=%3Credacted%3E"},
		{"nothing to redact is untouched",
			"https://api.github.com/x?q=search+term&page=2",
			"https://api.github.com/x?q=search+term&page=2"},
		{"a fragment stays at the end",
			"https://api.github.com/x?token=abc#frag",
			"https://api.github.com/x?token=%3Credacted%3E#frag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactedURL(tc.in, nil); got != tc.want {
				t.Fatalf("redactedURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A 4xx says the control plane would not take this ask. It refuses the
// request, and that is all it does: it is as likely to come from a body limit
// or a proxy in front of the handler as from the judge, so it must not be read
// as proof that this server judges. If it were, a discobox on a server with
// judging turned off could arm a refusal that fires on the next outage.
func TestARefusedAskDoesNotDecideThatTheServerJudges(t *testing.T) {
	var answer atomic.Value
	answer.Store(response{status: http.StatusRequestEntityTooLarge, body: map[string]any{"detail": "too big"}})
	resolver, _ := judgingPoolFunc(t, func() response { return answer.Load().(response) })

	if verdict, err := resolver.Authorize(context.Background(), authorizeRequest()); err == nil && verdict.Allow {
		t.Fatalf("verdict = %+v, err = %v; want the ask refused", verdict, err)
	}

	// Now nobody answers. Since nothing has ever judged for this pool, that is
	// not a refusal — which is only true if the 413 above latched nothing.
	answer.Store(response{status: http.StatusBadGateway, body: map[string]any{"detail": "gateway is down"}})
	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err != nil || !verdict.Allow {
		t.Fatalf("verdict = %+v, err = %v; want an outage to allow on a pool that was never told this server judges", verdict, err)
	}
}

// A discobox that hangs up while the judge is thinking refuses its own request
// and nothing more. Treating it as a control plane that went quiet would let a
// sandbox silence the next thirty seconds of asks for everybody on the pool,
// one aborted connection at a time.
func TestADiscoboxHangingUpDoesNotSilenceTheNextAsk(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": true}, http.StatusOK)

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if verdict, err := resolver.Authorize(dead, authorizeRequest()); err == nil && verdict.Allow {
		t.Fatalf("verdict = %+v, err = %v; want the abandoned request to carry nothing", verdict, err)
	}

	if _, err := resolver.Authorize(context.Background(), authorizeRequest()); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if len(asked()) != 1 {
		t.Fatalf("the judge was asked %d times, want the next request asked about", len(asked()))
	}
}

// A status from something that is not this server's judging handler says
// nothing about whether the server judges. An ingress returning 429, a gateway
// returning a plain-text 404 while the control plane restarts: none of them is
// an opinion, and reading one as an opinion is how a pool ends up refusing
// every discobox on a server that never turned judging on.
func TestAStatusFromSomethingElseSaysNothingAboutJudging(t *testing.T) {
	for _, tc := range []struct {
		name string
		said response
	}{
		{"an ingress refusing", response{status: http.StatusTooManyRequests, body: "slow down", contentType: "text/plain"}},
		{"a plain-text not-found", response{status: http.StatusNotFound, body: "404 page not found", contentType: "text/plain"}},
		{"a gateway", response{status: http.StatusBadGateway, body: "<html>bad gateway</html>", contentType: "text/html"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver, _ := judgingPoolFunc(t, func() response { return tc.said })
			verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
			if err != nil || !verdict.Allow {
				t.Fatalf("verdict = %+v, err = %v; want a pool that was never told this server judges to allow", verdict, err)
			}
		})
	}
}

// This server declining the ask is a refusal, and it is one whether or not the
// discobox could have caused it: a pool or project the control plane cannot
// find is its own problem, and letting a credential out because of it is not
// the answer.
func TestThisServersOwnRefusalIsARefusal(t *testing.T) {
	resolver, _ := judgingPool(t, map[string]any{
		"status": 404, "title": "Not Found", "detail": "pool not found",
	}, http.StatusNotFound)

	verdict, err := resolver.Authorize(context.Background(), authorizeRequest())
	if err == nil && verdict.Allow {
		t.Fatalf("verdict = %+v, err = %v; want this server's own 404 to refuse", verdict, err)
	}
}

// A request to a host the sandbox trusts by a pin is judged against the uses
// that trust was granted for, whether or not it carries a credential
// (ADR 0149 §5). Nothing here spends a sentinel, so if the trust's uses were
// not asked about, nothing would be — and the pin would authorize everything
// the sandbox sent that host.
func TestATrustedHostIsJudgedWithNoCredentialAtAll(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{
		"allow": false, "reason": "deleting a namespace is not reading the pods in it",
	}, http.StatusOK)

	req := authorizeRequest()
	req.Sentinels = nil
	req.Header.Del("Authorization")
	req.TrustUseIDs = []string{"use_kube"}
	req.Method = http.MethodDelete
	req.URL = "https://kube.internal:6443/api/v1/namespaces/prod"
	req.Host = "kube.internal"

	verdict, err := resolver.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if verdict.Allow {
		t.Fatalf("verdict = %+v, want the judge's refusal", verdict)
	}
	if verdict.Reason != "deleting a namespace is not reading the pods in it" {
		t.Fatalf("reason = %q, want the judge's own words", verdict.Reason)
	}
	if len(asked()) != 1 {
		t.Fatalf("asked %d times, want the trusted host's request asked about", len(asked()))
	}
	if ask := asked()[0]; ask.UseID != "use_kube" {
		t.Fatalf("asked about use %q, want the one the pin was granted for", ask.UseID)
	}
}

// A request that spends a credential and goes to a trusted host is asked about
// both, and either saying no is the answer.
func TestACredentialToATrustedHostIsJudgedAgainstBoth(t *testing.T) {
	resolver, asked := judgingPool(t, map[string]any{"allow": true}, http.StatusOK)

	req := authorizeRequest()
	req.TrustUseIDs = []string{"use_kube"}
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	seen := map[string]bool{}
	for _, ask := range asked() {
		seen[ask.UseID] = true
	}
	if !seen["use_kube"] || !seen["use_abc"] {
		t.Fatalf("asked about %v, want both the trust's use and the credential's", seen)
	}
}

// One use saying no is the answer, whichever it is. A request that spends a
// credential and goes to a pinned host asks about both, and the request goes
// nowhere if either refuses — which is the property "every applicable use must
// pass" actually means.
func TestEitherUseRefusingRefusesTheRequest(t *testing.T) {
	for _, tc := range []struct{ name, deny string }{
		{"the trust's use refuses", "use_kube"},
		{"the credential's use refuses", "use_abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver, _ := judgingPoolFunc(t, func() response { return response{status: http.StatusOK, body: nil} })
			// Answer per use: the named one refuses, the other allows.
			resolver.judge.plane.client = denyingClient(t, resolver, tc.deny)

			req := authorizeRequest()
			req.TrustUseIDs = []string{"use_kube"}
			verdict, err := resolver.Authorize(context.Background(), req)
			if err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			if verdict.Allow {
				t.Fatalf("verdict = %+v, want %s to have refused the request", verdict, tc.deny)
			}
		})
	}
}

// denyingClient answers a judging ask by refusing exactly one use and allowing
// every other.
func denyingClient(t *testing.T, resolver *secretResolver, deny string) *http.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ask judgeAsk
		_ = json.NewDecoder(r.Body).Decode(&ask)
		w.Header().Set("Content-Type", "application/json")
		if ask.UseID == deny {
			_, _ = w.Write([]byte(`{"allow":false,"reason":"that is not what ` + deny + ` is for"}`))
			return
		}
		_, _ = w.Write([]byte(`{"allow":true,"reason":"fine"}`))
	}))
	t.Cleanup(server.Close)
	if err := writeJSONAtomic(resolver.contextPath, resolveContext{
		ControlPlaneURL: server.URL, PoolID: "pool-1", Token: "token",
	}); err != nil {
		t.Fatal(err)
	}
	return server.Client()
}
