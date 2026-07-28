package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/obot-platform/discobox/layout"

	"github.com/obot-platform/discobox/proxy"
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

	clients := secretClientsFromDoc(doc)
	if len(clients) != 1 || clients[0].ClientID != "sb-2" {
		t.Fatalf("secretClientsFromDoc = %#v", clients)
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
	resolver := newSecretResolver(testProjectID, testPoolID)
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

	resolver := newSecretResolver(testProjectID, testPoolID)
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied", err)
	}
}

func TestResolverNoContextIsDenied(t *testing.T) {
	withTestRoot(t)
	resolver := newSecretResolver(testProjectID, testPoolID)
	_, err := resolver.Resolve(context.Background(), proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: "SENT", Host: "h"})
	if !errors.Is(err, proxy.ErrSecretResolveDenied) {
		t.Fatalf("err = %v, want ErrSecretResolveDenied when no context file", err)
	}
}
