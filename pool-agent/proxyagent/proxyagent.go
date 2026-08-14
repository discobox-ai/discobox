// Package proxyagent wires the pool-scoped proxy component into the pool
// agent. It owns the per-pool on-disk locations for proxy certificate material,
// runs the proxy server (as a systemd unit inside the pool container), and
// prepares the per-sandbox client material that is distributed into sandbox
// containers.
package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/layout"
	"github.com/obot-platform/discobox/proxy"
	"github.com/obot-platform/discobox/sandboxconfig"
)

const (
	clientCertValidity    = 365 * 24 * time.Hour
	clientCertRenewBefore = 30 * 24 * time.Hour
)

const (
	// envHostMountPrefix mirrors poolagent.EnvHostMountPrefix. It is duplicated
	// here to avoid importing the root pool-agent package (which imports this
	// one).
	envHostMountPrefix = "DISCOBOX_POOL_HOST_MOUNT_PREFIX"
	// envControlPlaneURL mirrors poolagent.EnvControlPlaneURL, duplicated for
	// the same reason.
	envControlPlaneURL = "DISCOBOX_CONTROL_PLANE_URL"
	// envProjectID and envPoolID tell the proxy unit which pool it serves. It
	// runs with a clean systemd environment, and every path it writes is scoped
	// to that pool, so without these it cannot address its own state.
	envProjectID = "DISCOBOX_PROJECT_ID"
	envPoolID    = "DISCOBOX_POOL_ID"

	// ListenAddress is where the pool proxy accepts mTLS connections. It binds
	// all interfaces so sandbox containers can reach it through the Docker host
	// gateway.
	ListenAddress = "0.0.0.0:17080"

	// ServerName is the stable DNS name presented on the pool proxy server
	// certificate. Sandbox containers resolve it to the pool over the
	// per-pool internal network (Docker embedded DNS + pool network alias),
	// so the mTLS ServerName check stays valid regardless of the pool IP.
	ServerName = "discobox-pool-proxy"

	// PoolProxyURL is the address sandbox forwarders dial.
	PoolProxyURL = "https://" + ServerName + ":17080"

	// SandboxProxyMount is where per-sandbox proxy material is mounted inside the
	// sandbox container.
	SandboxProxyMount = "/etc/discobox/proxy"

	// SystemCABundle is the Debian system CA bundle inside the sandbox. The
	// boot-time trust step adds the MITM CA to it via update-ca-certificates.
	SystemCABundle = "/etc/ssl/certs/ca-certificates.crt"

	// SandboxForwarderListen is the sandbox-local forwarder address that sandbox
	// processes use as their HTTP/HTTPS/ALL proxy.
	SandboxForwarderListen = "127.0.0.1:17008"

	// SandboxBuildkitBridgeListen is where a sandbox-local forwarder accepts
	// plaintext connections for the pool's BuildKit mediator.
	//
	// It exists for the same reason the HTTP forwarder does: the client cannot
	// present the mTLS certificate itself. buildx runs as the sandbox user,
	// while the client key is root-owned — reading it is not something a
	// sandbox user can do, and the key must not be world-readable just to make
	// one tool work. The forwarder runs as root, holds the key, and speaks
	// plaintext to loopback, which is private to this sandbox.
	SandboxBuildkitBridgeListen = "127.0.0.1:17082"

	// BuildkitMediatorURL is the pool endpoint that forwarder dials. The port
	// mirrors buildkitagent.MediatorListen; it is duplicated rather than
	// imported to keep the proxy wiring independent of the builder's.
	BuildkitMediatorURL = "https://" + ServerName + ":17081"

	// UnitEnvironmentFile is read by the proxy systemd unit. The pool agent
	// process writes it so the unit, which runs with a clean systemd
	// environment, learns which pool it serves and how to reach the control
	// plane.
	UnitEnvironmentFile = "/etc/discobox/proxy.env"
)

// UpstreamProxyEnvVarNames are the proxy-address variables forwarded to the
// proxy unit. They mirror proxy.UpstreamProxyEnvVars, which is what the proxy
// reads when deciding whether it must chain through another proxy.
var UpstreamProxyEnvVarNames = proxy.UpstreamProxyEnvVars

