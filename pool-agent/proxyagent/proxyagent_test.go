package proxyagent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxpath"
)

// materialRecord is the per-sandbox directory EnsureSandboxMaterial makes as
// the reaper's record of the sandbox.
func materialRecord(root layout.Root, projectID, poolID, sandboxID string) string {
	return filepath.Join(PoolSandboxMaterialRoot(root, projectID, poolID), sandboxID)
}

// The material is delivered in the sandbox's runtime-config document, not
// staged for it: what comes back is the sandbox's keypair, the pool's public
// CAs and its registry namespace, and nothing of the CAs' private keys
// (ADR 26-10-08-127 §1).
func TestEnsureSandboxMaterialDeliversClientOnly(t *testing.T) {
	root := withTestRoot(t)

	material, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, ) error = %v", err)
	}
	proxy := material.Proxy
	for name, pem := range map[string]string{"mtlsCa": proxy.MTLSCA, "mitmCa": proxy.MITMCA, "clientCert": proxy.ClientCert} {
		if !strings.Contains(pem, "BEGIN CERTIFICATE") || strings.Contains(pem, "PRIVATE KEY") {
			t.Fatalf("%s = %q, want a certificate and no private key", name, pem)
		}
	}
	if !strings.Contains(proxy.ClientKey, "PRIVATE KEY") {
		t.Fatal("the sandbox's client key is missing")
	}
	if proxy.RegistryNamespace == "" || strings.ContainsAny(proxy.RegistryNamespace, " \n") {
		t.Fatalf("registry namespace = %q", proxy.RegistryNamespace)
	}
	doc := sandboxconfig.RuntimeConfig{Revision: 1, Proxy: &proxy}
	if err := doc.Validate(sandboxpath.Paths{}); err != nil {
		t.Fatalf("the material does not make a valid document: %v", err)
	}
	// Nothing is staged for the sandbox to read; the directory is only the
	// reaper's record.
	entries, err := os.ReadDir(materialRecord(root, "project-1", "pool-1", "sandbox-1"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("material record = %v, %v; want an empty directory", entries, err)
	}

	if got := material.Env["HTTP_PROXY"]; got != "http://"+sandboxconfig.SandboxEgressListenAddress {
		t.Fatalf("HTTP_PROXY = %q, want %q", got, "http://"+sandboxconfig.SandboxEgressListenAddress)
	}
	// Node.js/Claude Code, Python/requests, and pip all bundle their own root
	// store, so each points at the system bundle the boot-time trust step
	// augments — not the raw MITM CA file, so a nested Docker container gets
	// the identical value working once its runc wrapper mounts the same
	// bundle at the same path (docs/adr/0020).
	for _, name := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "PIP_CERT"} {
		if got := material.Env[name]; got != SystemCABundle {
			t.Fatalf("%s = %q, want system CA bundle", name, got)
		}
	}
	// Node's built-in fetch ignores the proxy variables without it.
	if got := material.Env["NODE_USE_ENV_PROXY"]; got != "1" {
		t.Fatalf("NODE_USE_ENV_PROXY = %q, want 1", got)
	}
}

// A Docker pool's sandboxes reach its services over TCP by the pool's DNS
// name, which is also the name its one server certificate is issued for.
func TestPoolEndpointsNameThePoolsServices(t *testing.T) {
	got := PoolEndpoints()
	//nolint:gosec // G101: service URLs, not credentials.
	want := sandboxconfig.PoolEndpoints{
		Proxy:       "https://discobox-pool-proxy:17080",
		Credentials: "https://discobox-pool-proxy:17083",
		DNS:         "discobox-pool-proxy:17085",
		BuildKit:    "https://discobox-pool-proxy:17081",
		ServerName:  "discobox-pool-proxy",
	}
	if got != want {
		t.Fatalf("PoolEndpoints() = %+v, want %+v", got, want)
	}
}

