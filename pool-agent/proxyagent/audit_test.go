package proxyagent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/proxy"
)

// A sandbox reads back the request the proxy refused it, with the reason and
// with no live sentinel of its own in it, and another sandbox on the same pool
// reads none of it (ADR 26-10-02-678 §2).
func TestAuditHTTPReadsOnlyTheCallersRefusals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := withTestRoot(t)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()

	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("prepare bundle: %v", err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatalf("issue client certificate: %v", err)
	}
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = filepath.Join(root.Dir(), testProjectID+"-audit.db")
	cfg.Recording.StreamDir = t.TempDir()
	cfg.Recording.BodyDir = t.TempDir()
	live := newActivations()
	server, err := proxy.NewServer(ctx, cfg, bundle, newSecretResolver(root, testProjectID, testPoolID, live))
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.ListenAndServe() }()

	// A sentinel minted for a command, which the command put in a URL: the
	// proxy records the URL as sent, and swaps nothing in one.
	minted, err := live.mint("sb-1", "stable-sentinel", "use_1", nil, []string{"example.test"}, "", []string{"curl"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// The origin's certificate chains to a CA nothing trusts, so the proxy
	// refuses the request and records why.
	client := sandboxClient(t, server, material)
	if resp := mustGet(t, client, origin.URL+"/refused?token="+minted.Sentinel); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want the proxy's 502", resp.StatusCode)
	}

	mine := &credentialBroker{sandboxID: "sb-1", proxy: server, activations: live}
	blocked := true
	var exchanges []agentcreds.AuditExchange
	deadline := time.Now().Add(5 * time.Second)
	for len(exchanges) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the refused request was never recorded")
		}
		time.Sleep(20 * time.Millisecond)
		if exchanges, err = mine.AuditHTTP(ctx, agentcreds.AuditQuery{Blocked: &blocked, Limit: agentcreds.DefaultAuditLimit}); err != nil {
			t.Fatalf("audit: %v", err)
		}
	}
	got := exchanges[0]
	if !got.Blocked || got.BlockedReason == "" || !strings.HasPrefix(got.ID, "http_") || got.Status != http.StatusBadGateway {
		t.Fatalf("exchange %+v, want a blocked 502 with a reason and an http_ id", got)
	}
	if strings.Contains(got.URL, minted.Sentinel) || !strings.HasSuffix(got.URL, "token="+auditRedacted) {
		t.Fatalf("url %q, want the sentinel redacted", got.URL)
	}

	other := &credentialBroker{sandboxID: "sb-2", proxy: server, activations: live}
	if theirs, err := other.AuditHTTP(ctx, agentcreds.AuditQuery{Limit: agentcreds.MaxAuditLimit}); err != nil || len(theirs) != 0 {
		t.Fatalf("another sandbox read %d exchanges (err %v), want none", len(theirs), err)
	}
	allowed := false
	if passed, err := mine.AuditHTTP(ctx, agentcreds.AuditQuery{Blocked: &allowed, Limit: agentcreds.DefaultAuditLimit}); err != nil || len(passed) != 0 {
		t.Fatalf("read %d let-through exchanges (err %v), want none", len(passed), err)
	}
}

func TestTruncateAuditFieldCutsOnARune(t *testing.T) {
	long := strings.Repeat("a", auditFieldBytes-1) + "é" + "tail"
	got := truncateAuditField(long)
	if !strings.HasSuffix(got, "…") || strings.Contains(got, "é") || len(got) > auditFieldBytes+len("…") {
		t.Fatalf("truncated to %d bytes ending %q", len(got), got[len(got)-8:])
	}
	if short := "https://example.test/"; truncateAuditField(short) != short {
		t.Fatalf("a short field was changed")
	}
}

// A sentinel is redacted however a program encoded it into a URL, and a URL
// with none in it is returned as recorded.
func TestSentinelRedactorFindsEveryEncoding(t *testing.T) {
	const sentinel = "ab+c/d"
	redact := newSentinelRedactor([]string{sentinel})
	for in, want := range map[string]string{
		"https://h.test/p?token=ab+c/d":         "https://h.test/p?token=" + auditRedacted,
		"https://h.test/p?token=ab%2Bc%2Fd":     "https://h.test/p?token=" + auditRedacted,
		"https://h.test/p?token=ab%2bc%2fd&x=1": "https://h.test/p?token=" + auditRedacted + "&x=1",
		"https://h.test/p?token=ab%2Bc%2fd":     "https://h.test/p?token=" + auditRedacted,
		"https://h.test/keys/ab%2Bc%2Fd/info":   "https://h.test/keys/" + auditRedacted + "/info",
		"https://h.test/p?ab%2Bc%2Fd":           "https://h.test/p?" + auditRedacted,
		"https://user@h.test/a%20b?q=%7E#frag":  "https://user@h.test/a%20b?q=%7E#frag",
		"h.test:443":                            "h.test:443",
	} {
		if got := redact.url(in); got != want {
			t.Errorf("url(%q) = %q, want %q", in, got, want)
		}
	}
	if got := redact.text("judge: refused ab%2bc%2fd"); got != "judge: refused "+auditRedacted {
		t.Errorf("text = %q", got)
	}
	if got := newSentinelRedactor(nil).url("https://h.test/?t=ab+c/d"); got != "https://h.test/?t=ab+c/d" {
		t.Errorf("no sentinels changed the url to %q", got)
	}
}
