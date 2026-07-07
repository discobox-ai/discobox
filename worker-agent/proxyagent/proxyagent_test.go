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

	material, err := EnsureSandboxMaterial("sandbox-1", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}

	if want := filepath.Join(SandboxMaterialRoot, "sandbox-1"); material.MountSource != want {
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
	// Node.js/Claude Code trust the MITM CA directly; Python/openssl trust the
	// system bundle that the boot-time trust step augments.
	if got := material.Env["NODE_EXTRA_CA_CERTS"]; got != filepath.Join(SandboxProxyMount, "mitm-ca.crt") {
		t.Fatalf("NODE_EXTRA_CA_CERTS = %q, want mounted MITM CA path", got)
	}
	if got := material.Env["SSL_CERT_FILE"]; got != SystemCABundle {
		t.Fatalf("SSL_CERT_FILE = %q, want system CA bundle", got)
	}
	if got := material.Env["REQUESTS_CA_BUNDLE"]; got != SystemCABundle {
		t.Fatalf("REQUESTS_CA_BUNDLE = %q, want system CA bundle", got)
	}
}

func TestRemoveSandboxMaterialDeletesStagedFilesAndClientCert(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	material, err := EnsureSandboxMaterial("sandbox-1", resolver)
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

	if err := RemoveSandboxMaterial("sandbox-1", resolver); err != nil {
		t.Fatalf("RemoveSandboxMaterial() error = %v", err)
	}
	for _, dir := range []string{materialDir, clientCertDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("expected %q removed, stat err = %v", dir, err)
		}
	}

	// A repeated removal is a no-op.
	if err := RemoveSandboxMaterial("sandbox-1", resolver); err != nil {
		t.Fatalf("second RemoveSandboxMaterial() error = %v", err)
	}
}

func TestPruneOrphanedMaterialRemovesOnlyOrphans(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	live, err := EnsureSandboxMaterial("live-sandbox", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(live) error = %v", err)
	}
	orphan, err := EnsureSandboxMaterial("orphan-sandbox", resolver)
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
		for _, base := range []string{SandboxMaterialRoot, filepath.Join(CertDir, "clients")} {
			_ = os.Chtimes(resolver(filepath.Join(base, id)), past, past)
		}
	}

	if err := PruneOrphanedMaterial([]string{"live-sandbox"}, resolver, time.Minute); err != nil {
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

func TestPruneOrphanedMaterialProtectsFreshMaterial(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	fresh, err := EnsureSandboxMaterial("fresh-sandbox", resolver)
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}

	// A recently staged orphan (mid-CreateSandbox) must survive the grace window.
	if err := PruneOrphanedMaterial(nil, resolver, time.Hour); err != nil {
		t.Fatalf("PruneOrphanedMaterial() error = %v", err)
	}
	if _, err := os.Stat(resolver(fresh.MountSource)); err != nil {
		t.Fatalf("fresh material should be protected by grace period: %v", err)
	}
}

func TestEnsureSandboxMaterialReusesClientCertificate(t *testing.T) {
	root := t.TempDir()
	resolver := Resolver(root)

	first, err := EnsureSandboxMaterial("sandbox-1", resolver)
	if err != nil {
		t.Fatalf("first EnsureSandboxMaterial() error = %v", err)
	}
	firstCert, err := os.ReadFile(filepath.Join(resolver(first.MountSource), "client.crt"))
	if err != nil {
		t.Fatal(err)
	}

	second, err := EnsureSandboxMaterial("sandbox-1", resolver)
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
