// Package buildkitagent wires the pool-shared BuildKit builder and its output
// registry into the pool agent. It owns the on-disk locations for builder state
// and renders the configuration both systemd units read before systemd boots.
//
// See docs/adr/0044-builds-run-on-a-pool-shared-buildkit.md. Builds run here
// rather than in each sandbox so a pool's sandboxes share one build cache;
// `docker run` stays in the sandbox, because a nested run's bind mounts name
// sandbox paths that do not exist out here.
// Every path here is under a layout.Root: the pool container's, in production,
// and a relocated one in tests. The constants below are the pool container's
// own paths, and Root.System is how a caller reaches them.
package buildkitagent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/pkg/layout"
)

const (
	// Socket is where buildkitd listens. It is a Unix socket, not TCP: the only
	// local caller is the mediator, and sandboxes must not reach buildkitd
	// directly — the mediator is what binds a build to its sandbox.
	Socket = "/run/discobox/buildkitd.sock"

	// ConfigFile is buildkitd's TOML configuration, rendered by the pool agent.
	ConfigFile = "/etc/discobox/buildkitd.toml"

	// UnitEnvironmentFile is read by the buildkitd systemd unit. The unit runs
	// with a clean systemd environment, so without this it cannot address its
	// own pool-scoped state.
	UnitEnvironmentFile = "/etc/discobox/buildkitd.env"

	// RegistryEnvironmentFile is read by the registry systemd unit.
	RegistryEnvironmentFile = "/etc/discobox/registry.env"

	// RegistryListen is where the pool registry binds: the pool's loopback
	// alone, where buildkitd pushes to it and the front forwards to it, over
	// mTLS with the pool builder's certificate (registrytls.go). No sandbox
	// reaches it but through the front (ADR 26-10-29-160 §3): the proxy
	// refuses its own loopback, and the registry admits no other client.
	RegistryListen = registryIP + ":5000"

	// registryIP is the registry's address, which its server certificate
	// names.
	registryIP = "127.0.0.1"

	// RegistryHost is what buildkitd names the registry by. A sandbox names it
	// RegistryFrontHost instead, and the mediator rewrites one to the other
	// (rewriteSolve).
	RegistryHost = RegistryListen

	// RegistryFrontHost mirrors proxyagent.RegistryHost: the host a sandbox
	// pulls and pushes by, which its pool proxy forwards to the front. It is
	// duplicated rather than imported to keep this package independent of the
	// proxy wiring.
	RegistryFrontHost = "registry.discobox.internal"

	// PoolServerName mirrors proxyagent.ServerName, the name the pool's
	// services answer to, for the same reason.
	PoolServerName = "discobox-pool-proxy"

	// RealRunc is the runc binary shipped by the BuildKit image. The pool's runc
	// wrapper execs this, NOT "runc": a wrapper that cannot find its real runc
	// surfaces as a bare exit 127 attributed to the user's build.
	RealRunc = "/usr/bin/buildkit-runc"

	// WrapperDir is prepended to buildkitd's PATH so the wrapper is found ahead
	// of the real runc, the same PATH-ordering trick ADR 0020 uses in the
	// sandbox.
	WrapperDir = "/opt/discobox/bin"

	// MediatorListen is where the mediator accepts mTLS from sandboxes. It binds
	// all interfaces so sandboxes reach it over the per-pool internal network,
	// on the same alias the proxy already answers to.
	MediatorListen = "0.0.0.0:17081"

	// BuildForwarderPort is the port a per-build forwarder binds on the build
	// step's OWN loopback. Every build uses the same port because each has its
	// own network namespace, so the address is private to it — and that privacy
	// is what identifies the build, exactly as a sandbox's loopback forwarder
	// identifies the sandbox.
	BuildForwarderPort = 17009

	// envProjectID and envPoolID are read by the runc wrapper, which inherits
	// buildkitd's environment. It needs them to find the client certificate of
	// the sandbox that owns a build.
	envProjectID = "DISCOBOX_PROJECT_ID"
	envPoolID    = "DISCOBOX_POOL_ID"

	// MITMCAPath is where the pool's MITM CA is staged for the runc wrapper.
	// The wrapper is exec'd by buildkitd with no arguments identifying the
	// pool, so the CA is placed at a fixed path rather than one it would have
	// to derive from a pool ID it is never told.
	MITMCAPath = "/etc/discobox/mitm-ca.crt"
)

// StateRoot is buildkitd's state directory: the content store, snapshots, and
// the solver cache that makes this shared at all. It lives on the pool's
// disposable build tree because it is regenerable and pool-scoped.
func StateRoot(root layout.Root, projectID, poolID string) string {
	return filepath.Join(root.PoolBuild(projectID, poolID), "buildkit")
}

// RegistryRoot is the pool registry's blob storage. It holds build output, not
// cache, but it is still regenerable from the sandboxes' sources, so it shares
// the disposable tree rather than the durable one.
func RegistryRoot(root layout.Root, projectID, poolID string) string {
	return filepath.Join(root.PoolBuild(projectID, poolID), "registry")
}

