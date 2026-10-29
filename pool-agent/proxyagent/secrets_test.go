package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy"
)

func TestUpsertAndRemoveSentinels(t *testing.T) {
	root := withTestRoot(t)

	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", []string{"SENT-A", "SENT-B"}, nil); err != nil {
		t.Fatalf("upsert sb-1: %v", err)
	}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-2", []string{"SENT-C"}, nil); err != nil {
		t.Fatalf("upsert sb-2: %v", err)
	}

	doc, err := readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if len(doc.Clients["sb-1"]) != 2 || doc.Clients["sb-2"][0] != "SENT-C" {
		t.Fatalf("unexpected doc: %#v", doc.Clients)
	}

	if err := RemoveSandboxPolicy(root, testProjectID, testPoolID, "sb-1"); err != nil {
		t.Fatalf("remove sb-1: %v", err)
	}
	doc, _ = readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if _, ok := doc.Clients["sb-1"]; ok {
		t.Fatal("sb-1 should be removed")
	}
	if _, ok := doc.Clients["sb-2"]; !ok {
		t.Fatal("sb-2 should remain")
	}

	clients := secretClients(doc.Clients)
	if len(clients) != 1 || clients[0].ClientID != "sb-2" {
		t.Fatalf("secretClients = %#v", clients)
	}
}

func TestResolverApproved(t *testing.T) {
	root := withTestRoot(t)
	var gotAuth, gotPath string
	var gotBody resolveRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// Spelled as the server's resolve-sandbox-secret answer spells it, not
		// with this package's own struct, so a wrong field name shows here.
		exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		_, _ = w.Write([]byte(`{"status":"approved","value":"real-secret","expiresAt":"` + exp + `","secretId":"sec_1","reportScope":[{"host":"api.example.com","paths":["/v1/messages"]}]}`))
	}))
	defer srv.Close()

	if err := WriteResolveContext(root, testProjectID, testPoolID, srv.URL, "tok-123"); err != nil {
		t.Fatalf("write context: %v", err)
	}
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	res, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "api.example.com"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Value != "real-secret" {
		t.Fatalf("value = %q", res.Value)
	}
	// The requests that answer for the credential, which the proxy holds its
	// reports to (ADR 26-10-13-595).
	if !res.ReportScope.Covers("api.example.com", "/v1/messages/count_tokens") || res.ReportScope.Covers("api.example.com", "/v1/code/runners") {
		t.Fatalf("report scope = %#v, want /v1/messages on api.example.com and nothing else", res.ReportScope)
	}
	// The secret the value is, which the proxy records on the request
	// (ADR 26-10-01-240).
	if res.SecretID != "sec_1" {
		t.Fatalf("secretId = %q, want sec_1", res.SecretID)
	}
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotPath != "/api/pools/"+testPoolID+"/resolve-sandbox-secret" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.SandboxID != "sb-1" || gotBody.Sentinel != "SENT" || gotBody.Host != "api.example.com" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestResolverPendingIsDenied(t *testing.T) {
	root := withTestRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "pending"})
	}))
	defer srv.Close()
	_ = WriteResolveContext(root, testProjectID, testPoolID, srv.URL, "tok")

	resolver := newSecretResolver(root, testProjectID, testPoolID)
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied", err)
	}
}

// A status that says the resolve went unanswered — the control plane erroring,
// timing out, or rate limiting — is transient, so the proxy holds the request
// instead of sending its sentinel; a status that answers is a denial
// (ADR 26-10-14-482 §2).
func TestResolverTellsAnUnansweredResolveFromADenial(t *testing.T) {
	for status, transient := range map[int]bool{
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
		http.StatusRequestTimeout:      true,
		http.StatusTooEarly:            true,
		http.StatusTooManyRequests:     true,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			root := withTestRoot(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()
			_ = WriteResolveContext(root, testProjectID, testPoolID, srv.URL, "tok")

			resolver := newSecretResolver(root, testProjectID, testPoolID)
			_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
			if err == nil {
				t.Fatal("resolved, want an error")
			}
			if denied := errors.Is(err, proxy.ErrSecretResolveDenied); denied == transient {
				t.Fatalf("err = %v, want transient %v", err, transient)
			}
		})
	}
}

func TestResolverNoContextIsDenied(t *testing.T) {
	root := withTestRoot(t)
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied when no context file", err)
	}
}

