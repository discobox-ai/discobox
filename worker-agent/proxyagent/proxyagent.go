// Package proxyagent wires the worker-scoped proxy component into the worker
// agent. It owns the shared on-disk locations for proxy certificate material,
// runs the proxy server (as a systemd unit inside the worker container), and
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
	"strings"
	"time"

	"github.com/obot-platform/discobox/proxy"
)

const (
	clientCertValidity    = 365 * 24 * time.Hour
	clientCertRenewBefore = 30 * 24 * time.Hour
)

const (
	// envHostMountPrefix mirrors workeragent.EnvHostMountPrefix. It is duplicated
	// here to avoid importing the root worker-agent package (which imports this
	// one).
	envHostMountPrefix = "DISCOBOX_WORKER_HOST_MOUNT_PREFIX"

	// Root holds all proxy runtime state on the worker filesystem. It lives
	// under the worker's /var/lib/discobox volume so both the worker agent
	// process and the proxy systemd unit observe the same files.
	Root = "/var/lib/discobox/proxy"

	// CertDir holds the proxy CA bundle and worker server certificate.
	CertDir = Root + "/certs"

	// SandboxMaterialRoot holds per-sandbox client certificate material that is
	// bind-mounted read-only into sandbox containers.
	SandboxMaterialRoot = Root + "/sandboxes"

	// ListenAddress is where the worker proxy accepts mTLS connections. It binds
	// all interfaces so sandbox containers can reach it through the Docker host
	// gateway.
	ListenAddress = "0.0.0.0:17080"

	// ServerName is the stable DNS name presented on the worker proxy server
	// certificate. Sandbox containers resolve it to the worker over the
	// per-worker internal network (Docker embedded DNS + worker network alias),
	// so the mTLS ServerName check stays valid regardless of the worker IP.
	ServerName = "discobox-worker-proxy"

	// WorkerProxyURL is the address sandbox forwarders dial.
	WorkerProxyURL = "https://" + ServerName + ":17080"

	// SandboxProxyMount is where per-sandbox proxy material is mounted inside the
	// sandbox container.
	SandboxProxyMount = "/etc/discobox/proxy"

	// SystemCABundle is the Debian system CA bundle inside the sandbox. The
	// boot-time trust step adds the MITM CA to it via update-ca-certificates.
	SystemCABundle = "/etc/ssl/certs/ca-certificates.crt"

	// SandboxForwarderListen is the sandbox-local forwarder address that sandbox
	// processes use as their HTTP/HTTPS/ALL proxy.
	SandboxForwarderListen = "127.0.0.1:17008"

	// UnitEnvironmentFile is read by the proxy systemd unit. The worker agent
	// process writes it so the unit, which runs with a clean systemd
	// environment, sees the same host-mount prefix.
	UnitEnvironmentFile = "/etc/discobox/proxy.env"
)

// SandboxNetworkName is the per-worker internal Docker network that carries
// sandbox egress to the worker proxy. Sandboxes join it (and only it) and
// resolve ServerName via Docker's embedded DNS; the worker joins it aliased as
// ServerName, in addition to its egress network. Being internal, the network
// has no route off-box, so a sandbox can reach only the proxy and DNS.
func SandboxNetworkName(workerID string) string {
	return "discobox-sbnet-" + workerID
}

// WriteUnitEnvironment writes the environment file consumed by the proxy
// systemd unit. It is written to the worker container's own /etc, which is
// shared with the child systemd namespace.
func WriteUnitEnvironment(prefix string) error {
	if err := os.MkdirAll(filepath.Dir(UnitEnvironmentFile), 0o755); err != nil {
		return err
	}
	content := envHostMountPrefix + "=" + strings.TrimSpace(prefix) + "\n"
	return os.WriteFile(UnitEnvironmentFile, []byte(content), 0o600)
}

// HostPathResolver translates a worker container path into the path the calling
// process can read and write. It accounts for the host-mount prefix used when a
// worker shares the host Docker socket.
type HostPathResolver func(string) string

// ResolverFromEnv builds a HostPathResolver from the worker host-mount prefix
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

// PrepareBundle creates or reuses the proxy CA bundle and worker server
// certificate. It is idempotent and safe to call from both the worker agent
// startup path and the proxy systemd unit. hostDirFor may be nil for an
// identity mapping.
func PrepareBundle(hostDirFor HostPathResolver) (*proxy.CertificateBundle, error) {
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         hostDirFor(CertDir),
		ProxyURL:    WorkerProxyURL,
		ServerHosts: []string{ServerName, "127.0.0.1", "localhost"},
	})
	if err != nil {
		return nil, err
	}
	return prepared.Bundle, nil
}