// PoolsRoot is the parent of every one of a project's pools' proxy subtrees on
// the host. It is enumerated by the pool-sync reaper to find pools whose
// material lingers with no live pool.
//
// It is project-scoped to match the reaper's authority: the control plane hands
// a pool agent the authoritative pool set for one project's provider instance,
// so the tree that agent scans must contain only that project's pools. A
// host-global pools root would put another project's live pools in scope, and
// the reaper would delete the proxy material out from under running sandboxes.
// This mirrors the per-project scoping of the sandbox data root.
func PoolsRoot(projectID string) string {
	return layout.ProxyProjectPools(projectID)
}

// PoolProxyRoot is one pool's entire proxy subtree (material for all its
// sandboxes). Reaping it removes that pool's proxy footprint in one shot.
func PoolProxyRoot(projectID, poolID string) string {
	return layout.ProxyPool(projectID, poolID)
}

// PoolSandboxMaterialRoot is the per-pool root under which each sandbox's
// bind-mounted proxy material is staged. It is project- and pool-scoped so
// that, on a host daemon shared by multiple pools, a pool's orphan scan only
// ever sees — and reaps — its own sandboxes' material, never another pool's
// live material.
func PoolSandboxMaterialRoot(projectID, poolID string) string {
	return layout.ProxyPoolSandboxes(projectID, poolID)
}

// SandboxNetworkName is the per-pool internal Docker network that carries
// sandbox egress to the pool proxy. Sandboxes join it (and only it) and
// resolve ServerName via Docker's embedded DNS; the pool joins it aliased as
// ServerName, in addition to its egress network. Being internal, the network
// has no route off-box, so a sandbox can reach only the proxy and DNS.
func SandboxNetworkName(poolID string) string {
	return "discobox-sbnet-" + poolID
}

// WriteUnitEnvironment writes the environment file consumed by the proxy
// systemd unit. It is written to the pool container's own /etc, which is
// shared with the child systemd namespace.
func WriteUnitEnvironment(prefix, controlPlaneURL, projectID, poolID string) error {
	if err := os.MkdirAll(resolve(filepath.Dir(UnitEnvironmentFile)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(resolve(UnitEnvironmentFile),
		[]byte(unitEnvironment(prefix, controlPlaneURL, projectID, poolID)), 0o600)
}

// unitEnvironment renders the proxy unit's environment. It is separate from the
// write so its contents can be asserted without touching the container's /etc.
func unitEnvironment(prefix, controlPlaneURL, projectID, poolID string) string {
	content := envHostMountPrefix + "=" + strings.TrimSpace(prefix) + "\n"
	// The proxy unit resolves the same control-plane URL the agent uses, so both
	// reach the control plane over whatever transport its scheme names.
	if url := strings.TrimSpace(controlPlaneURL); url != "" {
		content += envControlPlaneURL + "=" + url + "\n"
	}
	// The unit runs with a clean systemd environment, and every path it writes
	// is scoped to this pool, so without these it cannot address its own state.
	content += envProjectID + "=" + strings.TrimSpace(projectID) + "\n"
	content += envPoolID + "=" + strings.TrimSpace(poolID) + "\n"
	// Propagate this process's own proxy-trust environment. It matters when the
	// pool itself runs inside a Discobox sandbox: the sandbox injects these
	// into the pool container, but systemd resets the environment for units, so
	// the proxy unit would otherwise start with none of them and try to reach
	// origins directly — which a sandbox has no route for. Forwarding them here
	// is what lets the inner proxy chain through the outer one.
	content += proxyEnvironment(os.Environ())
	return content
}

// proxyEnvironment renders the proxy-trust subset of an environment as
// systemd EnvironmentFile lines. Names are matched rather than listed
// separately so this stays in step with what a sandbox actually injects.
func proxyEnvironment(environ []string) string {
	wanted := map[string]bool{}
	for _, name := range append(append([]string{}, UpstreamProxyEnvVarNames...), "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR") {
		wanted[name] = true
	}
	var names []string
	values := map[string]string{}
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !wanted[name] || strings.TrimSpace(value) == "" {
			continue
		}
		if _, seen := values[name]; !seen {
			names = append(names, name)
		}
		values[name] = value
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s=%s\n", name, strconv.Quote(values[name]))
	}
	return b.String()
}

// PrepareBundle creates or reuses the proxy CA bundle and pool server
// certificate. It is idempotent and safe to call from both the pool agent
// startup path and the proxy systemd unit.
func PrepareBundle(projectID, poolID string) (*proxy.CertificateBundle, error) {
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         resolve(layout.ProxyCerts(projectID, poolID)),
		ProxyURL:    PoolProxyURL,
		ServerHosts: []string{ServerName, "127.0.0.1", "localhost"},
	})
	if err != nil {
		return nil, err
	}
	return prepared.Bundle, nil
}