// A report names the sentinel the upstream refused, and never the value
// (ADR 0132 §2).
func TestReportNamesTheSentinel(t *testing.T) {
	root := withTestRoot(t)
	var got rejectionRequestBody
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_ = WriteResolveContext(root, testProjectID, testPoolID, srv.URL, "tok")

	resolver := newSecretResolver(root, testProjectID, testPoolID)
	err := resolver.Report(context.Background(), proxy.SecretReportRequest{
		ClientID: "sb-1", Sentinel: "SENTINEL", Host: "api.example.com", Outcome: proxy.SecretRejected,
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if gotPath != "/api/pools/"+testPoolID+"/sandbox-secret-rejections" {
		t.Fatalf("path = %q", gotPath)
	}
	want := rejectionRequestBody{SandboxID: "sb-1", Sentinel: "SENTINEL", Host: "api.example.com", Outcome: string(proxy.SecretRejected)}
	if got != want {
		t.Fatalf("report = %+v, want %+v", got, want)
	}
}

// A document a pool agent wrote before route lists — one list a sentinel, no
// version — reads as that one list, with a route a judge read (a use) blocked,
// so the upgrade never sends a request on it unjudged; a route without one
// reads as it was written (ADR 26-10-27-880 §8).
func TestARoutePersistedWithAUseReadsBlocked(t *testing.T) {
	root := withTestRoot(t)
	path := root.ProxySecretsFile(testProjectID, testPoolID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"clients":{"sb-1":["SENT"]},"routes":{"sb-1":{"SENT":[` +
		`{"host":"githubcopilot.com"},` +
		`{"host":"githubcopilot.com","path":"/mcp","useId":"use_mcp"},` +
		`{"host":"github.com","useId":"use_gh"}]}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readSecretsDoc(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Each host covered the hosts beneath it, and each path the paths
	// beneath it (secretroute.FromLegacy).
	want := [][]secretroute.Route{parsedRoutes(t,
		"githubcopilot.com,*.githubcopilot.com",
		"githubcopilot.com,*.githubcopilot.com/mcp/**=blocked",
		"github.com,*.github.com=blocked",
	)}
	if got := doc.Routes["sb-1"]["SENT"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}
	r := &secretResolver{}
	r.setSentinelRoutes(doc.Routes)
	if verdict, _ := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
		ClientID: "sb-1", Sentinels: []string{"SENT"}, Host: "api.githubcopilot.com", URL: "https://api.githubcopilot.com/mcp/x",
	}); verdict.Allow {
		t.Fatal("a route a judge used to read was allowed after the upgrade")
	}
	// Written back by this pool, as lists, it keeps saying so.
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-2", []string{"OTHER"}, nil); err != nil {
		t.Fatal(err)
	}
	if doc, err = readSecretsDoc(path); err != nil || !reflect.DeepEqual(doc.Routes["sb-1"]["SENT"], want) {
		t.Fatalf("after a rewrite: routes = %+v (%v), want %+v", doc.Routes["sb-1"]["SENT"], err, want)
	}
}

// A sentinel a document from before route lists held to no route at all reads
// as held to any host, as it was enforced (ADR 26-10-27-939), until the
// control plane pushes its lists; one this pool wrote with no lists is sent
// nowhere.
func TestAnUnroutedSentinelReadsAsItsDocumentMeantIt(t *testing.T) {
	root := withTestRoot(t)
	path := root.ProxySecretsFile(testProjectID, testPoolID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"clients":{"sb-1":["SENT-PLAIN","SENT-ROUTED"]},"routes":{"sb-1":{"SENT-ROUTED":[{"host":"github.com"}]}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readSecretsDoc(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := doc.Routes["sb-1"]["SENT-PLAIN"]; !reflect.DeepEqual(got, anyHost) {
		t.Fatalf("an unrouted sentinel of an old document = %+v, want any host", got)
	}
	if got := doc.Routes["sb-1"]["SENT-ROUTED"]; !reflect.DeepEqual(got, [][]secretroute.Route{parsedRoutes(t, "github.com,*.github.com")}) {
		t.Fatalf("a routed sentinel of an old document = %+v, want its one list", got)
	}

	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-2", []string{"SENT-NEW"}, nil); err != nil {
		t.Fatal(err)
	}
	if doc, err = readSecretsDoc(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Routes["sb-2"]["SENT-NEW"]; ok {
		t.Fatalf("a sentinel written with no lists read as %+v, want none", doc.Routes["sb-2"]["SENT-NEW"])
	}
	r := &secretResolver{}
	r.setSentinelRoutes(doc.Routes)
	if verdict, _ := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
		ClientID: "sb-2", Sentinels: []string{"SENT-NEW"}, Host: "example.com", URL: "https://example.com/",
	}); verdict.Allow {
		t.Fatal("a sentinel with no lists was sent")
	}
	// The old document's unrouted sentinel keeps its reading once rewritten.
	if got := doc.Routes["sb-1"]["SENT-PLAIN"]; !reflect.DeepEqual(got, anyHost) {
		t.Fatalf("after a rewrite the unrouted sentinel = %+v, want any host", got)
	}
}

// A request is held to every route list of every sentinel it carries: off a
// route of any list, or on a blocked one, it is refused with a reason; a
// sentinel with no lists is sent nowhere (ADR 26-10-27-880 §§1, 4).
func TestAuthorizeHoldsARequestToItsSentinelsRouteLists(t *testing.T) {
	r := &secretResolver{}
	r.setSentinelRoutes(map[string]map[string][][]secretroute.Route{"sb-1": {
		"SENT-COPILOT": {parsedRoutes(t,
			"*.githubcopilot.com",
			"*.githubcopilot.com/mcp/**=blocked",
		)},
		// A secret open to any host, narrowed by its binding.
		"SENT-NARROWED": {
			parsedRoutes(t, "*"),
			parsedRoutes(t, "api.github.com/repos/acme/**"),
		},
	}})
	for _, tc := range []struct {
		name      string
		sentinels []string
		url       string
		allowed   bool
	}{
		{"on a route", []string{"SENT-COPILOT"}, "https://api.githubcopilot.com/v1/messages", true},
		{"on a blocked route", []string{"SENT-COPILOT"}, "https://api.githubcopilot.com/mcp/x", false},
		{"off every route", []string{"SENT-COPILOT"}, "https://api.github.com/user", false},
		{"on every list", []string{"SENT-NARROWED"}, "https://api.github.com/repos/acme/web", true},
		{"off the binding's list", []string{"SENT-NARROWED"}, "https://api.github.com/user", false},
		{"a sentinel with no lists", []string{"SENT-PLAIN"}, "https://api.github.com/user", false},
		{"one sentinel off its routes refuses the request", []string{"SENT-NARROWED", "SENT-COPILOT"}, "https://api.github.com/repos/acme/web", false},
	} {
		host, _ := strings.CutPrefix(tc.url, "https://")
		host, _, _ = strings.Cut(host, "/")
		verdict, err := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
			ClientID: "sb-1", Sentinels: tc.sentinels, Method: http.MethodGet, Host: host, URL: tc.url,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if verdict.Allow != tc.allowed || (!verdict.Allow && verdict.Reason == "") {
			t.Fatalf("%s: verdict = %+v, want allowed %v with a reason for a refusal", tc.name, verdict, tc.allowed)
		}
	}
	// Another sandbox's lists are not this one's: holding none, it is sent
	// nowhere.
	if verdict, _ := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
		ClientID: "sb-2", Sentinels: []string{"SENT-COPILOT"}, Host: "api.githubcopilot.com", URL: "https://api.githubcopilot.com/v1/messages",
	}); verdict.Allow {
		t.Fatalf("sb-2 was allowed under sb-1's lists: %+v", verdict)
	}
}

// Route lists are written only for sentinels the sandbox holds, and replaced
// with its sentinel set.
func TestUpsertSandboxSentinelsKeepsRoutesOfHeldSentinels(t *testing.T) {
	root := withTestRoot(t)
	routes := map[string][][]secretroute.Route{
		"SENT-A":    {parsedRoutes(t, "github.com=blocked"), parsedRoutes(t, "api.github.com")},
		"SENT-GONE": {parsedRoutes(t, "example.com")},
	}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", []string{"SENT-A", "SENT-B"}, routes); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	doc, err := readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if got := doc.Routes["sb-1"]; len(got) != 1 || !reflect.DeepEqual(got["SENT-A"], routes["SENT-A"]) {
		t.Fatalf("routes = %#v, want only SENT-A's lists", doc.Routes)
	}
	if doc.Version != secretsDocVersion {
		t.Fatalf("version = %d, want %d", doc.Version, secretsDocVersion)
	}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", []string{"SENT-A"}, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	doc, _ = readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if _, ok := doc.Routes["sb-1"]; ok {
		t.Fatalf("routes = %#v, want them replaced with the set", doc.Routes)
	}
}

