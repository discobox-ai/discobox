package proxyagent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/layout"
)

func TestEnsureSandboxMaterialStagesClientOnly(t *testing.T) {
	withTestRoot(t)

	material, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}

	if want := filepath.Join(PoolSandboxMaterialRoot("project-1", "pool-1"), "sandbox-1"); material.MountSource != want {
		t.Fatalf("MountSource = %q, want %q", material.MountSource, want)
	}

	dir := material.MountSource
	for _, name := range []string{"mtls-ca.crt", "mitm-ca.crt", "client.crt", "client.key", "bridge.json", "bridge-docker.json"} {
		if _, err := os.Stat(resolve(filepath.Join(dir, name))); err != nil {
			t.Fatalf("expected staged file %q: %v", name, err)
		}
	}

	// The CA private keys must never be exposed to a sandbox.
	for _, leaked := range []string{"mtls-ca.key", "mitm-ca.key"} {
		if _, err := os.Stat(resolve(filepath.Join(dir, leaked))); !os.IsNotExist(err) {
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

func TestRemoveSandboxMaterialDeletesStagedFilesAndClientCert(t *testing.T) {
	withTestRoot(t)

	material, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}
	materialDir := material.MountSource
	clientCertDir := filepath.Join(layout.ProxyCerts("project-1", "pool-1"), "clients", "sandbox-1")
	for _, dir := range []string{materialDir, clientCertDir} {
		if _, err := os.Stat(resolve(dir)); err != nil {
			t.Fatalf("expected %q to exist before removal: %v", dir, err)
		}
	}

	if err := RemoveSandboxMaterial("project-1", "pool-1", "sandbox-1"); err != nil {
		t.Fatalf("RemoveSandboxMaterial() error = %v", err)
	}
	for _, dir := range []string{materialDir, clientCertDir} {
		if _, err := os.Stat(resolve(dir)); !os.IsNotExist(err) {
			t.Fatalf("expected %q removed, stat err = %v", dir, err)
		}
	}

	// A repeated removal is a no-op.
	if err := RemoveSandboxMaterial("project-1", "pool-1", "sandbox-1"); err != nil {
		t.Fatalf("second RemoveSandboxMaterial() error = %v", err)
	}
}

func TestPruneOrphanedMaterialRemovesOnlyOrphans(t *testing.T) {
	withTestRoot(t)

	live, err := EnsureSandboxMaterial("project-1", "pool-1", "live-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(live) error = %v", err)
	}
	orphan, err := EnsureSandboxMaterial("project-1", "pool-1", "orphan-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(orphan) error = %v", err)
	}
	for _, id := range []string{"live-sandbox", "orphan-sandbox"} {
		if err := UpsertSandboxSentinels("project-1", "pool-1", id, []string{"sk-" + id}); err != nil {
			t.Fatalf("UpsertSandboxSentinels(%s) error = %v", id, err)
		}
	}

	// Age both so the grace period does not protect the orphan.
	past := time.Now().Add(-time.Hour)
	for _, id := range []string{"live-sandbox", "orphan-sandbox"} {
		_ = os.Chtimes(resolve(filepath.Join(PoolSandboxMaterialRoot("project-1", "pool-1"), id)), past, past)
	}

	if err := PruneOrphanedMaterial("project-1", "pool-1", []string{"live-sandbox"}, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial() error = %v", err)
	}

	if _, err := os.Stat(resolve(live.MountSource)); err != nil {
		t.Fatalf("live sandbox material should be kept: %v", err)
	}
	if _, err := os.Stat(resolve(orphan.MountSource)); !os.IsNotExist(err) {
		t.Fatalf("orphan sandbox material should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(resolve(filepath.Join(layout.ProxyCerts("project-1", "pool-1"), "clients", "orphan-sandbox"))); !os.IsNotExist(err) {
		t.Fatalf("orphan client cert should be removed, stat err = %v", err)
	}

	doc, err := readSecretsDoc(layout.ProxySecretsFile("project-1", "pool-1"))
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
	withTestRoot(t)

	poolBMaterial, err := EnsureSandboxMaterial("project-1", "pool-b", "sandbox-b")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(pool-b) error = %v", err)
	}
	// Age it past any grace window so only scoping — not the grace period —
	// protects it.
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(resolve(poolBMaterial.MountSource), past, past)

	// Pool A prunes with an empty live set: it must not see pool B's material.
	if err := PruneOrphanedMaterial("project-1", "pool-a", nil, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(pool-a) error = %v", err)
	}
	if _, err := os.Stat(resolve(poolBMaterial.MountSource)); err != nil {
		t.Fatalf("pool A reaped pool B's material: %v", err)
	}
}

// Pool IDs are unique per project, so a prune driven by one project's
// authoritative pool set must not reach another project's material even when
// both projects host a pool of the same name on a shared daemon.
func TestPruneOrphanedMaterialIsProjectScoped(t *testing.T) {
	withTestRoot(t)

	otherProject, err := EnsureSandboxMaterial("project-2", "pool-1", "sandbox-b")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial(project-2) error = %v", err)
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(resolve(otherProject.MountSource), past, past)

	if err := PruneOrphanedMaterial("project-1", "pool-1", nil, time.Minute); err != nil {
		t.Fatalf("PruneOrphanedMaterial(project-1) error = %v", err)
	}
	if _, err := os.Stat(resolve(otherProject.MountSource)); err != nil {
		t.Fatalf("project 1 reaped project 2's material: %v", err)
	}
}

func TestPruneOrphanedMaterialProtectsFreshMaterial(t *testing.T) {
	withTestRoot(t)

	fresh, err := EnsureSandboxMaterial("project-1", "pool-1", "fresh-sandbox")
	if err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}

	// A recently staged orphan (mid-CreateSandbox) must survive the grace window.
	if err := PruneOrphanedMaterial("project-1", "pool-1", nil, time.Hour); err != nil {
		t.Fatalf("PruneOrphanedMaterial() error = %v", err)
	}
	if _, err := os.Stat(resolve(fresh.MountSource)); err != nil {
		t.Fatalf("fresh material should be protected by grace period: %v", err)
	}
}

func TestEnsureSandboxMaterialReusesClientCertificate(t *testing.T) {
	withTestRoot(t)

	first, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("first EnsureSandboxMaterial() error = %v", err)
	}
	firstCert, err := os.ReadFile(resolve(filepath.Join(first.MountSource, "client.crt")))
	if err != nil {
		t.Fatal(err)
	}

	second, err := EnsureSandboxMaterial("project-1", "pool-1", "sandbox-1")
	if err != nil {
		t.Fatalf("second EnsureSandboxMaterial() error = %v", err)
	}
	secondCert, err := os.ReadFile(resolve(filepath.Join(second.MountSource, "client.crt")))
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
