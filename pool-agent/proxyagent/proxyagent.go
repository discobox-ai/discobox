// Package proxyagent wires the pool-scoped proxy component into the pool
// agent. It owns the per-pool on-disk locations for proxy certificate material,
// runs the proxy server (as a systemd unit inside the pool container), and
// prepares the per-sandbox client material that is distributed into sandbox
// containers.
package proxyagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/proxy"
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

	// EnvAuditRetention overrides how long the pool proxy keeps an audit row and
	// the recorded request/response body or upgraded stream it names. It is set
	// on the pool container by the server, from the backing provider instance's
	// configuration, and read here when the proxy unit starts.
	//
	// The response cache is not covered by it. That cache is keyed by content
	// digest and bounded by a byte ceiling, so a window of time says nothing
	// about what belongs in it.
	EnvAuditRetention = "DISCOBOX_PROXY_AUDIT_RETENTION"

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

	// SystemCABundle is the Debian system CA bundle inside the sandbox. The
	// boot-time trust step adds the MITM CA to it via update-ca-certificates.
	SystemCABundle = "/etc/ssl/certs/ca-certificates.crt"

	// BuildkitMediatorURL is the pool endpoint the sandbox's BuildKit
	// forwarder dials. The forwarder exists because the client cannot present
	// the mTLS certificate itself: buildx runs as the sandbox user, the client
	// key is root's, and the forwarder holds it and speaks plaintext to the
	// sandbox's own loopback (sandboxconfig.SandboxBuildKitListenAddress).
	BuildkitMediatorURL = "https://" + ServerName + ":" + mediatorPort

	// mediatorPort mirrors buildkitagent.MediatorListen's port; it is
	// duplicated rather than imported to keep the proxy wiring independent of
	// the builder's. TestBootstrapNamesTheMediatorsPort holds the two together.
	mediatorPort = "17081"

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
func PoolsRoot(root layout.Root, projectID string) string {
	return root.ProxyProjectPools(projectID)
}

// PoolSandboxMaterialRoot is the per-pool root under which each sandbox's
// bind-mounted proxy material is staged. It is project- and pool-scoped so
// that, on a host daemon shared by multiple pools, a pool's orphan scan only
// ever sees — and reaps — its own sandboxes' material, never another pool's
// live material.
func PoolSandboxMaterialRoot(root layout.Root, projectID, poolID string) string {
	return root.ProxyPoolSandboxes(projectID, poolID)
}

// SandboxNetworkName is the per-pool internal Docker network that carries
// sandbox egress to the pool proxy. Sandboxes join it (and only it) and
// resolve ServerName via Docker's embedded DNS; the pool joins it aliased as
// ServerName, in addition to its egress network. Being internal, the network
// has no route off-box, so a sandbox can reach only the pool: its proxy, and
// the DNS forwarder its external names are resolved through.
func SandboxNetworkName(poolID string) string {
	return "discobox-sbnet-" + poolID
}

