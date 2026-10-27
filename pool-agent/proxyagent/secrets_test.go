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

// A request is held to the routes of every sentinel it carries: off every
// route, or on a blocked one, it is refused with a reason; a sentinel whose
// secret has no routes holds it to nothing here (ADR 26-10-02-924,
// ADR 26-10-27-880).
func TestAuthorizeHoldsARequestToItsSentinelsRoutes(t *testing.T) {
	r := &secretResolver{}
	r.setSentinelRoutes(map[string]map[string][]secretroute.Route{"sb-1": {
		"SENT-COPILOT": {
			{Host: "githubcopilot.com"},
			{Host: "githubcopilot.com", Path: "/mcp", Blocked: true},
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
		{"a sentinel with no routes", []string{"SENT-PLAIN"}, "https://api.github.com/user", true},
		{"one sentinel off its routes refuses the request", []string{"SENT-PLAIN", "SENT-COPILOT"}, "https://api.github.com/user", false},
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
	// Another sandbox's routes are not this one's.
	if verdict, _ := r.Authorize(context.Background(), proxy.SecretAuthorizeRequest{
		ClientID: "sb-2", Sentinels: []string{"SENT-COPILOT"}, Host: "api.github.com", URL: "https://api.github.com/user",
	}); !verdict.Allow {
		t.Fatalf("sb-2 was held to sb-1's routes: %+v", verdict)
	}
}

// Routes are written only for sentinels the sandbox holds, and replaced with
// its sentinel set.
func TestUpsertSandboxSentinelsKeepsRoutesOfHeldSentinels(t *testing.T) {
	root := withTestRoot(t)
	routes := map[string][]secretroute.Route{
		"SENT-A":    {{Host: "github.com", Blocked: true}},
		"SENT-GONE": {{Host: "example.com"}},
	}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", []string{"SENT-A", "SENT-B"}, routes); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	doc, err := readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if got := doc.Routes["sb-1"]; len(got) != 1 || len(got["SENT-A"]) != 1 || !got["SENT-A"][0].Blocked {
		t.Fatalf("routes = %#v, want only SENT-A's", doc.Routes)
	}
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", []string{"SENT-A"}, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	doc, _ = readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if _, ok := doc.Routes["sb-1"]; ok {
		t.Fatalf("routes = %#v, want them replaced with the set", doc.Routes)
	}
}

// While the sentinel set moves, no sentinel reads as having no routes: one
// arriving has its routes before it is swappable, and one leaving keeps its
// own until it is gone.
func TestStagedRoutesCoverSentinelsArrivingAndLeaving(t *testing.T) {
	r := &secretResolver{}
	leaving := []secretroute.Route{{Host: "github.com", Path: "/old"}}
	r.setSentinelRoutes(map[string]map[string][]secretroute.Route{"sb-1": {"SENT-OLD": leaving}})

	arriving := []secretroute.Route{{Host: "githubcopilot.com"}}
	next := map[string]map[string][]secretroute.Route{"sb-1": {"SENT-NEW": arriving}}
	r.stageSentinelRoutes(next)
	if got := r.heldRoutes("sb-1", "SENT-NEW"); len(got) != 1 || got[0].Host != "githubcopilot.com" {
		t.Fatalf("arriving routes = %#v, want them staged before the sentinel set moves", got)
	}
	if got := r.heldRoutes("sb-1", "SENT-OLD"); len(got) != 1 || got[0].Path != "/old" {
		t.Fatalf("leaving routes = %#v, want them kept until the sentinel set has moved", got)
	}
	if _, ok := next["sb-1"]["SENT-OLD"]; ok {
		t.Fatal("staging wrote the leaving sentinel's routes into the caller's map")
	}

	r.setSentinelRoutes(next)
	if got := r.heldRoutes("sb-1", "SENT-OLD"); len(got) != 0 {
		t.Fatalf("leaving routes = %#v, want them dropped once the set has moved", got)
	}
}

// A route a pool agent from before ADR 26-10-27-880 persisted with a use — one
// a judge read — reads back blocked, so the upgrade never sends a request on
// it unjudged; a route without one reads as it was written.
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
	want := []secretroute.Route{
		{Host: "githubcopilot.com"},
		{Host: "githubcopilot.com", Path: "/mcp", Blocked: true},
		{Host: "github.com", Blocked: true},
	}
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
	// Written back by this pool, it keeps saying so.
	if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-2", []string{"OTHER"}, nil); err != nil {
		t.Fatal(err)
	}
	if doc, err = readSecretsDoc(path); err != nil || !reflect.DeepEqual(doc.Routes["sb-1"]["SENT"], want) {
		t.Fatalf("after a rewrite: routes = %+v (%v), want %+v", doc.Routes["sb-1"]["SENT"], err, want)
	}
}