// RunProxy prepares certificates and runs the pool proxy server until ctx is
// canceled. It is the entrypoint for the proxy systemd unit.
func RunProxy(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	projectID := strings.TrimSpace(os.Getenv(envProjectID))
	poolID := strings.TrimSpace(os.Getenv(envPoolID))
	if projectID == "" || poolID == "" {
		return fmt.Errorf("proxy unit environment names no pool (%s/%s)", envProjectID, envPoolID)
	}
	bundle, err := PrepareBundle(projectID, poolID)
	if err != nil {
		return fmt.Errorf("prepare proxy certificates: %w", err)
	}
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = ListenAddress
	cfg.PublicURL = PoolProxyURL
	cfg.CertDir = resolve(layout.ProxyCerts(projectID, poolID))
	// Everything below records this pool's own traffic, so it is pool-scoped:
	// pools from different projects can share one Docker daemon, and a shared
	// audit database would interleave their request histories.
	cfg.DatabaseDSN = resolve(layout.ProxyAuditDB(projectID, poolID))
	cfg.Cache.Dir = resolve(layout.ProxyCache(projectID, poolID))
	cfg.Recording.StreamDir = resolve(layout.ProxyStreams(projectID, poolID))
	cfg.Recording.BodyDir = resolve(layout.ProxyBodies(projectID, poolID))

	// The resolver fetches real secret values from the control plane using the
	// scoped token the pool-agent process writes for this pool.
	resolver := newSecretResolver(projectID, poolID)
	server, err := proxy.NewServer(ctx, cfg, bundle, resolver)
	if err != nil {
		return fmt.Errorf("create proxy server: %w", err)
	}
	go watchSecretsFile(ctx, server, cfg, resolve(layout.ProxySecretsFile(projectID, poolID)), func(err error) {
		logger.Warn("apply proxy sentinel config", "error", err)
	})
	errCh := make(chan error, 1)
	go func() {
		logger.Info("pool proxy serving", "addr", ListenAddress)
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		_ = server.Close()
		return ctx.Err()
	case err := <-errCh:
		_ = server.Close()
		return err
	}
}

// SandboxMaterial describes how a sandbox container is wired to the pool
// proxy.
type SandboxMaterial struct {
	// MountSource is the pool-host path (as handed to the container runtime)
	// holding the sandbox's proxy material. It is bind-mounted read-only into
	// the container at SandboxProxyMount.
	MountSource string
	// Env holds the proxy-related environment variables injected into the
	// sandbox so its processes route outbound traffic through the local
	// forwarder and trust the MITM CA.
	Env map[string]string
}

// bridgeConfig is the on-disk config read by the sandbox proxy-bridge service.
// Paths are expressed as seen inside the sandbox container.
type bridgeConfig struct {
	ListenAddress  string `json:"listenAddress"`
	PoolProxyURL   string `json:"workerProxyUrl"`
	MTLSCAPath     string `json:"mtlsCaPath"`
	ClientCertPath string `json:"clientCertPath"`
	ClientKeyPath  string `json:"clientKeyPath"`
}

// validateIDSegment rejects IDs that could escape the directories they become
// path segments of. kind names the ID in the error ("sandbox", "project", …).
func validateIDSegment(kind, id string) error {
	if id == "" {
		return fmt.Errorf("%s ID is required", kind)
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid %s ID %q", kind, id)
	}
	return nil
}

// validateMaterialScope validates the project and pool IDs that scope a
// sandbox's staged material, plus the sandbox ID itself. Every entry point that
// builds a material path validates all three so no caller can walk out of the
// project's proxy subtree.
func validateMaterialScope(projectID, poolID, sandboxID string) error {
	return errors.Join(
		validateIDSegment("project", projectID),
		validateIDSegment("pool", poolID),
		validateIDSegment("sandbox", sandboxID),
	)
}