// While the sentinel set moves, no sentinel reads as having no lists: one
// arriving has its lists before it is swappable, and one leaving keeps its
// own until it is gone.
func TestStagedRoutesCoverSentinelsArrivingAndLeaving(t *testing.T) {
	r := &secretResolver{}
	leaving := [][]secretroute.Route{parsedRoutes(t, "github.com/old/**")}
	r.setSentinelRoutes(map[string]map[string][][]secretroute.Route{"sb-1": {"SENT-OLD": leaving}})

	arriving := [][]secretroute.Route{parsedRoutes(t, "githubcopilot.com")}
	next := map[string]map[string][][]secretroute.Route{"sb-1": {"SENT-NEW": arriving}}
	r.stageSentinelRoutes(next)
	if got := r.heldRoutes("sb-1", "SENT-NEW"); !reflect.DeepEqual(got, arriving) {
		t.Fatalf("arriving lists = %#v, want them staged before the sentinel set moves", got)
	}
	if got := r.heldRoutes("sb-1", "SENT-OLD"); !reflect.DeepEqual(got, leaving) {
		t.Fatalf("leaving lists = %#v, want them kept until the sentinel set has moved", got)
	}
	if _, ok := next["sb-1"]["SENT-OLD"]; ok {
		t.Fatal("staging wrote the leaving sentinel's lists into the caller's map")
	}

	r.setSentinelRoutes(next)
	if got := r.heldRoutes("sb-1", "SENT-OLD"); len(got) != 0 {
		t.Fatalf("leaving lists = %#v, want them dropped once the set has moved", got)
	}
}