// legacyRoots are where StateRoot and RegistryRoot used to live: inside
// layout.PoolCache, which is bind-mounted whole into every sandbox. ADR 0050
// moved them out to layout.PoolBuild, a sibling no sandbox can reach.
//
// They are deleted rather than migrated. Both trees are regenerable, and a
// leftover copy is not merely wasted disk: it stays inside the mount, which is
// the exact thing the move was for.
func legacyRoots(root layout.Root, projectID, poolID string) []string {
	cache := root.PoolCache(projectID, poolID)
	return []string{filepath.Join(cache, "buildkit"), filepath.Join(cache, "registry")}
}

// purgeLegacyRoots removes the pre-ADR-0050 locations. A failure is logged by
// the caller rather than fatal: the pool must still boot, and what is left
// behind is stale cache -- wasteful and wrong, but not a reason to have no
// builder at all.
func purgeLegacyRoots(root layout.Root, projectID, poolID string) error {
	var errs []error
	for _, dir := range legacyRoots(root, projectID, poolID) {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove legacy build state %s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

// maxParallelism bounds concurrent build steps. BuildKit's default is 0
// (unlimited), which does not avoid contention so much as convert it into
// memory thrash when a wide graph fans out. Builds now share one daemon across
// the pool, so the bound is per pool rather than per sandbox.
func maxParallelism() int {
	if n := runtime.NumCPU(); n > 2 {
		return n
	}
	return 2
}

// gcKeepStorage is buildkitd's "Reserved[,Free[,Maximum]]" in MB.
//
// It is set explicitly because BuildKit derives its default from the filesystem
// it can see, not from any pool allocation: an observed default Maximum of
// ~93GiB was a fraction of the host disk, so several pools sharing a host would
// each believe they may use all of it.
const gcKeepStorage = "10000,10000,50000"

// Prepare renders every file the builder and registry units read, and creates
// the state directories they need. It runs before systemd boots, so both units
// find a complete configuration on first start.
// mitmCASource is the pool's MITM CA, as produced by the proxy's certificate
// bundle. Prepare copies it to MITMCAPath so the runc wrapper can find it
// without knowing which pool it serves.
func Prepare(root layout.Root, projectID, poolID, mitmCASource string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(poolID) == "" {
		return fmt.Errorf("buildkit configuration needs a project and pool")
	}
	// Before creating the new roots, so a pool that has already been through
	// this cannot pay for the walk twice, and so the sandbox-visible cache is
	// clean by the time any sandbox mounts it (ADR 0050).
	if err := purgeLegacyRoots(root, projectID, poolID); err != nil {
		slog.Warn("purge build state left inside the sandbox-visible cache", "error", err)
	}
	stateRoot := StateRoot(root, projectID, poolID)
	registryRoot := RegistryRoot(root, projectID, poolID)
	for _, dir := range []string{stateRoot, registryRoot, root.System(path.Dir(Socket))} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(root.System(path.Dir(ConfigFile)), 0o755); err != nil {
		return err
	}
	registryTLS, err := ensureRegistryTLS(root, projectID, poolID)
	if err != nil {
		return err
	}
	// Only buildkitd reads this, and it runs as root in the pool container.
	if err := os.WriteFile(root.System(ConfigFile), []byte(renderConfig(registryTLS)), 0o600); err != nil {
		return fmt.Errorf("write buildkitd config: %w", err)
	}
	if err := os.WriteFile(root.System(UnitEnvironmentFile), []byte(renderUnitEnvironment(stateRoot, projectID, poolID)), 0o600); err != nil {
		return fmt.Errorf("write buildkitd unit environment: %w", err)
	}
	if err := os.WriteFile(root.System(RegistryEnvironmentFile), []byte(renderRegistryEnvironment(registryRoot, registryTLS)), 0o600); err != nil {
		return fmt.Errorf("write registry unit environment: %w", err)
	}
	if err := stageMITMCA(root, mitmCASource); err != nil {
		return err
	}
	return nil
}

// stageMITMCA copies the pool's MITM CA where the runc wrapper looks for it. A
// missing source is not an error: a pool with no MITM proxy has nothing to
// inject, and the wrapper already treats an absent CA as "nothing to do".
func stageMITMCA(root layout.Root, source string) error {
	if strings.TrimSpace(source) == "" {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read pool MITM CA %s: %w", source, err)
	}
	//nolint:gosec // A trust anchor is public and must be readable inside every build container.
	if err := os.WriteFile(root.System(MITMCAPath), data, 0o644); err != nil {
		return fmt.Errorf("stage pool MITM CA: %w", err)
	}
	return nil
}

// renderConfig is the TOML buildkitd reads. Only settings with no command-line
// equivalent live here; everything the unit can pass as a flag is passed as a
// flag, so the unit stays the single readable description of how buildkitd runs.
func renderConfig(registryTLS *registryTLS) string {
	// The pool registry is reached at RegistryHost over mTLS. The keypair is
	// buildkitd's for that host alone: a build that names the registry any
	// other way presents nothing, and is refused (registrytls.go). `http =
	// false` is said, not left out: buildkitd speaks plaintext to a loopback
	// host it is told nothing about, which the registry's TLS answers 400.
	return fmt.Sprintf(`# Rendered by pool-agent. Do not edit.
[registry.%q]
  http = false
  ca = [%q]
  [[registry.%q.keypair]]
    key = %q
    cert = %q
`, RegistryHost, registryTLS.CA, RegistryHost, registryTLS.ClientKey, registryTLS.ClientCert)
}

// renderUnitEnvironment is separated from the write so its contents can be
// asserted without touching the container's /etc.
func renderUnitEnvironment(stateRoot, projectID, poolID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DISCOBOX_BUILDKIT_ROOT=%s\n", stateRoot)
	fmt.Fprintf(&b, "DISCOBOX_BUILDKIT_ADDR=unix://%s\n", Socket)
	fmt.Fprintf(&b, "DISCOBOX_BUILDKIT_MAX_PARALLELISM=%d\n", maxParallelism())
	fmt.Fprintf(&b, "DISCOBOX_BUILDKIT_GC_KEEPSTORAGE=%s\n", gcKeepStorage)
	fmt.Fprintf(&b, "DISCOBOX_BUILDKIT_RUNC=%s\n", path.Join(WrapperDir, "runc"))
	// The runc wrapper inherits this environment, and needs the pool's identity
	// to locate the client certificate of the sandbox that owns a build.
	fmt.Fprintf(&b, "%s=%s\n", envProjectID, projectID)
	fmt.Fprintf(&b, "%s=%s\n", envPoolID, poolID)
	// buildkitd resolves and pulls base images itself, before any build
	// container exists, so nothing injected into a container's spec can give it
	// egress. What it is given is the agent's own inherited proxy environment
	// (its unit's PassEnvironment): an operator's upstream proxy where one is
	// set, never the pool's proxy, and on a pool machine usually nothing, so
	// its pulls there go out over the machine's own route. A build step's proxy
	// is not this: the mediator injects it (withBuildProxy) and the runc
	// wrapper rewrites it. A cost ADR 26-10-27-767 states.
	b.WriteString(proxyEnvironment(os.Environ()))
	return b.String()
}

func renderRegistryEnvironment(registryRoot string, registryTLS *registryTLS) string {
	var b strings.Builder
	fmt.Fprintf(&b, "REGISTRY_HTTP_ADDR=%s\n", RegistryListen)
	// mTLS, admitting the pool builder's certificate alone (registrytls.go).
	// The client CAs are a list, which the registry reads from its
	// environment as YAML.
	fmt.Fprintf(&b, "REGISTRY_HTTP_TLS_CERTIFICATE=%s\n", registryTLS.ServerCert)
	fmt.Fprintf(&b, "REGISTRY_HTTP_TLS_KEY=%s\n", registryTLS.ServerKey)
	fmt.Fprintf(&b, "REGISTRY_HTTP_TLS_CLIENTCAS=[%q]\n", registryTLS.CA)
	// Its clients reach it at two names — buildkitd at RegistryHost, a
	// sandbox at RegistryFrontHost through its proxy and the front — so an
	// upload's Location names no host, and each follows it where it already is.
	b.WriteString("REGISTRY_HTTP_RELATIVEURLS=true\n")
	fmt.Fprintf(&b, "REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY=%s\n", registryRoot)
	// Deletes are what makes garbage collection possible at all; without this
	// the registry grows without bound and only a wipe reclaims anything.
	b.WriteString("REGISTRY_STORAGE_DELETE_ENABLED=true\n")
	b.WriteString(proxyEnvironment(os.Environ()))
	return b.String()
}

// proxyEnvironment forwards this process's proxy-trust variables to a unit:
// whatever proxy environment the agent inherited (an operator's upstream proxy,
// as a pool inside a sandbox has; never the pool's own proxy; on a pool machine
// usually nothing). systemd resets the environment for units, so without this
// the builder and the registry would not have even that, and where an upstream
// proxy is the only way out, a registry that cannot resolve its upstream fails
// as an opaque 404 rather than a connection error.
//
// The pool registry needs no exception: it is on loopback, which Go's proxy
// selection never sends through a proxy whatever NO_PROXY says.
func proxyEnvironment(environ []string) string {
	wanted := map[string]bool{
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true,
		"NO_PROXY": true, "no_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	}
	var b strings.Builder
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !wanted[name] || strings.TrimSpace(value) == "" {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", name, value)
	}
	return b.String()
}

// readFile is a thin wrapper so mediator.go reads through the same test-root
// relocation as the rest of this package.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
