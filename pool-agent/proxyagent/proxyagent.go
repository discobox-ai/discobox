// Package proxyagent wires the pool-scoped proxy component into the pool
// agent. It owns the shared on-disk locations for proxy certificate material,
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
	"strings"
	"time"

	"github.com/obot-platform/discobox/proxy"
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

	// Root holds all proxy runtime state on the pool filesystem. It lives
	// under the pool's /var/lib/discobox volume so both the pool agent
	// process and the proxy systemd unit observe the same files.
	Root = "/var/lib/discobox/proxy"

	// CertDir holds the proxy CA bundle and pool server certificate. It is a
	// single per-host trust root; client certificates are issued under
	// CertDir/clients/<sandboxID> keyed by the globally unique sandbox ID.
	CertDir = Root + "/certs"

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

	// UnitEnvironmentFile is read by the proxy systemd unit. The pool agent
	// process writes it so the unit, which runs with a clean systemd
	// environment, sees the same host-mount prefix.
	UnitEnvironmentFile = "/etc/discobox/proxy.env"
)

// PoolsRoot is the parent of every pool's proxy subtree on the host. It is
// enumerated by the pool-sync reaper to find pools whose material lingers with
// no live pool.
func PoolsRoot() string {
	return Root + "/pools"
}

// PoolProxyRoot is one pool's entire proxy subtree (material for all its
// sandboxes). Reaping it removes that pool's proxy footprint in one shot.
func PoolProxyRoot(poolID string) string {
	return PoolsRoot() + "/" + poolID
}

// PoolSandboxMaterialRoot is the per-pool root under which each sandbox's
// bind-mounted proxy material is staged. It is pool-scoped so that, on a host
// daemon shared by multiple pools, a pool's orphan scan only ever sees — and
// reaps — its own sandboxes' material, never another pool's live material.
func PoolSandboxMaterialRoot(poolID string) string {
	return PoolProxyRoot(poolID) + "/sandboxes"
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
func WriteUnitEnvironment(prefix string) error {
	if err := os.MkdirAll(filepath.Dir(UnitEnvironmentFile), 0o755); err != nil {
		return err
	}
	content := envHostMountPrefix + "=" + strings.TrimSpace(prefix) + "\n"
	return os.WriteFile(UnitEnvironmentFile, []byte(content), 0o600)
}

// HostPathResolver translates a pool container path into the path the calling
// process can read and write. It accounts for the host-mount prefix used when a
// pool shares the host Docker socket.
type HostPathResolver func(string) string

// ResolverFromEnv builds a HostPathResolver from the pool host-mount prefix
// environment variable. The proxy systemd unit uses this because it does not
// receive the runtime's resolver directly.
func ResolverFromEnv() HostPathResolver {
	return Resolver(strings.TrimSpace(os.Getenv(envHostMountPrefix)))
}

// Resolver builds a HostPathResolver for an explicit host-mount prefix.
func Resolver(prefix string) HostPathResolver {
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	return func(p string) string {
		if prefix == "" || p == "" {
			return p
		}
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return p
		}
		return filepath.Join(prefix, strings.TrimPrefix(p, "/"))
	}
}

