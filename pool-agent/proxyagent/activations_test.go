package proxyagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
)

// The resolver's activation path is where the ephemeral sentinel design either
// holds or leaks, so each of its rules gets its own test: translate a live one,
// refuse a stale one, refuse a live one aimed at the wrong host, and never let
// one sandbox spend another's activation.

func TestResolverTranslatesEphemeralToStableSentinel(t *testing.T) {
	withTestRoot(t)
	var gotSentinel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body resolveRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSentinel = body.Sentinel
		expiry := time.Now().Add(time.Hour)
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "approved", Value: "real-token", ExpiresAt: &expiry})
	}))
	defer server.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, server.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	live := newActivations()
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.github.com", "{alnum:12}", []string{"gh", "pr", "create"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resolver := newSecretResolver(testProjectID, testPoolID, live)

	result, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{
		ClientID: "sb-1", Sentinel: record.Sentinel, Host: "api.github.com",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if result.Value != "real-token" {
		t.Fatalf("value = %q, want the resolved credential", result.Value)
	}
	if gotSentinel != "STABLE-SENTINEL" {
		t.Fatalf("control plane saw sentinel %q; it must never learn that ephemeral sentinels exist", gotSentinel)
	}
	// The activation window is shorter than the grant, so it is what the proxy
	// may cache to.
	if result.ExpiresAt.After(record.ExpiresAt) {
		t.Fatalf("cache expiry %s outlives the activation window %s", result.ExpiresAt, record.ExpiresAt)
	}
}

func TestResolverRefusesEphemeralSentinelForAnotherHost(t *testing.T) {
	withTestRoot(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("control plane must not be asked about an activation used against the wrong host")
	}))
	defer server.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, server.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	live := newActivations()
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.github.com", "{alnum:12}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resolver := newSecretResolver(testProjectID, testPoolID, live)

	_, err = resolver.Resolve(context.Background(), proxy.SecretResolveRequest{
		ClientID: "sb-1", Sentinel: record.Sentinel, Host: "evil.example.com",
	})
	if err == nil {
		t.Fatal("resolve succeeded; an activation is scoped to the host its use was approved for")
	}
}

func TestResolverRefusesAnotherSandboxesActivation(t *testing.T) {
	withTestRoot(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("control plane must not be asked about another sandbox's activation")
	}))
	defer server.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, server.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	live := newActivations()
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.github.com", "{alnum:12}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	resolver := newSecretResolver(testProjectID, testPoolID, live)

	_, err = resolver.Resolve(context.Background(), proxy.SecretResolveRequest{
		ClientID: "sb-2", Sentinel: record.Sentinel, Host: "api.github.com",
	})
	if err == nil {
		t.Fatal("resolve succeeded; an activation belongs to the sandbox it was minted for")
	}
}

func TestResolverRefusesExpiredActivation(t *testing.T) {
	withTestRoot(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("control plane must not be asked about a lapsed activation")
	}))
	defer server.Close()
	if err := WriteResolveContext(testProjectID, testPoolID, server.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}

	live := newActivations()
	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.github.com", "{alnum:12}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Move the clock past the use window without sweeping, which is exactly the
	// state a request racing expiry finds.
	live.now = func() time.Time { return time.Now().Add(2 * activationTTL) }
	resolver := newSecretResolver(testProjectID, testPoolID, live)

	_, err = resolver.Resolve(context.Background(), proxy.SecretResolveRequest{
		ClientID: "sb-1", Sentinel: record.Sentinel, Host: "api.github.com",
	})
	if err == nil {
		t.Fatal("resolve succeeded; an activation must die with its window")
	}
}

// An ephemeral sentinel is useless unless the proxy is watching for it, and the
// proxy is only told about sentinels through the published set.
func TestMintPublishesTheSentinelToTheProxy(t *testing.T) {
	live := newActivations()
	published := 0
	live.setChangeHandler(func() { published++ })

	record, err := live.mint("sb-1", "STABLE-SENTINEL", "use-1", "api.github.com", "{alnum:12}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if published == 0 {
		t.Fatal("mint did not republish; the proxy would never match the sentinel it just handed out")
	}
	if sentinels := live.sentinelsByClient()["sb-1"]; len(sentinels) != 1 || sentinels[0] != record.Sentinel {
		t.Fatalf("published sentinels = %#v, want the freshly minted one", sentinels)
	}
}

// The sentinel has to look like the credential it stands in for, or a lookalike
// check upstream tells a sandbox which of its values are real.
func TestMintShapesTheSentinelLikeTheRealKey(t *testing.T) {
	live := newActivations()
	record, err := live.mint("sb-1", "STABLE", "use-1", "api.github.com", "ghp_{base62:36}", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(record.Sentinel, "ghp_") || len(record.Sentinel) != len("ghp_")+36 {
		t.Fatalf("sentinel = %q, want a GitHub-shaped value", record.Sentinel)
	}
}