// RunProxy prepares certificates and runs the worker proxy server until ctx is
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
	cfg.PublicURL = WorkerProxyURL
	cfg.CertDir = hostDirFor(CertDir)
	cfg.DatabaseDSN = hostDirFor(filepath.Join(Root, "audit.db"))
	cfg.Cache.Dir = hostDirFor(filepath.Join(Root, "cache"))
	cfg.Recording.StreamDir = hostDirFor(filepath.Join(Root, "streams"))
	cfg.Recording.BodyDir = hostDirFor(filepath.Join(Root, "bodies"))

	// The resolver fetches real secret values from the control plane using the
	// scoped token the worker-agent process writes to ResolveContextFile.
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
		logger.Info("worker proxy serving", "addr", ListenAddress)
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

// SandboxMaterial describes how a sandbox container is wired to the worker
// proxy.
type SandboxMaterial struct {
	// MountSource is the worker-host path (as handed to the container runtime)
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
	WorkerProxyURL string `json:"workerProxyUrl"`
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
func RemoveSandboxMaterial(sandboxID string, hostDirFor HostPathResolver) error {
	if err := validateSandboxID(sandboxID); err != nil {
		return err
	}
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	var errs []error
	materialDir := hostDirFor(filepath.Join(SandboxMaterialRoot, sandboxID))
	if err := os.RemoveAll(materialDir); err != nil {
		errs = append(errs, fmt.Errorf("remove sandbox proxy material: %w", err))
	}
	clientCertDir := hostDirFor(filepath.Join(CertDir, "clients", sandboxID))
	if err := os.RemoveAll(clientCertDir); err != nil {
		errs = append(errs, fmt.Errorf("remove sandbox proxy client certificate: %w", err))
	}
	return errors.Join(errs...)
}

// PruneOrphanedMaterial removes staged proxy material and client certificates
// for sandboxes that are no longer live. liveSandboxIDs is the set of sandbox
// IDs whose containers still exist; anything staged on disk but not in that set
// is an orphan (for example, a container deleted out of band or while the worker
// was down).
//
// minAge protects material that was just staged for an in-flight CreateSandbox:
// an orphan is only removed when its youngest on-disk file predates minAge. Pass
// 0 to prune regardless of age.
func PruneOrphanedMaterial(liveSandboxIDs []string, hostDirFor HostPathResolver, minAge time.Duration) error {
	if hostDirFor == nil {
		hostDirFor = func(p string) string { return p }
	}
	live := make(map[string]struct{}, len(liveSandboxIDs))
	for _, id := range liveSandboxIDs {
		live[id] = struct{}{}
	}

	var errs []error
	candidates := map[string]struct{}{}
	for _, base := range []string{SandboxMaterialRoot, filepath.Join(CertDir, "clients")} {
		entries, err := os.ReadDir(hostDirFor(base))
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("scan %s: %w", base, err))
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				candidates[entry.Name()] = struct{}{}
			}
		}
	}
	// Sentinel registrations live in a shared file, not a per-sandbox directory,
	// so gather them separately to reclaim entries whose material was already
	// removed.
	if doc, err := readSecretsDoc(hostDirFor(SecretsFile)); err != nil {
		errs = append(errs, err)
	} else {
		for id := range doc.Clients {
			candidates[id] = struct{}{}
		}
	}

	cutoff := time.Now().Add(-minAge)
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
			if modTime, hasDirs := materialModTime(id, hostDirFor); hasDirs && modTime.After(cutoff) {
				continue
			}
		}
		if err := RemoveSandboxMaterial(id, hostDirFor); err != nil {
			errs = append(errs, err)
		}
		if err := RemoveSandboxSentinels(hostDirFor, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// materialModTime returns the newest modification time across a sandbox's staged
// material and client certificate directories.
func materialModTime(id string, hostDirFor HostPathResolver) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, base := range []string{SandboxMaterialRoot, filepath.Join(CertDir, "clients")} {
		info, err := os.Stat(hostDirFor(filepath.Join(base, id)))
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		found = true
	}
	return newest, found
}

// EnsureSandboxMaterial issues (or reuses) a client certificate for sandboxID
// and stages the certificate material and bridge config into a per-sandbox
// directory. hostDirFor maps a worker container path into the path the worker
// agent process can actually write to; the returned MountSource is the
// un-resolved path handed to the container runtime as the bind-mount source.
func EnsureSandboxMaterial(sandboxID string, hostDirFor HostPathResolver) (*SandboxMaterial, error) {
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
	material, err := proxy.EnsureClientCertificate(bundle, sandboxID, WorkerProxyURL, "", clientCertValidity, clientCertRenewBefore)
	if err != nil {
		return nil, fmt.Errorf("ensure sandbox proxy certificate: %w", err)
	}

	mountSource := filepath.Join(SandboxMaterialRoot, sandboxID)
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
		WorkerProxyURL: WorkerProxyURL,
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