func TestRemoveSandboxMaterialDeletesStagedFilesAndClientCert(t *testing.T) {
	root := withTestRoot(t)

	if _, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1"); err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, ) error = %v", err)
	}
	materialDir := materialRecord(root, "project-1", "pool-1", "sandbox-1")
	clientCertDir := filepath.Join(root.ProxyCerts("project-1", "pool-1"), "clients", "sandbox-1")
	for _, dir := range []string{materialDir, clientCertDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("expected %q to exist before removal: %v", dir, err)
		}
	}

	if err := RemoveSandboxMaterial(root, "project-1", "pool-1", "sandbox-1"); err != nil {
		t.Fatalf("RemoveSandboxMaterial(root, ) error = %v", err)
	}
	for _, dir := range []string{materialDir, clientCertDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("expected %q removed, stat err = %v", dir, err)
		}
	}

	// A repeated removal is a no-op.
	if err := RemoveSandboxMaterial(root, "project-1", "pool-1", "sandbox-1"); err != nil {
		t.Fatalf("second RemoveSandboxMaterial(root, ) error = %v", err)
	}
}

func TestPruneOrphanedMaterialRemovesOnlyOrphans(t *testing.T) {
	root := withTestRoot(t)

	_, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "live-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, live) error = %v", err)
	}
	_, err = EnsureSandboxMaterial(root, "project-1", "pool-1", "orphan-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, orphan) error = %v", err)
	}
	for _, id := range []string{"live-sandbox", "orphan-sandbox"} {
		if err := UpsertSandboxSentinels(root, "project-1", "pool-1", id, []string{"sk-" + id}); err != nil {
			t.Fatalf("UpsertSandboxSentinels(root, %s) error = %v", id, err)
		}
	}

	// Age both so the grace period does not protect the orphan.
	past := time.Now().Add(-time.Hour)
	for _, id := range []string{"live-sandbox", "orphan-sandbox"} {
		_ = os.Chtimes(filepath.Join(PoolSandboxMaterialRoot(root, "project-1", "pool-1"), id), past, past)
	}

	if err := PruneOrphanedMaterial(root, "project-1", "pool-1", []string{"live-sandbox"}, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(root, ) error = %v", err)
	}

	if _, err := os.Stat(materialRecord(root, "project-1", "pool-1", "live-sandbox")); err != nil {
		t.Fatalf("live sandbox material should be kept: %v", err)
	}
	if _, err := os.Stat(materialRecord(root, "project-1", "pool-1", "orphan-sandbox")); !os.IsNotExist(err) {
		t.Fatalf("orphan sandbox material should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root.ProxyCerts("project-1", "pool-1"), "clients", "orphan-sandbox")); !os.IsNotExist(err) {
		t.Fatalf("orphan client cert should be removed, stat err = %v", err)
	}

	doc, err := readSecretsDoc(root.ProxySecretsFile("project-1", "pool-1"))
	if err != nil {
		t.Fatalf("readSecretsDoc() error = %v", err)
	}
	if _, ok := doc.Clients["orphan-sandbox"]; ok {
		t.Fatal("orphan sentinel entry should be removed")
	}
	if _, ok := doc.Clients["live-sandbox"]; !ok {
		t.Fatal("live sentinel entry should be kept")
	}
}

// On a host daemon shared by two pools, pool A's prune (with only A's live set)
// must never touch pool B's material, even though B's sandbox is not in A's set.
func TestPruneOrphanedMaterialIsPoolScoped(t *testing.T) {
	root := withTestRoot(t)

	_, err := EnsureSandboxMaterial(root, "project-1", "pool-b", "sandbox-b")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, pool-b) error = %v", err)
	}
	// Age it past any grace window so only scoping — not the grace period —
	// protects it.
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(materialRecord(root, "project-1", "pool-b", "sandbox-b"), past, past)

	// Pool A prunes with an empty live set: it must not see pool B's material.
	if err := PruneOrphanedMaterial(root, "project-1", "pool-a", nil, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(root, pool-a) error = %v", err)
	}
	if _, err := os.Stat(materialRecord(root, "project-1", "pool-b", "sandbox-b")); err != nil {
		t.Fatalf("pool A reaped pool B's material: %v", err)
	}
}