// WriteUnitEnvironment writes the environment file consumed by the proxy
// systemd unit. It is written to the pool container's own /etc, which is
// shared with the child systemd namespace.
func WriteUnitEnvironment(root layout.Root, prefix, controlPlaneURL, projectID, poolID, tunnelURL string) error {
	if err := os.MkdirAll(root.System(filepath.Dir(UnitEnvironmentFile)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(root.System(UnitEnvironmentFile),
		[]byte(unitEnvironment(prefix, controlPlaneURL, projectID, poolID, tunnelURL)), 0o600)
}

// unitEnvironment renders the proxy unit's environment. It is separate from the
// write so its contents can be asserted without touching the container's /etc.
func unitEnvironment(prefix, controlPlaneURL, projectID, poolID, tunnelURL string) string {
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
	// The unit serves the edge's tunnels only for a pool behind one.
	if url := strings.TrimSpace(tunnelURL); url != "" {
		content += EnvTunnelURL + "=" + url + "\n"
	}
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
func PrepareBundle(root layout.Root, projectID, poolID string) (*proxy.CertificateBundle, error) {
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         root.ProxyCerts(projectID, poolID),
		ProxyURL:    PoolProxyURL,
		ServerHosts: []string{ServerName, "127.0.0.1", "localhost"},
	})
	if err != nil {
		return nil, err
	}
	return prepared.Bundle, nil
}

// ConfiguredAuditRetention resolves the audit retention window from
// EnvAuditRetention, returning zero when it is unset so the proxy's own default
// applies.
//
// An unparsable or non-positive value is an error rather than a silent
// fallback. The two ways to get this wrong are discarding an audit trail
// someone still needs and never reclaiming anything, and both should be loud.
func ConfiguredAuditRetention() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(EnvAuditRetention))
	if value == "" {
		return 0, nil
	}
	retention, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", EnvAuditRetention, err)
	}
	if retention <= 0 {
		return 0, fmt.Errorf("%s must be greater than 0, got %s", EnvAuditRetention, value)
	}
	return retention, nil
}

