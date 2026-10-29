package buildkitagent_test

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/http/httpproxy"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
)

func prepare(t *testing.T) layout.Root {
	t.Helper()
	root := layout.ContainerAt(t.TempDir())
	if err := buildkitagent.Prepare(root, "proj", "pool", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return root
}

// read reads one of the pool container's own files, as root relocates it.
func read(t *testing.T, root layout.Root, path string) string {
	t.Helper()
	data, err := os.ReadFile(root.System(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPreparePlacesStateOnTheDisposableTree(t *testing.T) {
	root := prepare(t)

	// Builder state and registry blobs are regenerable, so they belong on the
	// disposable cache tree rather than the durable one a backend must retain.
	for _, dir := range []string{
		buildkitagent.StateRoot(root, "proj", "pool"),
		buildkitagent.RegistryRoot(root, "proj", "pool"),
	} {
		if !strings.Contains(filepath.ToSlash(dir), "/cache/") {
			t.Errorf("%s is not on the disposable cache tree", dir)
		}
		if !strings.Contains(dir, "proj") || !strings.Contains(dir, "pool") {
			t.Errorf("%s is not pool-scoped: two pools sharing a host would collide", dir)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("Prepare did not create %s: %v", dir, err)
		}
	}
}

func TestBuilderEnvironmentBoundsParallelismAndStorage(t *testing.T) {
	root := prepare(t)
	env := read(t, root, buildkitagent.UnitEnvironmentFile)

	// BuildKit derives GC defaults from the filesystem it can see, not from any
	// pool allocation, so several pools on one host would each claim the whole
	// disk. Both bounds must be stated rather than inherited.
	if !strings.Contains(env, "DISCOBOX_BUILDKIT_GC_KEEPSTORAGE=") {
		t.Error("no explicit GC bound: the inherited default is a fraction of the host disk")
	}
	if !strings.Contains(env, "DISCOBOX_BUILDKIT_MAX_PARALLELISM=") {
		t.Error("no explicit parallelism bound; the default of 0 is unlimited")
	}
	for _, unset := range []string{"MAX_PARALLELISM=0\n", "MAX_PARALLELISM=1\n"} {
		if strings.Contains(env, unset) {
			t.Errorf("parallelism %q leaves builds unbounded or serialized", unset)
		}
	}
}

func TestBuilderRunsTheWrapperNotTheRealRunc(t *testing.T) {
	root := prepare(t)
	env := read(t, root, buildkitagent.UnitEnvironmentFile)

	// The wrapper is what injects CA trust and the per-build egress hooks. If
	// buildkitd is pointed at the real runc, every build silently loses both.
	if !strings.Contains(env, "DISCOBOX_BUILDKIT_RUNC="+buildkitagent.WrapperDir) {
		t.Errorf("buildkitd is not pointed at the wrapper directory:\n%s", env)
	}
	if strings.Contains(env, "DISCOBOX_BUILDKIT_RUNC="+buildkitagent.RealRunc) {
		t.Error("buildkitd is pointed straight at the real runc, bypassing injection")
	}
}

func TestRegistryStoresBlobsUnderItsOwnPoolRootAndAllowsDeletes(t *testing.T) {
	root := prepare(t)
	env := read(t, root, buildkitagent.RegistryEnvironmentFile)

	if !strings.Contains(env, "REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY="+buildkitagent.RegistryRoot(root, "proj", "pool")) {
		t.Errorf("registry storage is not pool-scoped:\n%s", env)
	}
	// Without deletes enabled the registry only grows, and garbage collection
	// cannot reclaim anything short of wiping the tree.
	if !strings.Contains(env, "REGISTRY_STORAGE_DELETE_ENABLED=true") {
		t.Error("registry deletes are disabled, so its storage can never be reclaimed")
	}
}

// The registry is reached over mTLS with the pool builder's certificate, which
// buildkitd presents for the registry's own address alone, and the registry
// admits no client without it (ADR 26-10-29-160 §3).
func TestTheRegistryAdmitsOnlyThePoolBuilder(t *testing.T) {
	root := prepare(t)
	cfg := read(t, root, buildkitagent.ConfigFile)
	dir := buildkitagent.RegistryTLSDir(root, "proj", "pool")
	if !strings.Contains(cfg, `[registry."`+buildkitagent.RegistryHost+`"]`) || !strings.Contains(cfg, "keypair") || !strings.Contains(cfg, dir) {
		t.Errorf("buildkitd presents no certificate to the registry:\n%s", cfg)
	}
	// Left unsaid, buildkitd speaks plaintext to a loopback registry.
	if !strings.Contains(cfg, "http = false") || strings.Contains(cfg, "insecure") {
		t.Errorf("buildkitd reaches the registry in plaintext:\n%s", cfg)
	}
	env := read(t, root, buildkitagent.RegistryEnvironmentFile)
	for _, name := range []string{"REGISTRY_HTTP_TLS_CERTIFICATE", "REGISTRY_HTTP_TLS_KEY", "REGISTRY_HTTP_TLS_CLIENTCAS"} {
		if !strings.Contains(lineFor(t, env, name), dir) {
			t.Errorf("%s does not name the registry's own material:\n%s", name, env)
		}
	}
	if _, err := buildkitagent.RegistryClientTLS(root, "proj", "pool"); err != nil {
		t.Fatalf("the front cannot load the pool builder's certificate: %v", err)
	}
}

func TestPrepareForwardsProxyEnvironmentToBothUnits(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:17008")
	root := prepare(t)

	// Neither unit inherits the agent's environment, and a pool has no route
	// off-box except the proxy. buildkitd resolves base images in its own
	// process, before any build container exists, so no spec-level injection
	// can reach it.
	for _, file := range []string{buildkitagent.UnitEnvironmentFile, buildkitagent.RegistryEnvironmentFile} {
		if !strings.Contains(read(t, root, file), "HTTPS_PROXY=http://127.0.0.1:17008") {
			t.Errorf("%s does not forward the proxy environment; upstream fetches will fail", file)
		}
	}
}

// The registry leaves the pool's network: it binds loopback alone, and the
// front is how a sandbox reaches it (ADR 26-10-29-160 §3). Its upload
// locations name no host, since buildkitd and a sandbox reach it at different
// ones.
func TestThePoolRegistryListensOnLoopbackAlone(t *testing.T) {
	root := prepare(t)
	env := read(t, root, buildkitagent.RegistryEnvironmentFile)

	addr := strings.TrimPrefix(lineFor(t, env, "REGISTRY_HTTP_ADDR"), "REGISTRY_HTTP_ADDR=")
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("the registry binds %s, which is not loopback", addr)
	}
	if lineFor(t, env, "REGISTRY_HTTP_RELATIVEURLS") != "REGISTRY_HTTP_RELATIVEURLS=true" {
		t.Errorf("upload locations name a host:\n%s", env)
	}
}

// The builder pushes to the registry directly whatever proxy it inherited:
// Go's proxy selection never proxies loopback, which is what lets the units
// carry the agent's proxy environment with no exception for the registry.
func TestThePoolRegistryIsNeverReachedThroughTheProxy(t *testing.T) {
	proxyFor := (&httpproxy.Config{HTTPProxy: "http://127.0.0.1:17008", HTTPSProxy: "http://127.0.0.1:17008"}).ProxyFunc()
	via, err := proxyFor(&url.URL{Scheme: "http", Host: buildkitagent.RegistryHost})
	if err != nil || via != nil {
		t.Fatalf("a push to %s goes through %v (%v)", buildkitagent.RegistryHost, via, err)
	}
}

// lineFor returns the `NAME=...` line of an EnvironmentFile, so a test asserts
// against one variable rather than against the whole file's text.
func lineFor(t *testing.T, body, name string) string {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, name+"=") {
			return line
		}
	}
	t.Fatalf("%s is not set:\n%s", name, body)
	return ""
}

func TestPrepareRejectsAnUnscopedPool(t *testing.T) {
	if err := buildkitagent.Prepare(layout.ContainerAt(t.TempDir()), "", "pool", ""); err == nil {
		t.Error("Prepare accepted an empty project, which would write pool state to a shared path")
	}
}

// ADR 0050. layout.PoolCache is bind-mounted whole into every sandbox, and the
// sandbox user holds sudo, so build state kept under it is readable and
// writable by every sandbox in the pool. Being on the disposable tree is not
// enough — it has to be outside that particular directory.
func TestBuildStateIsOutsideTheSandboxVisibleCache(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	cache := root.PoolCache("proj", "pool")
	for _, dir := range []string{
		buildkitagent.StateRoot(root, "proj", "pool"),
		buildkitagent.RegistryRoot(root, "proj", "pool"),
	} {
		if dir == cache || strings.HasPrefix(dir, cache+string(filepath.Separator)) {
			t.Errorf("%s is inside the sandbox-visible cache %s", dir, cache)
		}
	}
}

// A pool upgraded across ADR 0050 still has the old directories, and they are
// still inside the mount. Leaving them would make the move pointless, so
// Prepare removes them rather than migrating them.
func TestPreparePurgesPreADR0050BuildState(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())

	cache := root.PoolCache("proj", "pool")
	legacyBuildkit := filepath.Join(cache, "buildkit")
	legacyRegistry := filepath.Join(cache, "registry")
	// A sandbox's own cache content sits in the same directory and must survive.
	sandboxCache := filepath.Join(cache, "home", "dev", ".cache")
	for _, dir := range []string{legacyBuildkit, legacyRegistry, sandboxCache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := buildkitagent.Prepare(root, "proj", "pool", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	for _, dir := range []string{legacyBuildkit, legacyRegistry} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s still exists inside the sandbox-visible cache (stat err %v)", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sandboxCache, "marker")); err != nil {
		t.Errorf("Prepare destroyed a sandbox's own cache content: %v", err)
	}
}