// parsedRoutes reads routes in the typed form a person writes them.
func parsedRoutes(t *testing.T, texts ...string) []secretroute.Route {
	t.Helper()
	routes := make([]secretroute.Route, 0, len(texts))
	for _, text := range texts {
		route, err := secretroute.Parse(text)
		if err != nil {
			t.Fatalf("parse %q: %v", text, err)
		}
		routes = append(routes, route)
	}
	return routes
}

// A version 2 document's route lists read through secretroute.FromLegacy,
// each kept apart. A legacy route the new shape cannot say — a path holding
// `*`, literal then and a glob now — fails closed: an open one is left out, a
// blocked one blocks its path cut back before the `*`, outranking the open
// route it was a carve-out from, and a blocked one whose host does not read
// either makes its list refuse everything (secretroute.FromLegacyList).
// None is dropped open.
func TestAVersion2DocumentReadsThroughFromLegacy(t *testing.T) {
	root := withTestRoot(t)
	path := root.ProxySecretsFile(testProjectID, testPoolID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"version":2,"clients":{"sb-1":["SENT","SENT-BAD-HOST"]},"routes":{"sb-1":{` +
		`"SENT":[` +
		`[{"host":"*"}],` +
		`[{"host":"api.github.com","path":"/repos/acme/"},` +
		`{"host":"api.github.com","path":"/repos/acme/web/*","blocked":true},` +
		`{"host":"api.github.com","path":"/orgs/*"}]],` +
		`"SENT-BAD-HOST":[[{"host":"github.com"},{"host":"git*hub.com","path":"/x*","blocked":true}]]` +
		`}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readSecretsDoc(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := [][]secretroute.Route{
		parsedRoutes(t, "*"),
		parsedRoutes(t,
			"api.github.com,*.api.github.com/repos/acme/**",
			"api.github.com,*.api.github.com/repos/acme/web/**=blocked",
		),
	}
	if got := doc.Routes["sb-1"]["SENT"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}
	if got := doc.Routes["sb-1"]["SENT-BAD-HOST"]; !reflect.DeepEqual(got, [][]secretroute.Route{parsedRoutes(t, "*=blocked")}) {
		t.Fatalf("a list with an unreadable blocked route = %+v, want one refusing every request", got)
	}

	r := &secretResolver{}
	r.setSentinelRoutes(doc.Routes)
	for _, tc := range []struct {
		sentinel, url string
	}{
		{"SENT", "https://api.github.com/repos/acme/web/settings"},
		{"SENT", "https://api.github.com/repos/acme/web/*/x"},
		{"SENT", "https://api.github.com/orgs/acme"},
		{"SENT-BAD-HOST", "https://github.com/"},
	} {
		host, _ := strings.CutPrefix(tc.url, "https://")
		host, _, _ = strings.Cut(host, "/")
		if verdict, _ := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
			ClientID: "sb-1", Sentinels: []string{tc.sentinel}, Method: http.MethodGet, Host: host, URL: tc.url,
		}); verdict.Allow {
			t.Fatalf("%s to %s was allowed after a legacy route it fell under could not be read", tc.sentinel, tc.url)
		}
	}

	// Rewritten by this pool, the document is version 3 and reads the same.
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-2", []string{"OTHER"}, nil); err != nil {
		t.Fatal(err)
	}
	if doc, err = readSecretsDoc(path); err != nil || doc.Version != secretsDocVersion || !reflect.DeepEqual(doc.Routes["sb-1"]["SENT"], want) {
		t.Fatalf("after a rewrite: version %d, routes = %+v (%v), want %+v", doc.Version, doc.Routes["sb-1"]["SENT"], err, want)
	}
}

