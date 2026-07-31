// Package nrica implements the sandbox's NRI plugin: on every container a
// nested Docker daemon creates inside the sandbox, it mounts the sandbox's
// MITM CA trust bundles and injects proxy-trust env vars, so a user's
// Dockerfile or `docker run` never needs to know the sandbox sits behind a
// MITM proxy. See docs/adr/0015-nested-docker-builds-trust-the-mitm-proxy-via-nri.md.
//
// containerd/nri/pkg/api's Container/PodSandbox messages carry no rootfs or
// bundle-path field, so the plugin has no way to inspect the target image
// before it starts. It compensates by mounting every known bundle
// destination unconditionally rather than detecting which applies: a bind
// mount onto a path an image never reads is a harmless no-op.
package nrica

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/containerd/nri/pkg/api"
	"github.com/obot-platform/discobox/sandboxconfig"
)

// DefaultCABundleDir is where sandbox boot (discobox-trust-ca.service)
// stages already-updated CA bundles for this plugin to mount, one file per
// destination path convention. It sits under /run rather than beside the CA in
// /etc/discobox/proxy, which is a read-only bind mount nothing can be staged
// into; see docs/adr/0015 decision 4.
const DefaultCABundleDir = "/run/discobox/proxy/ca-bundles"

// bundleMount pairs a bundle file staged under a CA bundle directory with
// the path convention of the distro family that reads it.
type bundleMount struct {
	source      string
	destination string
}

// bundleMountsIn builds the bundle mount list for a given CA bundle
// directory; see docs/adr/0015 decision 4. Every mount targets the identical
// PEM bytes at a different distro's expected trust-store path.
func bundleMountsIn(dir string) []bundleMount {
	return []bundleMount{
		{source: dir + "/debian.pem", destination: "/etc/ssl/certs/ca-certificates.crt"},
		{source: dir + "/alpine.pem", destination: "/etc/ssl/cert.pem"},
		{source: dir + "/rhel.pem", destination: "/etc/pki/tls/certs/ca-bundle.crt"},
	}
}

// Plugin is the NRI plugin implementation. It satisfies
// containerd/nri/pkg/stub.CreateContainerInterface.
type Plugin struct {
	logger  *slog.Logger
	proxEnv map[string]string
	bundles []bundleMount
}

// New reads sandboxJSONPath (the sandbox's own /etc/discobox/sandbox.json)
// and returns a ready-to-run Plugin that mounts bundles staged under
// caBundleDir. sandbox.json is read directly, not a value sandbox-agent
// derives and stages separately: /etc/discobox is a read-only mount, sandbox
// boot never rewrites it after pool-agent writes it once, and (unlike an
// earlier version of this plugin) its Env no longer carries anything more
// sensitive than the proxy-trust vars this plugin already needs — so there is
// no separate, narrower file worth maintaining. A missing or unreadable file
// means no MITM proxy is configured; the plugin still runs, it just injects
// no env (mounts are unconditional either way).
func New(logger *slog.Logger, sandboxJSONPath, caBundleDir string) (*Plugin, error) {
	if logger == nil {
		logger = slog.Default()
	}
	env, err := loadProxyEnv(sandboxJSONPath)
	if err != nil {
		return nil, err
	}
	return &Plugin{logger: logger, proxEnv: env, bundles: bundleMountsIn(caBundleDir)}, nil
}

// loadProxyEnv reads sandbox.json and returns the subset of its Env that
// Runtime.ProxyEnvs (docs/adr/0015 decision 3) names.
func loadProxyEnv(sandboxJSONPath string) (map[string]string, error) {
	data, err := os.ReadFile(sandboxJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sandbox config %s: %w", sandboxJSONPath, err)
	}
	var cfg sandboxconfig.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse sandbox config %s: %w", sandboxJSONPath, err)
	}
	if len(cfg.ProxyEnvs) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(cfg.ProxyEnvs))
	for _, name := range cfg.ProxyEnvs {
		if value, ok := cfg.Env[name]; ok {
			env[name] = value
		}
	}
	return env, nil
}

// CreateContainer mounts every staged CA bundle and injects every proxy-trust
// env var into the new container, skipping anything the container already
// sets. It never returns an error: a plugin failure here must not block a
// user's container from starting.
func (p *Plugin) CreateContainer(_ context.Context, _ *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	adjustment := &api.ContainerAdjustment{}

	existingMounts := make(map[string]struct{}, len(container.GetMounts()))
	for _, m := range container.GetMounts() {
		existingMounts[m.GetDestination()] = struct{}{}
	}
	for _, bundle := range p.bundles {
		if _, ok := existingMounts[bundle.destination]; ok {
			continue
		}
		if _, err := os.Stat(bundle.source); err != nil {
			// Not staged on this sandbox (e.g. no MITM proxy configured, or
			// this format was never prepared at boot) — skip, don't fail.
			continue
		}
		adjustment.AddMount(&api.Mount{
			Destination: bundle.destination,
			Type:        "bind",
			Source:      bundle.source,
			Options:     []string{"rbind", "ro"},
		})
	}

	existingEnv := make(map[string]struct{}, len(container.GetEnv()))
	for _, kv := range container.GetEnv() {
		name, _, ok := strings.Cut(kv, "=")
		if ok {
			existingEnv[name] = struct{}{}
		}
	}
	for name, value := range p.proxEnv {
		if _, ok := existingEnv[name]; ok {
			continue
		}
		adjustment.AddEnv(name, value)
	}

	p.logger.Debug("nri create container",
		"container", container.GetName(),
		"mounts", len(adjustment.GetMounts()),
		"env", len(adjustment.GetEnv()))
	return adjustment, nil, nil
}
