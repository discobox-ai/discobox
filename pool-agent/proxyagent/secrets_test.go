package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/proxy"
)

func tempResolver(t *testing.T) HostPathResolver {
	t.Helper()
	dir := t.TempDir()
	// Map the fixed /var/lib/discobox/proxy tree under a writable temp dir.
	return func(p string) string { return filepath.Join(dir, p) }
}

func TestUpsertAndRemoveSentinels(t *testing.T) {
	hostDirFor := tempResolver(t)

	if err := UpsertSandboxSentinels(hostDirFor, "sb-1", []string{"SENT-A", "SENT-B"}); err != nil {
		t.Fatalf("upsert sb-1: %v", err)
	}
	if err := UpsertSandboxSentinels(hostDirFor, "sb-2", []string{"SENT-C"}); err != nil {
		t.Fatalf("upsert sb-2: %v", err)
	}

	doc, err := readSecretsDoc(hostDirFor(SecretsFile))
	if err != nil {
		t.Fatalf("read doc: %v", err)
	}
	if len(doc.Clients["sb-1"]) != 2 || doc.Clients["sb-2"][0] != "SENT-C" {
		t.Fatalf("unexpected doc: %#v", doc.Clients)
	}

	if err := RemoveSandboxSentinels(hostDirFor, "sb-1"); err != nil {
		t.Fatalf("remove sb-1: %v", err)
	}
	doc, _ = readSecretsDoc(hostDirFor(SecretsFile))
	if _, ok := doc.Clients["sb-1"]; ok {
		t.Fatal("sb-1 should be removed")
	}
	if _, ok := doc.Clients["sb-2"]; !ok {
		t.Fatal("sb-2 should remain")
	}

	clients := secretClientsFromDoc(doc)
	if len(clients) != 1 || clients[0].ClientID != "sb-2" {
		t.Fatalf("secretClientsFromDoc = %#v", clients)
	}
}

func TestResolverApproved(t *testing.T) {
	hostDirFor := tempResolver(t)
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

	if err := WriteResolveContext(hostDirFor, srv.URL, "pool-1", "tok-123"); err != nil {
		t.Fatalf("write context: %v", err)
	}
	resolver := newSecretResolver(hostDirFor)
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
	if gotPath != "/api/pools/pool-1/resolve-sandbox-secret" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.SandboxID != "sb-1" || gotBody.Sentinel != "SENT" || gotBody.Host != "api.example.com" {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestResolverPendingIsDenied(t *testing.T) {
	hostDirFor := tempResolver(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "pending"})
	}))
	defer srv.Close()
	_ = WriteResolveContext(hostDirFor, srv.URL, "pool-1", "tok")

	resolver := newSecretResolver(hostDirFor)
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied", err)
	}
}

func TestResolverNoContextIsDenied(t *testing.T) {
	hostDirFor := tempResolver(t)
	resolver := newSecretResolver(hostDirFor)
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied when no context file", err)
	}
}
