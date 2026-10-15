package proxyagent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/proxy/egress"
)

// A sandbox's recorded egress policy reaches the running proxy through the
// policy file, restricts that sandbox alone, and is lifted when it is.
func TestSandboxEgressRestrictsOnlyThatSandbox(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := withTestRoot(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	restricted, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	open, err := proxy.EnsureClientCertificate(bundle, "sb-2", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = filepath.Join(root.Dir(), testProjectID+"-audit.db")
	cfg.Recording.Enabled = false
	server, err := proxy.NewServer(ctx, cfg, bundle, nil)
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	policy := newPolicyPublisher(server, cfg, newActivations(), func(err error) { t.Errorf("apply proxy config: %v", err) })
	go func() { _ = server.ListenAndServe() }()
	restrictedClient := sandboxClient(t, server, restricted)
	openClient := sandboxClient(t, server, open)

	apply := func(p egress.Policy) {
		t.Helper()
		if err := SetSandboxEgress(root, testProjectID, testPoolID, "sb-1", p); err != nil {
			t.Fatal(err)
		}
		doc, err := readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
		if err != nil {
			t.Fatal(err)
		}
		policy.setFile(doc)
	}

	apply(egress.Policy{DenyByDefault: true})
	if resp := mustGet(t, restrictedClient, origin.URL); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restricted, nothing allowed: status %d", resp.StatusCode)
	}
	if resp := mustGet(t, openClient, origin.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("another sandbox: status %d", resp.StatusCode)
	}

	apply(egress.Policy{DenyByDefault: true, Allow: []string{"127.0.0.1"}})
	if resp := mustGet(t, restrictedClient, origin.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("restricted, origin allowed: status %d", resp.StatusCode)
	}

	apply(egress.Policy{DenyByDefault: true, Allow: []string{"example.com"}})
	if resp := mustGet(t, restrictedClient, origin.URL); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("restricted, origin no longer allowed: status %d", resp.StatusCode)
	}

	// An allow list without denyByDefault is no restriction.
	apply(egress.Policy{Allow: []string{"example.com"}})
	if resp := mustGet(t, restrictedClient, origin.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("unrestricted again: status %d", resp.StatusCode)
	}

	apply(egress.Policy{DenyByDefault: true})
	if err := RemoveSandboxPolicy(root, testProjectID, testPoolID, "sb-1"); err != nil {
		t.Fatal(err)
	}
	doc, err := readSecretsDoc(root.ProxySecretsFile(testProjectID, testPoolID))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Egress["sb-1"]; ok {
		t.Fatal("removing the sandbox's policy left its egress entry")
	}
}
