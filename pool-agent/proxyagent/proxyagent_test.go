package proxyagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestEnsureSandboxMaterialStagesClientOnly(t *testing.T) {
	root := withTestRoot(t)

	material, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, ) error = %v", err)
	}

	if want := filepath.Join(PoolSandboxMaterialRoot(root, "project-1", "pool-1"), "sandbox-1"); material.MountSource != want {
		t.Fatalf("MountSource = %q, want %q", material.MountSource, want)
	}

	dir := material.MountSource
	for _, name := range []string{"mtls-ca.crt", "mitm-ca.crt", "client.crt", "client.key", "bridge.json", "bridge-docker.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected staged file %q: %v", name, err)
		}
	}

	// The CA private keys must never be exposed to a sandbox.
	for _, leaked := range []string{"mtls-ca.key", "mitm-ca.key"} {
		if _, err := os.Stat(filepath.Join(dir, leaked)); !os.IsNotExist(err) {
			t.Fatalf("CA private key %q must not be staged into sandbox material", leaked)
		}
	}

	if got := material.Env["HTTP_PROXY"]; got != "http://"+SandboxForwarderListen {
		t.Fatalf("HTTP_PROXY = %q, want %q", got, "http://"+SandboxForwarderListen)
	}
	// Node.js/Claude Code, Python/requests, and pip all bundle their own root
	// store, so each points at the system bundle the boot-time trust step
	// augments — not the raw MITM CA file, so a nested Docker container gets
	// the identical value working once its runc wrapper mounts the same
	// bundle at the same path (docs/adr/0020).
	if got := material.Env["NODE_EXTRA_CA_CERTS"]; got != SystemCABundle {
		t.Fatalf("NODE_EXTRA_CA_CERTS = %q, want system CA bundle", got)
	}
	if got := material.Env["SSL_CERT_FILE"]; got != SystemCABundle {
		t.Fatalf("SSL_CERT_FILE = %q, want system CA bundle", got)
	}
	if got := material.Env["REQUESTS_CA_BUNDLE"]; got != SystemCABundle {
		t.Fatalf("REQUESTS_CA_BUNDLE = %q, want system CA bundle", got)
	}
	if got := material.Env["PIP_CERT"]; got != SystemCABundle {
		t.Fatalf("PIP_CERT = %q, want system CA bundle", got)
	}
}

// The sandbox's DNS stub reads these two fields from bridge.json: where to
// listen (the DNS server its container was created with) and where to dial.
func TestEnsureSandboxMaterialStagesDNS(t *testing.T) {
	root := withTestRoot(t)

	material, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, ) error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(material.MountSource, "bridge.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bridge struct {
		DNSServer        string `json:"dnsServer"`
		DNSListenAddress string `json:"dnsListenAddress"`
	}
	if err := json.Unmarshal(data, &bridge); err != nil {
		t.Fatal(err)
	}
	if bridge.DNSServer != "discobox-pool-proxy:17085" {
		t.Fatalf("dnsServer = %q", bridge.DNSServer)
	}
	if bridge.DNSListenAddress != "169.254.53.53:53" {
		t.Fatalf("dnsListenAddress = %q", bridge.DNSListenAddress)
	}
}

// A Docker pool's sandboxes reach its services over TCP by the pool's DNS
// name, so every bridge config names an https URL. The server name is stated
// beside it because it is what a vsock or unix URL could not carry, and it is
// the name the pool's one server certificate is issued for.
func TestEnsureSandboxMaterialStagesPoolEndpoints(t *testing.T) {
	root := withTestRoot(t)

	material, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}
	for file, want := range map[string]struct{ pool, creds string }{
		"bridge.json":          {"https://discobox-pool-proxy:17080", "https://discobox-pool-proxy:17083"},
		"bridge-docker.json":   {"https://discobox-pool-proxy:17080", ""},
		"bridge-buildkit.json": {"https://discobox-pool-proxy:17081", ""},
	} {
		data, err := os.ReadFile(filepath.Join(material.MountSource, file))
		if err != nil {
			t.Fatal(err)
		}
		var got bridgeConfig
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.PoolProxyURL != want.pool || got.CredentialsURL != want.creds {
			t.Fatalf("%s: workerProxyUrl = %q, credentialsUrl = %q; want %q, %q", file, got.PoolProxyURL, got.CredentialsURL, want.pool, want.creds)
		}
		if got.ServerName != ServerName {
			t.Fatalf("%s: serverName = %q, want %q", file, got.ServerName, ServerName)
		}
	}
}