// A route is written normalized, and one that does not normalize fails
// closed: an open one is left out, and a blocked one refuses its whole list,
// since no narrower block can be known to cover what it did.
func TestUpsertSandboxSentinelsHoldsAnUnreadableRouteClosed(t *testing.T) {
	root := withTestRoot(t)
	routes := map[string][][]secretroute.Route{"SENT": {
		{
			{Hosts: []string{"API.GitHub.com"}, Methods: []string{"get"}},
			{Hosts: []string{"api.github.com"}, Paths: []string{"no-slash"}},
		},
		{
			{Hosts: []string{"uploads.github.com"}},
			{Hosts: []string{"uploads.github.com"}, Paths: []string{"no-slash"}, Blocked: true},
		},
	}}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", []string{"SENT"}, routes); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	doc, err := readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]secretroute.Route{parsedRoutes(t, "GET api.github.com"), parsedRoutes(t, "*=blocked")}
	if got := doc.Routes["sb-1"]["SENT"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("routes = %+v, want %+v", got, want)
	}
}

// A route's protocol block is matched against the request body the proxy
// hands the authorizer: a GraphQL query is allowed and a mutation refused
// under a route to queries alone, and a body that cannot be read matches no
// open route.
func TestAuthorizeMatchesAProtocolBlockOnTheBody(t *testing.T) {
	r := &secretResolver{}
	r.setSentinelRoutes(map[string]map[string][][]secretroute.Route{"sb-1": {
		"SENT": {parsedRoutes(t, "api.x.test/graphql graphql=query")},
	}})
	authorize := func(body func() ([]byte, bool)) bool {
		t.Helper()
		verdict, err := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
			ClientID: "sb-1", Sentinels: []string{"SENT"}, Method: http.MethodPost,
			Host: "api.x.test", URL: "https://api.x.test/graphql", Body: body,
		})
		if err != nil {
			t.Fatal(err)
		}
		return verdict.Allow
	}
	body := func(text string) func() ([]byte, bool) {
		return func() ([]byte, bool) { return []byte(text), true }
	}
	if !authorize(body(`{"query":"query { viewer { login } }"}`)) {
		t.Fatal("a query was refused under a route to queries")
	}
	if authorize(body(`{"query":"mutation { deleteRepository(input: {}) { clientMutationId } }"}`)) {
		t.Fatal("a mutation was allowed under a route to queries")
	}
	if authorize(func() ([]byte, bool) { return nil, false }) {
		t.Fatal("a body past the limit was allowed under a protocol route")
	}
	if authorize(nil) {
		t.Fatal("no body was allowed under a protocol route")
	}
}

// The authorizer holds a request to the method its headers name besides its
// request line: a POST overriding itself to DELETE falls on the route that
// blocks DELETE.
func TestAuthorizeReadsAMethodOverride(t *testing.T) {
	r := &secretResolver{}
	r.setSentinelRoutes(map[string]map[string][][]secretroute.Route{"sb-1": {
		"SENT": {parsedRoutes(t, "api.x.test/**", "DELETE api.x.test/repos/a=blocked")},
	}})
	authorize := func(header http.Header) bool {
		t.Helper()
		verdict, err := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
			ClientID: "sb-1", Sentinels: []string{"SENT"}, Method: http.MethodPost,
			Host: "api.x.test", URL: "https://api.x.test/repos/a", Header: header,
			Body: func() ([]byte, bool) { return []byte(`{}`), true },
		})
		if err != nil {
			t.Fatal(err)
		}
		return verdict.Allow
	}
	if authorize(http.Header{"Content-Type": {"application/json"}, "X-Http-Method-Override": {"DELETE"}}) {
		t.Fatal("a POST overridden to DELETE was allowed under a route blocking DELETE")
	}
	if !authorize(http.Header{"Content-Type": {"application/json"}}) {
		t.Fatal("a plain JSON POST was refused")
	}
}

// A pushed list none of whose routes normalizes refuses every request rather
// than reading as no list.
func TestHeldListOfNothingReadableRefuses(t *testing.T) {
	got := heldList([]secretroute.Route{{Hosts: []string{"api.github.com"}, Paths: []string{"no-slash"}}})
	if !reflect.DeepEqual(got, secretroute.RefusingList()) {
		t.Fatalf("heldList = %+v, want RefusingList", got)
	}
}