// RunProxy prepares certificates and runs the pool proxy server until ctx is
// canceled. It is the entrypoint for the proxy systemd unit, which finds its
// pool's state under root.
func RunProxy(ctx context.Context, root layout.Root, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	projectID := strings.TrimSpace(os.Getenv(envProjectID))
	poolID := strings.TrimSpace(os.Getenv(envPoolID))
	if projectID == "" || poolID == "" {
		return fmt.Errorf("proxy unit environment names no pool (%s/%s)", envProjectID, envPoolID)
	}
	bundle, err := PrepareBundle(root, projectID, poolID)
	if err != nil {
		return fmt.Errorf("prepare proxy certificates: %w", err)
	}
	cfg := proxy.DefaultConfig()
	cfg.ListenAddress = ListenAddress
	cfg.PublicURL = PoolProxyURL
	cfg.CertDir = root.ProxyCerts(projectID, poolID)
	// Everything below records this pool's own traffic, so it is pool-scoped:
	// pools from different projects can share one Docker daemon, and a shared
	// audit database would interleave their request histories.
	cfg.DatabaseDSN = root.ProxyAuditDB(projectID, poolID)
	// Registry blobs are content-addressed and immutable, so caching them is
	// safe and is where nearly all the bytes are. A pool shares one set of pull
	// credentials, which is what makes a pool-wide blob cache sound: the digest
	// carries no notion of who was authorized for it.
	//
	// Scoped to digest-bearing URLs only. There is no TTL here — correct for
	// anything named by its own digest, wrong for anything else — so a tag
	// manifest must never be admitted. It never is: a tag carries no `sha256:`,
	// which is what both the content-aware arm and these patterns key on.
	// A digest-addressed manifest is admitted by the content-aware arm and is
	// as immutable as a blob.
	cfg.Cache.Enabled = true
	cfg.Cache.ContentAware = true
	cfg.Cache.Patterns = []string{`^/v2/.*/blobs/sha256:[a-fA-F0-9]{64}$`, `/blobs/sha256/[a-fA-F0-9]{2}/[a-fA-F0-9]{64}/data$`}
	cfg.Cache.Dir = root.ProxyCache(projectID, poolID)
	cfg.Recording.StreamDir = root.ProxyStreams(projectID, poolID)
	cfg.Recording.BodyDir = root.ProxyBodies(projectID, poolID)
	// A sandbox's rows and recordings deliberately outlive the sandbox, so
	// age and the spool budget below are all that reclaim them.
	retention, err := ConfiguredAuditRetention()
	if err != nil {
		return err
	}
	if retention > 0 {
		cfg.Recording.Retention = retention
	}
	// Age alone bounds the trees by time, not bytes: a busy pool fills its
	// disk well inside the window. The budget is what bounds them by size.
	if err := ConfigureAuditSpoolBudget(&cfg.Recording); err != nil {
		return err
	}
	// The read-only control API the pool agent relays audit reads through
	// (ADR 0130 §4). The agent prepared its key before systemd started this
	// unit; this only reads it.
	cfg.Control = proxyControlConfig(root, projectID, poolID, logger)
	// The discobox API's host, which this proxy never sends to the internet:
	// the resolver's gate answers it from the control plane (ADR 0140 §2).
	cfg.Secrets.GateHost = GateHost
	// The sandboxes' Git origins, their bundle assets and their build
	// registry, which this proxy never sends to the internet either: it
	// forwards each to the pool's listener for it as the sandbox the client
	// certificate names (ADR 26-10-08-561, ADR 26-10-16-423 §13, ADR
	// 26-10-29-160). Assets are read-only, and assets and the registry are on
	// sockets no box can name (AssetsHost, RegistryHost). A registry body is
	// a layer, already on the pool's disk, so none is spooled (§4).
	cfg.PoolHosts = []proxy.PoolHostConfig{
		{Host: OriginsHost, Upstream: "http://" + OriginsListenAddress},
		{Host: AssetsHost, Upstream: wire.UnixURL(root.ProxyAssetsSocket(projectID, poolID)), ReadOnly: true},
		{Host: RegistryHost, Upstream: wire.UnixURL(root.ProxyRegistrySocket(projectID, poolID)), UnspooledBodies: true},
	}

	// The resolver fetches real secret values from the control plane using the
	// scoped token the pool-agent process writes for this pool.
	resolver := newSecretResolver(root, projectID, poolID)
	server, err := proxy.NewServer(ctx, cfg, bundle, resolver)
	if err != nil {
		return fmt.Errorf("create proxy server: %w", err)
	}
	policy := newPolicyPublisher(server, cfg, func(err error) {
		logger.Warn("apply proxy policy", "error", err)
	})
	// Before the listener starts: until it is applied, a restricted sandbox's
	// egress is not filtered at all.
	if err := policy.loadFile(root.ProxySecretsFile(projectID, poolID), resolver); err != nil {
		return err
	}
	go watchSecretsFile(ctx, policy, resolver, root.ProxySecretsFile(projectID, poolID))
	// The pins people approved for this pool's sandboxes, kept in step with
	// the control plane (ADR 0149).
	controlPlane := newControlPlaneCredentials(root, projectID, poolID)
	trusts := newHostTrusts(server, controlPlane, policy, func(err error) {
		logger.Warn("host trusts", "error", err)
	})
	go trusts.run(ctx)
	errCh := make(chan error, 3)
	go func() {
		logger.Info("pool proxy serving", "addr", ListenAddress)
		errCh <- server.ListenAndServe()
	}()
	if cfg.Control.ListenAddress != "" {
		go func() {
			logger.Info("pool proxy control API serving", "addr", cfg.Control.ListenAddress)
			// Logged, not sent to errCh: the control API only serves audit
			// reads, and its failing must not stop the proxy every sandbox's
			// egress goes through.
			if err := server.ListenAndServeControl(ctx); err != nil {
				logger.Warn("pool proxy control API stopped", "error", err)
			}
		}()
	}
	go func() {
		errCh <- serveCredentials(ctx, logger, bundle, controlPlane, root.ProxySecretsFile(projectID, poolID), trusts, server)
	}()
	go func() {
		// Logged, not sent to errCh, for the control API's reason: a sandbox
		// that cannot resolve names can still reach everything through the
		// proxy, which must not stop with it.
		if err := serveDNS(ctx, logger, bundle, server); err != nil {
			logger.Warn("pool sandbox dns stopped", "error", err)
		}
	}()
	// A pool behind an edge is reached only through the tunnels, so their
	// listener stops the unit with it, as the proxy's does.
	tunnelURL, err := ConfiguredTunnelURL()
	if err != nil {
		_ = server.Close()
		return err
	}
	if tunnelURL != "" {
		go func() {
			errCh <- serveTunnels(ctx, logger)
		}()
	}
	select {
	case <-ctx.Done():
		_ = server.Close()
		return ctx.Err()
	case err := <-errCh:
		_ = server.Close()
		return err
	}
}

