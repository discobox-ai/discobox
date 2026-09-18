package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/layout"

	"github.com/discobox-ai/discobox/proxy"
)

func TestUpsertAndRemoveSentinels(t *testing.T) {
	withTestRoot(t)

	if err := UpsertSandboxSentinels(testProjectID, testPoolID, "sb-1", []string{"SENT-A", "SENT-B"}); err != nil {
		t.Fatalf("upsert sb-1: %v", err)
	}
	if err := UpsertSandboxSentinels(testProjectID, testPoolID, "sb-2", []string{"SENT-C"}); err != nil {
		t.Fatalf("upsert sb-2: %v", err)
	}

	doc, err := readSecretsDoc(layout.ProxySecretsFile(testProjectID, testPoolID))
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if len(doc.Clients["sb-1"]) != 2 || doc.Clients["sb-2"][0] != "SENT-C" {
		t.Fatalf("unexpected doc: %#v", doc.Clients)
	}

	if err := RemoveSandboxSentinels(testProjectID, testPoolID, "sb-1"); err != nil {
		t.Fatalf("remove sb-1: %v", err)
	}
	doc, _ = readSecretsDoc(layout.ProxySecretsFile(testProjectID, testPoolID))
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
	withTestRoot(t)
	var gotAuth, gotPath string
	var gotBody resolveRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		exp := time.Now().Add(time.Hour)
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "approved", Value: "real-secret", ExpiresAt: &exp})
	}))
	defer srv.Close()

	if err := WriteResolveContext(testProjectID, testPoolID, srv.URL, "tok-123"); err != nil {
		t.Fatalf("write context: %v", err)
	}
	resolver := newSecretResolver(testProjectID, testPoolID, newActivations())
	res, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "api.example.com"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Value != "real-secret" {
		t.Fatalf("value = %q", res.Value)
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
	withTestRoot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "pending"})
	}))
	defer srv.Close()
	_ = WriteResolveContext(testProjectID, testPoolID, srv.URL, "tok")

	resolver := newSecretResolver(testProjectID, testPoolID, newActivations())
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied", err)
	}
}

func TestResolverNoContextIsDenied(t *testing.T) {
	withTestRoot(t)
	resolver := newSecretResolver(testProjectID, testPoolID, newActivations())
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied when no context file", err)
	}
}

// The activation's host is a scope, not a string: a use approved for the site
// covers the hosts beneath it, and covers nothing above it. It has to read the
// same way here as it does on the control plane — a sentinel the pool agent
// forwards and the control plane then refuses is a credential that fails
// halfway, with the reason on the other side of the wire.
func TestActivationHostCoversSubdomainsAndNothingAbove(t *testing.T) {
	withTestRoot(t)
	live := newActivations()
	resolver := newSecretResolver(testProjectID, testPoolID, live)

	record, err := live.mint("sb-1", "STABLE", "use-1", "github.com", "ghp_{base62:36}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := resolver.activation(proxy.SecretResolveRequest{
		ClientID: "sb-1", Sentinel: record.Sentinel, Host: "api.github.com",
	}); !ok {
		t.Fatal("a use approved for the site does not cover its API")
	}

	narrow, err := live.mint("sb-1", "STABLE", "use-2", "api.github.com", "ghp_{base62:36}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := resolver.activation(proxy.SecretResolveRequest{
		ClientID: "sb-1", Sentinel: narrow.Sentinel, Host: "github.com",
	}); ok {
		t.Fatal("a use approved for the API covers the whole site")
	}
	if _, ok := resolver.activation(proxy.SecretResolveRequest{
		ClientID: "sb-1", Sentinel: narrow.Sentinel, Host: "uploads.github.com",
	}); ok {
		t.Fatal("a use approved for one host covers a sibling")
	}
}

// A report names the stable sentinel and the use the ephemeral one was minted
// for, so the control plane can attribute a refused credential without ever
// learning that ephemeral sentinels exist (ADR 0031 §3, ADR 0132 §2).
func TestReportTranslatesAnEphemeralSentinel(t *testing.T) {
	withTestRoot(t)
	var got rejectionRequestBody
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_ = WriteResolveContext(testProjectID, testPoolID, srv.URL, "tok")

	live := newActivations()
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.example.com", "", []string{"curl"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resolver := newSecretResolver(testProjectID, testPoolID, live)
	err = resolver.Report(context.Background(), proxy.SecretReportRequest{
		ClientID: "sb-1", Sentinel: record.Sentinel, Host: "api.example.com", Outcome: proxy.SecretRejected,
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if gotPath != "/api/pools/"+testPoolID+"/sandbox-secret-rejections" {
		t.Fatalf("path = %q", gotPath)
	}
	if got.Sentinel != "STABLE-SENTINEL" {
		t.Fatalf("sentinel = %q, want the stable one the control plane knows", got.Sentinel)
	}
	if got.UseID != "use-1" {
		t.Fatalf("useId = %q, want the use the sentinel was minted for", got.UseID)
	}
}

// A report is necessarily late — it crosses the response path, a queue and a
// cooldown — so the activation it names has usually lapsed and been swept by
// the time it goes out. Translating it is what keeps the ephemeral string out
// of the control plane, where a resolve of the same sentinel is refused.
func TestReportTranslatesALapsedActivation(t *testing.T) {
	withTestRoot(t)
	var got rejectionRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_ = WriteResolveContext(testProjectID, testPoolID, srv.URL, "tok")

	now := time.Now()
	live := newActivations()
	live.now = func() time.Time { return now }
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.example.com", "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Past its TTL, and swept: the state a report normally finds.
	now = now.Add(activationTTL + time.Minute)
	live.sweep()
	if _, stillLive := live.lookup(record.Sentinel); stillLive {
		t.Fatal("the activation still authorizes a swap after its TTL")
	}

	resolver := newSecretResolver(testProjectID, testPoolID, live)
	if err := resolver.Report(context.Background(), proxy.SecretReportRequest{
		ClientID: "sb-1", Sentinel: record.Sentinel, Host: "api.example.com", Outcome: proxy.SecretRejectedAfterRetry,
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if got.Sentinel != "STABLE-SENTINEL" {
		t.Fatalf("sentinel = %q, want the stable one — never the ephemeral string", got.Sentinel)
	}

	// And past the grace it is forgotten entirely, which is the point at which
	// an ephemeral sentinel is indistinguishable from an ordinary one.
	now = now.Add(activationReportGrace + time.Minute)
	live.sweep()
	if _, known := live.lookupAny(record.Sentinel); known {
		t.Fatal("a lapsed activation is remembered past its reporting grace")
	}
}

// A sentinel this process minted for another sandbox is not this one's to
// report, the same rule a resolve of it follows.
func TestReportRefusesAnotherSandboxesSentinel(t *testing.T) {
	withTestRoot(t)
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	_ = WriteResolveContext(testProjectID, testPoolID, srv.URL, "tok")

	live := newActivations()
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.example.com", "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resolver := newSecretResolver(testProjectID, testPoolID, live)
	if err := resolver.Report(context.Background(), proxy.SecretReportRequest{
		ClientID: "sb-2", Sentinel: record.Sentinel, Host: "api.example.com", Outcome: proxy.SecretRejected,
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if called {
		t.Fatal("a report was sent for a sentinel minted for another sandbox")
	}
}