// Pool IDs are unique per project, so a prune driven by one project's
// authoritative pool set must not reach another project's material even when
// both projects host a pool of the same name on a shared daemon.
func TestPruneOrphanedMaterialIsProjectScoped(t *testing.T) {
	root := withTestRoot(t)

	_, err := EnsureSandboxMaterial(root, "project-2", "pool-1", "sandbox-b")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, project-2) error = %v", err)
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(materialRecord(root, "project-2", "pool-1", "sandbox-b"), past, past)

	if err := PruneOrphanedMaterial(root, "project-1", "pool-1", nil, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(root, project-1) error = %v", err)
	}
	if _, err := os.Stat(materialRecord(root, "project-2", "pool-1", "sandbox-b")); err != nil {
		t.Fatalf("project 1 reaped project 2's material: %v", err)
	}
}

func TestPruneOrphanedMaterialProtectsFreshMaterial(t *testing.T) {
	root := withTestRoot(t)

	_, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "fresh-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, ) error = %v", err)
	}

	// A recently staged orphan (mid-CreateSandbox) must survive the grace window.
	if err := PruneOrphanedMaterial(root, "project-1", "pool-1", nil, time.Hour); err != nil {
		t.Fatalf("PruneOrphanedMaterial(root, ) error = %v", err)
	}
	if _, err := os.Stat(materialRecord(root, "project-1", "pool-1", "fresh-sandbox")); err != nil {
		t.Fatalf("fresh material should be protected by grace period: %v", err)
	}
}

func TestEnsureSandboxMaterialReusesClientCertificate(t *testing.T) {
	root := withTestRoot(t)

	first, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("first EnsureSandboxMaterial(root, ) error = %v", err)
	}
	firstCert := first.Proxy.ClientCert

	second, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("second EnsureSandboxMaterial(root, ) error = %v", err)
	}
	secondCert := second.Proxy.ClientCert

	if firstCert != secondCert {
		t.Fatal("client certificate was not reused across calls")
	}
}

// systemd resets the environment for units, so a proxy unit starts with none of
// the proxy-trust variables the surrounding sandbox injected into the pool
// container. Without forwarding them the inner proxy dials origins directly,
// which a sandbox has no route for. Only the proxy-trust subset is forwarded --
// the pool agent's wider environment is not the unit's business.
func TestUnitEnvironmentForwardsProxyTrustVars(t *testing.T) {
	got := proxyEnvironment([]string{
		"HTTPS_PROXY=http://172.30.0.1:17008",
		"NO_PROXY=127.0.0.1,localhost",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
		"PATH=/usr/bin",
		"AWS_SECRET_ACCESS_KEY=nope",
		"EMPTY_PROXY=",
	})

	for _, want := range []string{
		`HTTPS_PROXY="http://172.30.0.1:17008"`,
		`NO_PROXY="127.0.0.1,localhost"`,
		`SSL_CERT_FILE="/etc/ssl/certs/ca-certificates.crt"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"PATH=", "AWS_SECRET_ACCESS_KEY", "EMPTY_PROXY"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("forwarded %s, which is not proxy-trust material:\n%s", unwanted, got)
		}
	}
	// Sorted, so the unit file is byte-stable across restarts.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if !sort.StringsAreSorted(lines) {
		t.Fatalf("lines not sorted: %v", lines)
	}
}

// A pool with direct egress has no proxy vars, and must not gain empty ones.
func TestUnitEnvironmentOmitsAbsentProxyVars(t *testing.T) {
	if got := proxyEnvironment([]string{"PATH=/usr/bin"}); got != "" {
		t.Fatalf("expected nothing forwarded, got %q", got)
	}
}

func TestSandboxEnvironmentExemptsThePoolByName(t *testing.T) {
	root := withTestRoot(t)

	material, err := EnsureSandboxMaterial(root, "proj", "pool", "sbx_1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial: %v", err)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		// The pool must be exempt by name. The subnet token resolves to CIDRs,
		// and NO_PROXY matching never resolves a hostname to compare against
		// one, so without this a client addressing the pool by its alias is
		// routed through the sandbox's own egress proxy — which MITMs it and
		// serves a certificate signed by the MITM CA, where the caller expects
		// the mTLS CA.
		if !strings.Contains(material.Env[name], ServerName) {
			t.Errorf("%s does not exempt %s: %q", name, ServerName, material.Env[name])
		}
	}
}