// SandboxMaterial is what wires a sandbox to the pool proxy: the credential and
// trust its runtime-config document delivers, and the env its bootstrap
// carries.
type SandboxMaterial struct {
	// Proxy is the sandbox's client keypair and the pool's CAs — the dynamic
	// half, delivered in the runtime-config document rather than placed in the
	// sandbox (ADR 26-10-08-127 §1).
	Proxy sandboxconfig.RuntimeProxy
	// Env holds the proxy-related environment variables injected into the
	// sandbox so its processes route outbound traffic through the local
	// forwarder and trust the MITM CA.
	Env map[string]string
}

// PoolEndpoints is where this pool serves a sandbox, as the sandbox's
// bootstrap names it: the far end of each of its bridges. The pool URLs are
// wire URLs: their scheme picks the transport the sandbox dials, and every one
// carries mTLS on top (ADR 0144 §4). A Docker pool's sandboxes reach them over
// TCP by ServerName, which is also the name its one server certificate is
// issued for.
//
// A pool behind an HTTPS edge (tunnelURL, from ConfiguredTunnelURL) names a
// tunnel through the edge for each instead, and nothing else: its sandboxes
// do not choose between the two (ADR 26-10-23-719 §6). The mTLS inside is
// verified for ServerName all the same.
func PoolEndpoints(tunnelURL string) sandboxconfig.PoolEndpoints {
	if tunnelURL != "" {
		return sandboxconfig.PoolEndpoints{
			Proxy:       tunnelURL + tunnelProxyPath,
			Credentials: tunnelURL + tunnelCredentialsPath,
			DNS:         tunnelURL + tunnelDNSPath,
			BuildKit:    tunnelURL + tunnelBuildKitPath,
			ServerName:  ServerName,
		}
	}
	return sandboxconfig.PoolEndpoints{
		Proxy:       PoolProxyURL,
		Credentials: CredentialsURL,
		DNS:         DNSServerURL,
		BuildKit:    BuildkitMediatorURL,
		ServerName:  ServerName,
	}
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
func RemoveSandboxMaterial(root layout.Root, projectID, poolID, sandboxID string) error {
	if err := validateMaterialScope(projectID, poolID, sandboxID); err != nil {
		return err
	}
	var errs []error
	materialDir := filepath.Join(PoolSandboxMaterialRoot(root, projectID, poolID), sandboxID)
	if err := os.RemoveAll(materialDir); err != nil {
		errs = append(errs, fmt.Errorf("remove sandbox proxy material: %w", err))
	}
	// The client certificate is keyed by the globally unique sandbox ID, so
	// removing it by ID never touches another pool's material.
	clientCertDir := filepath.Join(root.ProxyCerts(projectID, poolID), "clients", sandboxID)
	if err := os.RemoveAll(clientCertDir); err != nil {
		errs = append(errs, fmt.Errorf("remove sandbox proxy client certificate: %w", err))
	}
	return errors.Join(errs...)
}

// OrphanedSandboxIDs returns sandbox IDs with persisted pool-local material
// but no live container. The material is the level-triggered record used to
// recover removals whose Docker destroy event was missed while the pool was
// down.
//
// minAge spares a record directory made more recently than that; pass 0 to
// return every candidate. The directory is made once, by the first
// EnsureSandboxMaterial, and a re-stage leaves its age alone, so it never
// protects a sandbox whose container is being replaced: the caller reaps a
// candidate only under the sandbox's lock, which a create holds.
func OrphanedSandboxIDs(root layout.Root, projectID, poolID string, liveSandboxIDs []string, minAge time.Duration) ([]string, error) {
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
	base := PoolSandboxMaterialRoot(root, projectID, poolID)
	entries, err := os.ReadDir(base)
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
		// Spare a record made within minAge (see above).
		if minAge > 0 {
			if modTime, hasDir := materialModTime(root, projectID, poolID, id); hasDir && modTime.After(cutoff) {
				continue
			}
		}
		orphans = append(orphans, id)
	}
	sort.Strings(orphans)
	return orphans, errors.Join(errs...)
}

