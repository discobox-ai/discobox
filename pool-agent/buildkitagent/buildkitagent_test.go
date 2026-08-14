package buildkitagent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/pool-agent/buildkitagent"
)

func prepare(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	buildkitagent.SetTestRoot(root)
	t.Cleanup(func() { buildkitagent.SetTestRoot("") })
	if err := buildkitagent.Prepare("proj", "pool", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return root
}

func read(t *testing.T, root, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPreparePlacesStateOnThePoolCacheVolume(t *testing.T) {
	root := prepare(t)

	// Builder state and registry blobs are regenerable, so they belong on the
	// disposable cache tree rather than the durable one a backend must retain.
	for _, dir := range []string{
		buildkitagent.StateRoot("proj", "pool"),
		buildkitagent.RegistryRoot("proj", "pool"),
	} {
		if !strings.Contains(dir, "/cache/") {
			t.Errorf("%s is not on the disposable cache tree", dir)
		}
		if !strings.Contains(dir, "proj") || !strings.Contains(dir, "pool") {
			t.Errorf("%s is not pool-scoped: two pools sharing a host would collide", dir)
		}
		if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
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

	if !strings.Contains(env, "REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY="+buildkitagent.RegistryRoot("proj", "pool")) {
		t.Errorf("registry storage is not pool-scoped:\n%s", env)
	}
	// Without deletes enabled the registry only grows, and garbage collection
	// cannot reclaim anything short of wiping the tree.
	if !strings.Contains(env, "REGISTRY_STORAGE_DELETE_ENABLED=true") {
		t.Error("registry deletes are disabled, so its storage can never be reclaimed")
	}
}

func TestBuilderTreatsThePoolRegistryAsPlaintext(t *testing.T) {
	root := prepare(t)
	cfg := read(t, root, buildkitagent.ConfigFile)

	// The pool registry has no TLS: it is reachable only over the per-pool
	// internal network. buildkitd defaults to HTTPS, so without this every push
	// fails with a TLS handshake error against a plaintext listener.
	if !strings.Contains(cfg, buildkitagent.RegistryHost) || !strings.Contains(cfg, "http = true") {
		t.Errorf("pool registry is not declared plaintext:\n%s", cfg)
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

func TestThePoolRegistryIsNeverReachedThroughTheProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:17008")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	root := prepare(t)

	// Declaring the registry plaintext is not enough on its own: a proxied
	// request never reaches it as plaintext. The MITM proxy answers the
	// registry's port over TLS, so a push fails with `EOF` against a URL the
	// builder never spelled as https — which reads as a broken registry rather
	// than as a routing mistake.
	for _, file := range []string{buildkitagent.UnitEnvironmentFile, buildkitagent.RegistryEnvironmentFile} {
		body := read(t, root, file)
		for _, name := range []string{"NO_PROXY", "no_proxy"} {
			line := lineFor(t, body, name)
			if !strings.Contains(line, buildkitagent.RegistryServerName) {
				t.Errorf("%s: %s does not bypass the pool registry: %q", file, name, line)
			}
			if !strings.Contains(line, "127.0.0.1") {
				t.Errorf("%s: %s dropped what the pool already bypassed: %q", file, name, line)
			}
		}
	}
}

func TestTheRegistryIsBypassedEvenWithNoInheritedNoProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:17008")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	root := prepare(t)

	// A pool that set no NO_PROXY at all still must not proxy its own
	// registry, so the variable is written rather than only amended.
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		line := lineFor(t, read(t, root, buildkitagent.UnitEnvironmentFile), name)
		if !strings.Contains(line, buildkitagent.RegistryServerName) {
			t.Errorf("%s does not bypass the pool registry: %q", name, line)
		}
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
	buildkitagent.SetTestRoot(t.TempDir())
	t.Cleanup(func() { buildkitagent.SetTestRoot("") })
	if err := buildkitagent.Prepare("", "pool", ""); err == nil {
		t.Error("Prepare accepted an empty project, which would write pool state to a shared path")
	}
}
