package proxyagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureSandboxMaterialStagesClientOnly(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	material, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}

	if want := filepath.Join(PoolSandboxMaterialRoot("project-1", "pool-1"), "sandbox-1"); material.MountSource != want {
		t.Fatalf("MountSource = %q, want %q", material.MountSource, want)
	}

	dir := resolver(material.MountSource)
	for _, name := range []string{"mtls-ca.crt", "mitm-ca.crt", "client.crt", "client.key", "bridge.json"} {
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
	// the identical value working once its NRI plugin mounts the same bundle
	// at the same path (docs/adr/0015).
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

func TestRemoveSandboxMaterialDeletesStagedFilesAndClientCert(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	material, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}
	materialDir := resolver(material.MountSource)
	clientCertDir := resolver(filepath.Join(CertDir, "clients", "sandbox-1"))
	for _, dir := range []string{materialDir, clientCertDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("expected %q to exist before removal: %v", dir, err)
		}
	}

	if err := RemoveSandboxMaterial("project-1", "pool-1", "sandbox-1", resolver); err != nil {
		t.Fatalf("RemoveSandboxMaterial() error = %v", err)
	}
	for _, dir := range []string{materialDir, clientCertDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("expected %q removed, stat err = %v", dir, err)
		}
	}

	// A repeated removal is a no-op.
	if err := RemoveSandboxMaterial("project-1", "pool-1", "sandbox-1", resolver); err != nil {
		t.Fatalf("second RemoveSandboxMaterial() error = %v", err)
	}
}

func TestPruneOrphanedMaterialRemovesOnlyOrphans(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	live, err := EnsureSandboxMaterial("project-1", "pool-1", "live-sandbox", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(live) error = %v", err)
	}
	orphan, err := EnsureSandboxMaterial("project-1", "pool-1", "orphan-sandbox", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(orphan) error = %v", err)
	}
	for _, id := range []string{"live-sandbox", "orphan-sandbox"} {
		if err := UpsertSandboxSentinels(resolver, id, []string{"sk-" + id}); err != nil {
			t.Fatalf("UpsertSandboxSentinels(%s) error = %v", id, err)
		}
	}

	// Age both so the grace period does not protect the orphan.
	past := time.Now().Add(-time.Hour)
	for _, id := range []string{"live-sandbox", "orphan-sandbox"} {
		_ = os.Chtimes(resolver(filepath.Join(PoolSandboxMaterialRoot("project-1", "pool-1"), id)), past, past)
	}

	if err := PruneOrphanedMaterial("project-1", "pool-1", []string{"live-sandbox"}, resolver, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial() error = %v", err)
	}

	if _, err := os.Stat(resolver(live.MountSource)); err != nil {
		t.Fatalf("live sandbox material should be kept: %v", err)
	}
	if _, err := os.Stat(resolver(orphan.MountSource)); !os.IsNotExist(err) {
		t.Fatalf("orphan sandbox material should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(resolver(filepath.Join(CertDir, "clients", "orphan-sandbox"))); !os.IsNotExist(err) {
		t.Fatalf("orphan client cert should be removed, stat err = %v", err)
	}

	doc, err := readSecretsDoc(resolver(SecretsFile))
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
	root := t.TempDir()
	resolver := Resolver(root)

	poolBMaterial, err := EnsureSandboxMaterial("project-1", "pool-b", "sandbox-b", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(pool-b) error = %v", err)
	}
	// Age it past any grace window so only scoping — not the grace period —
	// protects it.
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(resolver(poolBMaterial.MountSource), past, past)

	// Pool A prunes with an empty live set: it must not see pool B's material.
	if err := PruneOrphanedMaterial("project-1", "pool-a", nil, resolver, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(pool-a) error = %v", err)
	}
	if _, err := os.Stat(resolver(poolBMaterial.MountSource)); err != nil {
		t.Fatalf("pool A reaped pool B's material: %v", err)
	}
}

// Pool IDs are unique per project, so a prune driven by one project's
// authoritative pool set must not reach another project's material even when
// both projects host a pool of the same name on a shared daemon.
func TestPruneOrphanedMaterialIsProjectScoped(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	otherProject, err := EnsureSandboxMaterial("project-2", "pool-1", "sandbox-b", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(project-2) error = %v", err)
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(resolver(otherProject.MountSource), past, past)

	if err := PruneOrphanedMaterial("project-1", "pool-1", nil, resolver, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(project-1) error = %v", err)
	}
	if _, err := os.Stat(resolver(otherProject.MountSource)); err != nil {
		t.Fatalf("project 1 reaped project 2's material: %v", err)
	}
}

func TestPruneOrphanedMaterialProtectsFreshMaterial(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	fresh, err := EnsureSandboxMaterial("project-1", "pool-1", "fresh-sandbox", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}

	// A recently staged orphan (mid-CreateSandbox) must survive the grace window.
	if err := PruneOrphanedMaterial("project-1", "pool-1", nil, resolver, time.Hour); err != nil {
		t.Fatalf("PruneOrphanedMaterial() error = %v", err)
	}
	if _, err := os.Stat(resolver(fresh.MountSource)); err != nil {
		t.Fatalf("fresh material should be protected by grace period: %v", err)
	}
}

func TestEnsureSandboxMaterialReusesClientCertificate(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	first, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1", resolver)
	if err != nil {
		t.Fatalf("first EnsureSandboxMaterial() error = %v", err)
	}
	firstCert, err := os.ReadFile(filepath.Join(resolver(first.MountSource), "client.crt"))
	if err != nil {
		t.Fatal(err)
	}

	second, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1", resolver)
	if err != nil {
		t.Fatalf("second EnsureSandboxMaterial() error = %v", err)
	}
	secondCert, err := os.ReadFile(filepath.Join(resolver(second.MountSource), "client.crt"))
	if err != nil {
		t.Fatal(err)
	}

	if string(firstCert) != string(secondCert) {
		t.Fatal("client certificate was not reused across calls")
	}
}
