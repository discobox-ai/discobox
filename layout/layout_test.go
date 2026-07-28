package layout

import (
	"reflect"
	"strings"
	"testing"
)

// The layout must match what the pool agent and engine already write, so
// adopting this package moves no existing pool's data.
func TestPathsMatchTheEstablishedLayout(t *testing.T) {
	for name, tc := range map[string]struct{ got, want string }{
		"pool data":         {PoolData("prj", "pool"), "/var/lib/discobox/projects/prj/pools/pool"},
		"pool sandboxes":    {PoolSandboxes("prj", "pool"), "/var/lib/discobox/projects/prj/pools/pool/sandboxes"},
		"sandbox data":      {SandboxData("prj", "pool", "sb"), "/var/lib/discobox/projects/prj/pools/pool/sandboxes/sb/data"},
		"sandbox config":    {SandboxConfig("prj", "pool", "sb"), "/var/lib/discobox/projects/prj/pools/pool/sandboxes/sb/config"},
		"sandbox secrets":   {SandboxSecrets("prj", "pool", "sb"), "/var/lib/discobox/projects/prj/pools/pool/sandboxes/sb/secrets"},
		"sandbox sources":   {SandboxSources("prj", "pool", "sb"), "/var/lib/discobox/projects/prj/pools/pool/sandboxes/sb/sources"},
		"pool cache":        {PoolCache("prj", "pool"), "/var/lib/discobox/cache/projects/prj/pools/pool/cache"},
		"proxy certs":       {ProxyCerts("prj", "pool"), "/var/lib/discobox/proxy/projects/prj/pools/pool/certs"},
		"proxy pool":        {ProxyPool("prj", "pool"), "/var/lib/discobox/proxy/projects/prj/pools/pool"},
		"proxy pool sboxes": {ProxyPoolSandboxes("prj", "pool"), "/var/lib/discobox/proxy/projects/prj/pools/pool/sandboxes"},
		"proxy audit db":    {ProxyAuditDB("prj", "pool"), "/var/lib/discobox/proxy/projects/prj/pools/pool/audit.db"},
		"proxy cache":       {ProxyCache("prj", "pool"), "/var/lib/discobox/proxy/projects/prj/pools/pool/cache"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", name, tc.got, tc.want)
		}
	}
}

// The reason this package exists: one daemon can host pools from different
// projects, so no two pools may ever be handed the same writable path.
func TestPoolStateIsAlwaysScopedToItsPool(t *testing.T) {
	perPool := map[string]func(projectID, poolID string) string{
		"PoolData":                PoolData,
		"PoolSandboxes":           PoolSandboxes,
		"PoolCache":               PoolCache,
		"ProxyPool":               ProxyPool,
		"ProxyPoolSandboxes":      ProxyPoolSandboxes,
		"ProxySecretsFile":        ProxySecretsFile,
		"ProxyResolveContextFile": ProxyResolveContextFile,
		"ProxyAuditDB":            ProxyAuditDB,
		"ProxyCache":              ProxyCache,
		"ProxyStreams":            ProxyStreams,
		"ProxyBodies":             ProxyBodies,
	}
	for name, fn := range perPool {
		samePool := fn("prj-a", "pool-1")
		otherPool := fn("prj-a", "pool-2")
		otherProject := fn("prj-b", "pool-1")
		if samePool == otherPool {
			t.Errorf("%s returns the same path for two pools (%q); they would overwrite each other", name, samePool)
		}
		if samePool == otherProject {
			t.Errorf("%s returns the same path for two projects (%q)", name, samePool)
		}
		if !strings.Contains(samePool, "pool-1") || !strings.Contains(samePool, "prj-a") {
			t.Errorf("%s = %q, want it to name both its project and pool", name, samePool)
		}
	}
}

// These two carry a pool's identity and its scoped credential. They were
// previously shared across every pool on a daemon, which let one pool's proxy
// read another pool's token.
func TestProxyCredentialFilesAreNotShared(t *testing.T) {
	first := ProxyResolveContextFile("prj", "pool-1")
	second := ProxyResolveContextFile("prj", "pool-2")
	if first == second {
		t.Fatalf("resolve context is shared between pools: %q", first)
	}
	if ProxySecretsFile("prj", "pool-1") == ProxySecretsFile("prj", "pool-2") {
		t.Fatal("secrets registry is shared between pools")
	}
	// Certificates are pool-scoped too: a pool's proxy is its own trust domain,
	// so a CA shared across pools would leave a sandbox trusting another pool's
	// proxy.
	if ProxyCerts("prj", "pool-1") == ProxyCerts("prj", "pool-2") {
		t.Fatal("proxy certificate material is shared between pools")
	}
}

func TestMountRootsAreTheThreeTrees(t *testing.T) {
	want := []string{
		"/var/lib/discobox/projects",
		"/var/lib/discobox/cache",
		"/var/lib/discobox/proxy",
	}
	if got := MountRoots(); !reflect.DeepEqual(got, want) {
		t.Fatalf("MountRoots() = %v, want %v", got, want)
	}
}

// A backend whose daemon sees the state root somewhere else — wslc, which must
// place it on the only persistent disk it has — translates only at the boundary.
func TestHostMappingTranslatesOnlyUnderTheStateRoot(t *testing.T) {
	mapping := NewHostMapping("/var/lib/docker/discobox")

	if got, want := mapping.HostPath(PoolData("prj", "pool")),
		"/var/lib/docker/discobox/projects/prj/pools/pool"; got != want {
		t.Fatalf("HostPath(pool data) = %q, want %q", got, want)
	}
	if got, want := mapping.HostPath(ContainerRoot), "/var/lib/docker/discobox"; got != want {
		t.Fatalf("HostPath(root) = %q, want %q", got, want)
	}
	// A developer's own source directory is already a daemon path and must pass
	// through untouched, or a local source bind would be rewritten to nonsense.
	for _, outside := range []string{"/home/darren/project", "/var/lib/dockerfoo", "C:/src/thing", ""} {
		if got := mapping.HostPath(outside); got != outside {
			t.Fatalf("HostPath(%q) = %q, want it unchanged", outside, got)
		}
	}
}

// The common case: the state root is bind-mounted at the same path the
// container sees, so nothing is translated at all.
func TestHostMappingIsIdentityWithoutARelocation(t *testing.T) {
	mapping := NewHostMapping("")
	for _, p := range []string{PoolData("prj", "pool"), ContainerRoot, "/elsewhere"} {
		if got := mapping.HostPath(p); got != p {
			t.Fatalf("HostPath(%q) = %q, want it unchanged", p, got)
		}
	}
	if got := mapping.HostRoot(); got != ContainerRoot {
		t.Fatalf("HostRoot() = %q, want %q", got, ContainerRoot)
	}
}

func TestHostMappingTrimsTrailingSeparators(t *testing.T) {
	mapping := NewHostMapping("/var/lib/docker/discobox/")
	if got, want := mapping.HostRoot(), "/var/lib/docker/discobox"; got != want {
		t.Fatalf("HostRoot() = %q, want %q", got, want)
	}
}