// RemoveSandboxMaterial deletes the staged proxy material and client certificate
// for sandboxID. It is idempotent: absent directories are not an error.
func RemoveSandboxMaterial(projectID, poolID, sandboxID string) error {
	if err := validateMaterialScope(projectID, poolID, sandboxID); err != nil {
		return err
	}
	var errs []error
	materialDir := filepath.Join(PoolSandboxMaterialRoot(projectID, poolID), sandboxID)
	if err := os.RemoveAll(resolve(materialDir)); err != nil {
		errs = append(errs, fmt.Errorf("remove sandbox proxy material: %w", err))
	}
	// The client certificate is keyed by the globally unique sandbox ID, so
	// removing it by ID never touches another pool's material.
	clientCertDir := filepath.Join(layout.ProxyCerts(projectID, poolID), "clients", sandboxID)
	if err := os.RemoveAll(resolve(clientCertDir)); err != nil {
		errs = append(errs, fmt.Errorf("remove sandbox proxy client certificate: %w", err))
	}
	return errors.Join(errs...)
}

// PruneOrphanedMaterial removes staged proxy material and client certificates
// for sandboxes that are no longer live. liveSandboxIDs is the set of sandbox
// IDs whose containers still exist; anything staged on disk but not in that set
// is an orphan (for example, a container deleted out of band or while the pool
// was down).
//
// minAge protects material that was just staged for an in-flight CreateSandbox:
// an orphan is only removed when its youngest on-disk file predates minAge. Pass
// 0 to prune regardless of age.
func PruneOrphanedMaterial(projectID, poolID string, liveSandboxIDs []string, minAge time.Duration) error {
	orphans, scanErr := OrphanedSandboxIDs(projectID, poolID, liveSandboxIDs, minAge)
	var errs []error
	if scanErr != nil {
		errs = append(errs, scanErr)
	}
	for _, id := range orphans {
		if err := RemoveSandboxMaterial(projectID, poolID, id); err != nil {
			errs = append(errs, err)
		}
		if err := RemoveSandboxSentinels(projectID, poolID, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// OrphanedSandboxIDs returns sandbox IDs with persisted pool-local material
// but no live container. The material is the level-triggered record used to
// recover removals whose Docker destroy event was missed while the pool was
// down.
func OrphanedSandboxIDs(projectID, poolID string, liveSandboxIDs []string, minAge time.Duration) ([]string, error) {
	live := make(map[string]struct{}, len(liveSandboxIDs))
	for _, id := range liveSandboxIDs {
		live[id] = struct{}{}
	}

	var errs []error
	candidates := map[string]struct{}{}
	// Only this project's pool-scoped material root is scanned. Client certs
	// (CertDir/clients) and sentinel registrations live in per-host shared
	// locations keyed by sandbox ID; scanning those would surface other pools'
	// and other projects' sandboxes as candidates on a shared daemon. Every sandbox always has a
	// material dir here, so it is a complete record of this pool's sandboxes;
	// the shared cert/sentinel entries are reclaimed by ID when their material
	// orphan is pruned.
	base := PoolSandboxMaterialRoot(projectID, poolID)
	entries, err := os.ReadDir(resolve(base))
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("scan %s: %w", base, err))
		}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			candidates[entry.Name()] = struct{}{}
		}
	}

	cutoff := time.Now().Add(-minAge)
	var orphans []string
	for id := range candidates {
		if _, ok := live[id]; ok {
			continue
		}
		// Ignore names that could not have been produced by EnsureSandboxMaterial
		// rather than risk removing something unexpected.
		if validateIDSegment("sandbox", id) != nil {
			continue
		}
		// Protect material that is still being staged for an in-flight
		// CreateSandbox. Only staged directories carry a grace window; a
		// sentinel-only leftover (its material already gone) is always eligible.
		if minAge > 0 {
			if modTime, hasDir := materialModTime(projectID, poolID, id); hasDir && modTime.After(cutoff) {
				continue
			}
		}
		orphans = append(orphans, id)
	}
	sort.Strings(orphans)
	return orphans, errors.Join(errs...)
}