func TestRemoveSandboxMaterialDeletesStagedFilesAndClientCert(t *testing.T) {
	root := withTestRoot(t)

	material, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, ) error = %v", err)
	}
	materialDir := material.MountSource
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

	live, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "live-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, live) error = %v", err)
	}
	orphan, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "orphan-sandbox")
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

	if _, err := os.Stat(live.MountSource); err != nil {
		t.Fatalf("live sandbox material should be kept: %v", err)
	}
	if _, err := os.Stat(orphan.MountSource); !os.IsNotExist(err) {
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

	poolBMaterial, err := EnsureSandboxMaterial(root, "project-1", "pool-b", "sandbox-b")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, pool-b) error = %v", err)
	}
	// Age it past any grace window so only scoping — not the grace period —
	// protects it.
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(poolBMaterial.MountSource, past, past)

	// Pool A prunes with an empty live set: it must not see pool B's material.
	if err := PruneOrphanedMaterial(root, "project-1", "pool-a", nil, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(root, pool-a) error = %v", err)
	}
	if _, err := os.Stat(poolBMaterial.MountSource); err != nil {
		t.Fatalf("pool A reaped pool B's material: %v", err)
	}
}

// Pool IDs are unique per project, so a prune driven by one project's
// authoritative pool set must not reach another project's material even when
// both projects host a pool of the same name on a shared daemon.
func TestPruneOrphanedMaterialIsProjectScoped(t *testing.T) {
	root := withTestRoot(t)

	otherProject, err := EnsureSandboxMaterial(root, "project-2", "pool-1", "sandbox-b")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, project-2) error = %v", err)
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(otherProject.MountSource, past, past)

	if err := PruneOrphanedMaterial(root, "project-1", "pool-1", nil, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(root, project-1) error = %v", err)
	}
	if _, err := os.Stat(otherProject.MountSource); err != nil {
		t.Fatalf("project 1 reaped project 2's material: %v", err)
	}
}

func TestPruneOrphanedMaterialProtectsFreshMaterial(t *testing.T) {
	root := withTestRoot(t)

	fresh, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "fresh-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(root, ) error = %v", err)
	}

	// A recently staged orphan (mid-CreateSandbox) must survive the grace window.
	if err := PruneOrphanedMaterial(root, "project-1", "pool-1", nil, time.Hour); err != nil {
		t.Fatalf("PruneOrphanedMaterial(root, ) error = %v", err)
	}
	if _, err := os.Stat(fresh.MountSource); err != nil {
		t.Fatalf("fresh material should be protected by grace period: %v", err)
	}
}

func TestEnsureSandboxMaterialReusesClientCertificate(t *testing.T) {
	root := withTestRoot(t)

	first, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("first EnsureSandboxMaterial(root, ) error = %v", err)
	}
	firstCert, err := os.ReadFile(filepath.Join(first.MountSource, "client.crt"))
	if err != nil {
		t.Fatal(err)
	}

	second, err := EnsureSandboxMaterial(root, "project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("second EnsureSandboxMaterial(root, ) error = %v", err)
	}
	secondCert, err := os.ReadFile(filepath.Join(second.MountSource, "client.crt"))
	if err != nil {
		t.Fatal(err)
	}

	if string(firstCert) != string(secondCert) {
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