// PrepareBundle creates or reuses the proxy CA bundle and pool server
// certificate. It is idempotent and safe to call from both the pool agent
// startup path and the proxy systemd unit. hostDirFor may be nil for an
// identity mapping.
func PrepareBundle(hostDirFor HostPathResolver) (*proxy.CertificateBundle, error) {
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         hostDirFor(CertDir),
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
	hostDirFor := ResolverFromEnv()
	bundle, err := PrepareBundle(hostDirFor)
	if err != nil {
		return fmt.Errorf("prepare proxy certificates: %w", err)
	}
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = ListenAddress
	cfg.PublicURL = PoolProxyURL
	cfg.CertDir = hostDirFor(CertDir)
	cfg.DatabaseDSN = hostDirFor(filepath.Join(Root, "audit.db"))
	cfg.Cache.Dir = hostDirFor(filepath.Join(Root, "cache"))
	cfg.Recording.StreamDir = hostDirFor(filepath.Join(Root, "streams"))
	cfg.Recording.BodyDir = hostDirFor(filepath.Join(Root, "bodies"))

	// The resolver fetches real secret values from the control plane using the
	// scoped token the pool-agent process writes to ResolveContextFile.
	resolver := newSecretResolver(hostDirFor)
	server, err := proxy.NewServer(ctx, cfg, bundle, resolver)
	if err != nil {
		return fmt.Errorf("create proxy server: %w", err)
	}
	go watchSecretsFile(ctx, server, cfg, hostDirFor(SecretsFile), func(err error) {
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

// validateSandboxID rejects IDs that could escape the per-sandbox directories
// they become path segments of.
func validateSandboxID(sandboxID string) error {
	if sandboxID == "" {
		return fmt.Errorf("sandbox ID is required")
	}
	if sandboxID != filepath.Base(sandboxID) || strings.ContainsAny(sandboxID, `/\`) || strings.Contains(sandboxID, "..") {
		return fmt.Errorf("invalid sandbox ID %q", sandboxID)
	}
	return nil
}

// RemoveSandboxMaterial deletes the staged proxy material and client certificate
// for sandboxID. It is idempotent: absent directories are not an error.
func RemoveSandboxMaterial(poolID, sandboxID string, hostDirFor HostPathResolver) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	var errs []error
	materialDir := hostDirFor(filepath.Join(PoolSandboxMaterialRoot(poolID), sandboxID))
	if err := os.RemoveAll(materialDir); err != nil {
		errs = append(errs, fmt.Errorf("remove sandbox proxy material: %w", err))
	}
	// The client certificate is keyed by the globally unique sandbox ID, so
	// removing it by ID never touches another pool's material.
	clientCertDir := hostDirFor(filepath.Join(CertDir, "clients", sandboxID))
	if err := os.RemoveAll(clientCertDir); err != nil {
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
func PruneOrphanedMaterial(poolID string, liveSandboxIDs []string, hostDirFor HostPathResolver, minAge time.Duration) error {
	orphans, scanErr := OrphanedSandboxIDs(poolID, liveSandboxIDs, hostDirFor, minAge)
	var errs []error
	if scanErr != nil {
		errs = append(errs, scanErr)
	}
	for _, id := range orphans {
		if err := RemoveSandboxMaterial(poolID, id, hostDirFor); err != nil {
			errs = append(errs, err)
		}
		if err := RemoveSandboxSentinels(hostDirFor, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// OrphanedSandboxIDs returns sandbox IDs with persisted pool-local material
// but no live container. The material is the level-triggered record used to
// recover removals whose Docker destroy event was missed while the pool was
// down.
func OrphanedSandboxIDs(poolID string, liveSandboxIDs []string, hostDirFor HostPathResolver, minAge time.Duration) ([]string, error) {
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	live := make(map[string]struct{}, len(liveSandboxIDs))
	for _, id := range liveSandboxIDs {
		live[id] = struct{}{}
	}

	var errs []error
	candidates := map[string]struct{}{}
	// Only the pool-scoped material root is scanned. Client certs
	// (CertDir/clients) and sentinel registrations live in per-host shared
	// locations keyed by sandbox ID; scanning those would surface other pools'
	// sandboxes as candidates on a shared daemon. Every sandbox always has a
	// material dir here, so it is a complete record of this pool's sandboxes;
	// the shared cert/sentinel entries are reclaimed by ID when their material
	// orphan is pruned.
	base := PoolSandboxMaterialRoot(poolID)
	entries, err := os.ReadDir(hostDirFor(base))
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
		if validateSandboxID(id) != nil {
			continue
		}
		// Protect material that is still being staged for an in-flight
		// CreateSandbox. Only staged directories carry a grace window; a
		// sentinel-only leftover (its material already gone) is always eligible.
		if minAge > 0 {
			if modTime, hasDir := materialModTime(poolID, id, hostDirFor); hasDir && modTime.After(cutoff) {
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
func materialModTime(poolID, id string, hostDirFor HostPathResolver) (time.Time, bool) {
	info, err := os.Stat(hostDirFor(filepath.Join(PoolSandboxMaterialRoot(poolID), id)))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// EnsureSandboxMaterial issues (or reuses) a client certificate for sandboxID
// and stages the certificate material and bridge config into a per-sandbox
// directory. hostDirFor maps a pool container path into the path the pool
// agent process can actually write to; the returned MountSource is the
// un-resolved path handed to the container runtime as the bind-mount source.
func EnsureSandboxMaterial(poolID, sandboxID string, hostDirFor HostPathResolver) (*SandboxMaterial, error) {
	if err := validateSandboxID(sandboxID); err != nil {
		return nil, err
	}
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	bundle, err := PrepareBundle(hostDirFor)
	if err != nil {
		return nil, err
	}
	material, err := proxy.EnsureClientCertificate(bundle, sandboxID, PoolProxyURL, "", clientCertValidity, clientCertRenewBefore)
	if err != nil {
		return nil, fmt.Errorf("ensure sandbox proxy certificate: %w", err)
	}

	mountSource := filepath.Join(PoolSandboxMaterialRoot(poolID), sandboxID)
	writeDir := hostDirFor(mountSource)
	if err := os.MkdirAll(writeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox proxy material dir: %w", err)
	}

	// Copy only the public CAs and this sandbox's client keypair. Never expose
	// the CA private keys or other sandboxes' material. bundle paths are already
	// resolved because PrepareBundle ran with hostDirFor.
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
	if err := os.WriteFile(filepath.Join(writeDir, "bridge.json"), bridgeJSON, 0o600); err != nil {
		return nil, fmt.Errorf("write bridge config: %w", err)
	}

	mitmCA := filepath.Join(SandboxProxyMount, "mitm-ca.crt")
	proxyURL := "http://" + SandboxForwarderListen
	env := map[string]string{
		"HTTP_PROXY":  proxyURL,
		"http_proxy":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		"https_proxy": proxyURL,
		"ALL_PROXY":   proxyURL,
		"all_proxy":   proxyURL,
		"NO_PROXY":    "127.0.0.1,localhost,::1",
		"no_proxy":    "127.0.0.1,localhost,::1",
		// Node.js (and Claude Code, which runs on Node) ship their own root
		// store, so point them at the MITM CA explicitly. NODE_EXTRA_CA_CERTS
		// appends the MITM CA to Node's built-in roots.
		"NODE_EXTRA_CA_CERTS": mitmCA,
		// Python's ssl module and requests/certifi also bundle their own roots.
		// Point them at the system bundle, which the boot-time trust step
		// augments with the MITM CA alongside the real public roots so both
		// intercepted and directly-reached (NO_PROXY) TLS validate.
		"SSL_CERT_FILE":      SystemCABundle,
		"REQUESTS_CA_BUNDLE": SystemCABundle,
	}
	// curl, git, wget, and the OpenSSL CLI read the system bundle directly, so
	// the boot-time update-ca-certificates step covers them without env vars.
	return &SandboxMaterial{MountSource: mountSource, Env: env}, nil
}

// copyFile copies proxy material into a per-sandbox directory. dst is always
// under the validated per-sandbox material directory, and public CA
// certificates are intentionally world-readable so non-root sandbox tools can
// trust the MITM CA.
func copyFile(dst, src string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	//nolint:gosec // dst is under a validated per-sandbox dir; public CAs are 0644 by design.
	if err := os.WriteFile(dst, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}