// materialModTime returns the modification time of a sandbox's staged material
// directory, used to protect material still being staged for an in-flight create.
func materialModTime(projectID, poolID, id string) (time.Time, bool) {
	info, err := os.Stat(resolve(filepath.Join(PoolSandboxMaterialRoot(projectID, poolID), id)))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// EnsureSandboxMaterial issues (or reuses) a client certificate for sandboxID
// and stages the certificate material and bridge config into a per-sandbox
// directory. Paths are the container's own, which is also where the pool
// agent process can actually write to; the returned MountSource is the
// un-resolved path handed to the container runtime as the bind-mount source.
func EnsureSandboxMaterial(projectID, poolID, sandboxID string) (*SandboxMaterial, error) {
	if err := validateMaterialScope(projectID, poolID, sandboxID); err != nil {
		return nil, err
	}
	bundle, err := PrepareBundle(projectID, poolID)
	if err != nil {
		return nil, err
	}
	material, err := proxy.EnsureClientCertificate(bundle, sandboxID, PoolProxyURL, "", clientCertValidity, clientCertRenewBefore)
	if err != nil {
		return nil, fmt.Errorf("ensure sandbox proxy certificate: %w", err)
	}

	mountSource := filepath.Join(PoolSandboxMaterialRoot(projectID, poolID), sandboxID)
	writeDir := mountSource
	if err := os.MkdirAll(resolve(writeDir), 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox proxy material dir: %w", err)
	}

	// Copy only the public CAs and this sandbox's client keypair. Never expose
	// the CA private keys or other sandboxes' material. bundle paths are already
	// resolved because PrepareBundle already created the bundle.
	files := []struct {
		name string
		src  string
		mode os.FileMode
	}{
		{"mtls-ca.crt", bundle.MTLSCAPath, 0o644},
		{"mitm-ca.crt", bundle.MITMCAPath, 0o644},
		{"client.crt", material.ClientCertPath, 0o644},
		{"client.key", material.ClientKeyPath, 0o600},
	}
	for _, f := range files {
		if err := copyFile(filepath.Join(writeDir, f.name), f.src, f.mode); err != nil {
			return nil, err
		}
	}

	bridge := bridgeConfig{
		ListenAddress:  SandboxForwarderListen,
		PoolProxyURL:   PoolProxyURL,
		MTLSCAPath:     filepath.Join(SandboxProxyMount, "mtls-ca.crt"),
		ClientCertPath: filepath.Join(SandboxProxyMount, "client.crt"),
		ClientKeyPath:  filepath.Join(SandboxProxyMount, "client.key"),
	}
	bridgeJSON, err := json.MarshalIndent(&bridge, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(resolve(filepath.Join(writeDir, "bridge.json")), bridgeJSON, 0o600); err != nil {
		return nil, fmt.Errorf("write bridge config: %w", err)
	}

	// Material for the sandbox's second forwarder instance, which serves
	// containers a nested dockerd creates: SandboxForwarderListen is
	// loopback-only from the sandbox's own point of view, so a nested
	// container cannot reach it. Only the upstream and credentials are set
	// here — the listen address belongs to the sandbox.
	// No ListenAddress: the sandbox's nested Docker bridge subnet is chosen by
	// its own dockerd (its daemon.json pins no "bip"), so the address is not
	// knowable out here and is not pool-agent's to decide. sandbox-agent
	// discovers docker0's address at runtime and publishes it; see the
	// sandbox-agent/nestedbridge package.
	dockerBridge := bridgeConfig{
		PoolProxyURL:   PoolProxyURL,
		MTLSCAPath:     filepath.Join(SandboxProxyMount, "mtls-ca.crt"),
		ClientCertPath: filepath.Join(SandboxProxyMount, "client.crt"),
		ClientKeyPath:  filepath.Join(SandboxProxyMount, "client.key"),
	}
	dockerBridgeJSON, err := json.MarshalIndent(&dockerBridge, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(resolve(filepath.Join(writeDir, "bridge-docker.json")), dockerBridgeJSON, 0o600); err != nil {
		return nil, fmt.Errorf("write nested-docker bridge config: %w", err)
	}

	// The sandbox's forwarder for the pool's BuildKit mediator. Unlike the two
	// above it carries a listen address, because loopback inside the sandbox is
	// known here and needs no discovery.
	buildkitBridge := bridgeConfig{
		ListenAddress:  SandboxBuildkitBridgeListen,
		PoolProxyURL:   BuildkitMediatorURL,
		MTLSCAPath:     filepath.Join(SandboxProxyMount, "mtls-ca.crt"),
		ClientCertPath: filepath.Join(SandboxProxyMount, "client.crt"),
		ClientKeyPath:  filepath.Join(SandboxProxyMount, "client.key"),
	}
	buildkitBridgeJSON, err := json.MarshalIndent(&buildkitBridge, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(resolve(filepath.Join(writeDir, "bridge-buildkit.json")), buildkitBridgeJSON, 0o600); err != nil {
		return nil, fmt.Errorf("write buildkit bridge config: %w", err)
	}

	proxyURL := "http://" + SandboxForwarderListen
	env := map[string]string{
		"HTTP_PROXY":  proxyURL,
		"http_proxy":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		"https_proxy": proxyURL,
		"ALL_PROXY":   proxyURL,
		"all_proxy":   proxyURL,
		// The token stands in for the sandbox's own directly-connected
		// networks, which pool-agent cannot know: Docker allocates them, and
		// the nested-Docker bridge does not exist until dockerd first starts.
		// sandbox-agent substitutes the real list when it materializes this
		// env. Without it, traffic to a sandbox's own networks (a pool agent
		// reaching its sandboxes, for one) is sent out through the egress
		// proxy instead of straight there.
		// ServerName is exempted by name, not left to the subnet token. Go's
		// NO_PROXY does honor CIDR entries, but only consults them when the
		// request host is an IP literal (httpproxy.useProxy guards the IP
		// matchers on a successful netip.ParseAddr); it never resolves a name
		// to test it against a subnet. Sandboxes address the pool by its
		// alias, so no CIDR the token expands to can ever match, and a client
		// would route through the sandbox's own egress proxy — which
		// MITMs the connection and presents a certificate signed by the MITM
		// CA, where the caller expects the mTLS CA. That is not hypothetical:
		// it is what stopped buildx reaching the pool's BuildKit mediator,
		// reported as "certificate signed by unknown authority". Tools that
		// ignore the proxy environment (openssl) succeed against the same
		// endpoint, which makes the failure look like anything but a proxy.
		"NO_PROXY": "127.0.0.1,localhost,::1," + ServerName + "," + sandboxconfig.LocalSubnetsToken,
		"no_proxy": "127.0.0.1,localhost,::1," + ServerName + "," + sandboxconfig.LocalSubnetsToken,
		// Node.js (and Claude Code, which runs on Node), Python's ssl module
		// and requests/certifi, and pip all bundle their own root store and
		// ignore the system bundle, so each needs pointing at it explicitly.
		// It is the system bundle (not just the raw MITM CA) in every case, so
		// the same value also validates directly-reached (NO_PROXY) TLS, not
		// just MITM-intercepted TLS. See docs/adr/0020: sandbox-agent's runc
		// wrapper mounts the identical bundle at SystemCABundle inside a
		// nested Docker container too, so this value is reusable there
		// unchanged once named in ProxyEnvs.
		"NODE_EXTRA_CA_CERTS": SystemCABundle,
		"SSL_CERT_FILE":       SystemCABundle,
		"REQUESTS_CA_BUNDLE":  SystemCABundle,
		"PIP_CERT":            SystemCABundle,
	}
	// curl, git, wget, and the OpenSSL CLI read the system bundle directly, so
	// the boot-time update-ca-certificates step covers them without env vars
	// — and so does sandbox-agent's runc wrapper mounting the same bundle
	// path into a nested container.
	//
	// Sandbox daemons systemd starts directly (dockerd, notably) never inherit
	// this map: they are units, and units never inherit the container's own
	// env. That is not solved here — pool-agent cannot resolve
	// sandboxconfig.LocalSubnetsToken, only sandbox-agent can, so sandbox-agent
	// derives its own systemd EnvironmentFile from sandbox.json's Env/ProxyEnvs
	// at boot. See sandbox-agent's proxyenv package and
	// discobox-render-proxy-env.service.
	return &SandboxMaterial{MountSource: mountSource, Env: env}, nil
}

// copyFile copies proxy material into a per-sandbox directory. dst is always
// under the validated per-sandbox material directory, and public CA
// certificates are intentionally world-readable so non-root sandbox tools can
// trust the MITM CA.
func copyFile(dst, src string, mode os.FileMode) error {
	data, err := os.ReadFile(resolve(src))
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	//nolint:gosec // dst is under a validated per-sandbox dir; public CAs are 0644 by design.
	if err := os.WriteFile(resolve(dst), data, mode); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}