// materialModTime returns the modification time of a sandbox's material record
// directory, which OrphanedSandboxIDs's minAge is judged by.
func materialModTime(root layout.Root, projectID, poolID, id string) (time.Time, bool) {
	info, err := os.Stat(filepath.Join(PoolSandboxMaterialRoot(root, projectID, poolID), id))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// EnsureSandboxMaterial issues (or reuses) a client certificate for sandboxID
// and returns it with the pool's CAs, for its runtime-config document. Nothing
// is staged for the sandbox to read: the sandbox is delivered the material and
// writes it itself (ADR 0126 §§1, 3).
//
// The per-sandbox directory under PoolSandboxMaterialRoot is still made: it is
// this pool's record of which sandboxes it has issued material for, which the
// orphan reaper reads (OrphanedSandboxIDs).
func EnsureSandboxMaterial(root layout.Root, projectID, poolID, sandboxID string) (*SandboxMaterial, error) {
	if err := validateMaterialScope(projectID, poolID, sandboxID); err != nil {
		return nil, err
	}
	bundle, err := PrepareBundle(root, projectID, poolID)
	if err != nil {
		return nil, err
	}
	material, err := proxy.EnsureClientCertificate(bundle, sandboxID, PoolProxyURL, "", clientCertValidity, clientCertRenewBefore)
	if err != nil {
		return nil, fmt.Errorf("ensure sandbox proxy certificate: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(PoolSandboxMaterialRoot(root, projectID, poolID), sandboxID), 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox proxy material record: %w", err)
	}

	// Only the public CAs and this sandbox's client keypair. Never the CA
	// private keys or other sandboxes' material.
	read := func(path string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		return string(data), nil
	}
	var out sandboxconfig.RuntimeProxy
	for _, piece := range []struct {
		into *string
		path string
	}{
		{&out.MTLSCA, bundle.MTLSCAPath},
		{&out.MITMCA, bundle.MITMCAPath},
		{&out.ClientCert, material.ClientCertPath},
		{&out.ClientKey, material.ClientKeyPath},
	} {
		if *piece.into, err = read(piece.path); err != nil {
			return nil, err
		}
	}

	proxyURL := "http://" + sandboxconfig.SandboxEgressListenAddress
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
		// Node's built-in fetch (undici), and from Node 24 its http/https
		// modules, ignore HTTP_PROXY/HTTPS_PROXY unless this is set, so they
		// connect directly — which a sandbox has no route for — and fail with
		// a bare "fetch failed" (ENETUNREACH). That bites hardest in tools
		// that bundle their own Node runtime, where nobody can install a
		// proxy dispatcher. Versions that predate it ignore it; Node 22 prints
		// an UNDICI-EHPA experimental warning to stderr.
		"NODE_USE_ENV_PROXY": "1",
		// The discobox API, at the host this pool's proxy answers for itself
		// (ADR 0140 §2). The discobox CLI in the image reads DISCOBOX_SERVER;
		// DISCOBOX_API_URL is the same address for anything else. Whether a
		// call there is answered is the control plane's: only a sandbox it
		// lets call the API is (ADR 26-10-27-880 §9).
		"DISCOBOX_API_URL": "https://" + GateHost,
		"DISCOBOX_SERVER":  "https://" + GateHost,
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
	return &SandboxMaterial{Proxy: out, Env: env}, nil
}
