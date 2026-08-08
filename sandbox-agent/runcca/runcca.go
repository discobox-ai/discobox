// Package runcca injects the sandbox's MITM CA into every container a nested
// Docker daemon creates, by adjusting the OCI spec just before runc starts it.
//
// # Why a runc wrapper rather than an NRI plugin
//
// This replaces the NRI plugin of docs/adr/0015, which never ran: containerd
// invokes NRI hooks only from its CRI path, and dockerd drives the plain
// containerd client API instead, so CreateContainer was never called for any
// container a sandbox user created. Enabling the containerd image store
// unified where *layers* live, but not where containers are *executed*, so it
// did not produce the single interception point that ADR assumed.
//
// The runc binary is the one place every path converges. Both routes reach it,
// with different verbs, which is why both are handled:
//
//   - `docker run`   -> containerd shim -> `runc create --bundle <dir>`
//   - `docker build` -> BuildKit executor -> `runc run --bundle <dir>`
//
// # Why a seeded directory rather than a bind over the bundle file
//
// The obvious approach — bind the sandbox's CA bundle straight onto
// /etc/ssl/certs/ca-certificates.crt — makes that path a mount point, and
// Debian's update-ca-certificates replaces the bundle with rename(), which
// fails EBUSY over a mount point. Any Dockerfile doing `apt-get install
// ca-certificates` then dies with a dpkg error. That is not hypothetical: it
// broke this repository's own pool-agent image.
//
// Instead, each trust store is seeded as a *directory*: its contents are
// copied to a per-container staging directory under /run (already tmpfs, so
// the copy is cheap and never outlives the sandbox), the MITM CA is appended
// to the aggregate bundle there, and the staging directory is bind-mounted
// read-write over the original. rename() inside it then works normally.
//
// The raw CA is additionally dropped at each distro's "source anchor" path.
// Those directories are read, never rewritten, by the update tools, so binding
// a file there is safe — and it means a container that regenerates its bundle
// picks the MITM CA back up instead of dropping it.
//
// Nothing here writes into the container's own filesystem, so the MITM CA is
// never captured into a committed image layer.
//
// # Blind injection
//
// Every known trust store is seeded unconditionally rather than detecting a
// distro: seeding a path an image never reads costs nothing. Anything the
// container already declares is left alone, so an explicit user choice wins.
package runcca

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/obot-platform/discobox/sandbox-agent/nestedbridge"
	"github.com/obot-platform/discobox/sandboxconfig"
)

const (
	// DefaultCABundleDir is where discobox-trust-ca.service stages
	// already-updated bundles at boot, one per distro path convention. These
	// are used only when an image ships no bundle of its own.
	DefaultCABundleDir = "/run/discobox/proxy/ca-bundles"
	// DefaultMITMCA is the raw proxy CA, appended to whatever bundle an image
	// already has so the image's own trust set is preserved rather than
	// replaced.
	DefaultMITMCA = "/etc/discobox/proxy/mitm-ca.crt"
	// DefaultSandboxJSON is pool-agent's read-only sandbox manifest.
	DefaultSandboxJSON = "/etc/discobox/sandbox.json"
	// DefaultLoopbackBridge describes the forwarder the sandbox's own
	// processes use; its address is what the manifest's proxy vars point at.
	DefaultLoopbackBridge = "/etc/discobox/proxy/bridge.json"
	// DefaultNestedBridge is where the bridge-facing forwarder publishes the
	// address it actually bound. It is a runtime file, not configuration:
	// dockerd chooses its own default-bridge subnet (daemon.json pins no
	// "bip"), so the address is only known once docker0 exists. An absent file
	// means no forwarder is up yet, and callers degrade rather than guess.
	DefaultNestedBridge = nestedbridge.DefaultPublishPath
	// DefaultStagingRoot holds per-container seeded trust stores. /run is
	// tmpfs, so these are ephemeral and never land on durable storage.
	DefaultStagingRoot = "/run/discobox/runc-ca"
)

// Config locates everything the adjustment reads. Zero fields take the
// Default* paths above.
type Config struct {
	SandboxJSON    string
	CABundleDir    string
	MITMCA         string
	LoopbackBridge string
	NestedBridge   string
	StagingRoot    string
}

func (c Config) withDefaults() Config {
	if c.SandboxJSON == "" {
		c.SandboxJSON = DefaultSandboxJSON
	}
	if c.CABundleDir == "" {
		c.CABundleDir = DefaultCABundleDir
	}
	if c.MITMCA == "" {
		c.MITMCA = DefaultMITMCA
	}
	if c.LoopbackBridge == "" {
		c.LoopbackBridge = DefaultLoopbackBridge
	}
	if c.NestedBridge == "" {
		c.NestedBridge = DefaultNestedBridge
	}
	if c.StagingRoot == "" {
		c.StagingRoot = DefaultStagingRoot
	}
	return c
}

// trustStore is a directory-shaped trust store: the aggregate bundle lives
// inside it, so seeding the whole directory keeps rename() working.
type trustStore struct {
	dir string // path inside the container
	// bundle is the aggregate file within dir that TLS clients read.
	bundle string
	// staged is the pre-built full bundle to fall back on when the image ships
	// no bundle of its own (a distroless or scratch-derived image).
	staged string
}

// trustStores covers Debian/Ubuntu and Alpine (whose /etc/ssl/cert.pem is a
// symlink into /etc/ssl/certs, so seeding that directory covers it), plus the
// RHEL family.
func trustStores() []trustStore {
	return []trustStore{
		{dir: "/etc/ssl/certs", bundle: "ca-certificates.crt", staged: "debian.pem"},
		{dir: "/etc/pki/tls/certs", bundle: "ca-bundle.crt", staged: "rhel.pem"},
	}
}

// anchorDirs are the "drop a .crt here" directories each family's update tool
// reads when it regenerates a bundle. Placing the CA here is what keeps a
// container that runs update-ca-certificates from dropping our CA.
func anchorDirs() []string {
	return []string{
		"/usr/local/share/ca-certificates", // Debian/Ubuntu, Alpine
		"/etc/pki/ca-trust/source/anchors", // RHEL family
	}
}

// Adjust rewrites bundleDir/config.json in place, seeding this container's
// trust stores under cfg.StagingRoot/containerID. It reports whether the spec
// was changed.
//
// The spec is decoded into a generic map rather than typed structs so every
// field this code does not model round-trips untouched: the OCI spec is large
// and still growing, and silently dropping a field runc depends on would be
// far worse than not injecting at all.
func Adjust(bundleDir, containerID string, cfg Config) (bool, error) {
	cfg = cfg.withDefaults()
	path := filepath.Join(bundleDir, "config.json")

	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read oci spec %s: %w", path, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return false, fmt.Errorf("parse oci spec %s: %w", path, err)
	}

	changed, err := addTrustMounts(spec, bundleDir, containerID, cfg)
	if err != nil {
		return false, err
	}
	env, err := proxyEnv(cfg)
	if err != nil {
		return false, err
	}
	if addEnv(spec, env) {
		changed = true
	}
	if !changed {
		return false, nil
	}

	out, err := json.Marshal(spec)
	if err != nil {
		return false, fmt.Errorf("encode oci spec: %w", err)
	}
	// Write via a sibling temp file and rename: runc may be starting other
	// containers from neighboring bundles concurrently, and a partially
	// written spec would be unparseable rather than merely un-injected.
	tmp := path + ".discobox.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, fmt.Errorf("write oci spec: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("replace oci spec: %w", err)
	}
	return true, nil
}

// Cleanup removes a container's staging directory. It is safe to call for a
// container that never had one.
func Cleanup(containerID string, cfg Config) error {
	cfg = cfg.withDefaults()
	if containerID == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(cfg.StagingRoot, containerID))
}

// addTrustMounts seeds each trust store and appends the resulting bind mounts.
func addTrustMounts(spec map[string]any, bundleDir, containerID string, cfg Config) (bool, error) {
	ca, err := os.ReadFile(cfg.MITMCA)
	if err != nil {
		// No MITM proxy configured for this sandbox: nothing to inject, and
		// that is a normal local-development state rather than an error.
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read mitm ca %s: %w", cfg.MITMCA, err)
	}
	if containerID == "" {
		return false, errors.New("no container id on the runc command line")
	}

	rootfs := specRootPath(spec, bundleDir)
	staging := filepath.Join(cfg.StagingRoot, containerID)
	existing := existingMountDests(spec)

	var mounts []any
	if current, ok := spec["mounts"].([]any); ok {
		mounts = current
	}
	changed := false

	for _, store := range trustStores() {
		if _, taken := existing[store.dir]; taken {
			continue // the container brought its own; leave it alone
		}
		dst := filepath.Join(staging, store.dir)
		if err := seedTrustStore(filepath.Join(rootfs, store.dir), dst, store, ca, cfg); err != nil {
			return changed, err
		}
		mounts = append(mounts, map[string]any{
			"destination": store.dir,
			"type":        "bind",
			"source":      dst,
			// Read-write so update-ca-certificates can rename within it. The
			// directory is per-container tmpfs, so nothing written here is
			// visible to any other container or to a committed layer.
			"options": []any{"rbind", "rw"},
		})
		changed = true
	}

	for _, dir := range anchorDirs() {
		dest := filepath.Join(dir, AnchorFileNameFor(ca))
		if _, taken := existing[dest]; taken {
			continue
		}
		src := filepath.Join(staging, "anchors", strings.ReplaceAll(strings.TrimPrefix(dir, "/"), "/", "_")+".crt")
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			return changed, fmt.Errorf("stage anchor dir: %w", err)
		}
		//nolint:gosec // A trust anchor is public and must be readable by every user in the container.
		if err := os.WriteFile(src, ca, 0o644); err != nil {
			return changed, fmt.Errorf("stage anchor %s: %w", src, err)
		}
		mounts = append(mounts, map[string]any{
			"destination": dest,
			"type":        "bind",
			"source":      src,
			"options":     []any{"rbind", "ro"},
		})
		changed = true
	}

	if changed {
		spec["mounts"] = mounts
	}
	return changed, nil
}

// specRootPath resolves the container's root filesystem from the spec.
//
// It is NOT simply bundleDir/rootfs: the OCI spec says root.path may be
// absolute or relative to the bundle, and containerd's snapshotter uses an
// absolute path into /var/lib/docker while leaving bundleDir/rootfs an empty
// directory. Assuming the bundle-relative path silently finds nothing, which
// makes every seeded trust store fall back to the staged bundle and quietly
// discards whatever CAs the image itself shipped.
func specRootPath(spec map[string]any, bundleDir string) string {
	root, ok := spec["root"].(map[string]any)
	if !ok {
		return filepath.Join(bundleDir, "rootfs")
	}
	path, ok := root["path"].(string)
	if !ok || path == "" {
		return filepath.Join(bundleDir, "rootfs")
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(bundleDir, path)
}

// seedTrustStore copies an image's trust store into staging and appends the
// MITM CA to its aggregate bundle, preserving whatever CAs the image shipped
// rather than replacing them.
func seedTrustStore(src, dst string, store trustStore, ca []byte, cfg Config) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create staging dir %s: %w", dst, err)
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}

	bundle := filepath.Join(dst, store.bundle)
	body, err := os.ReadFile(bundle)
	switch {
	case err == nil:
		// The image has its own bundle: keep it and append ours.
		if len(body) > 0 && !strings.HasSuffix(string(body), "\n") {
			body = append(body, '\n')
		}
		body = append(body, ca...)
	case os.IsNotExist(err):
		// No bundle in the image (distroless, scratch): fall back to the full
		// staged bundle, which already contains the MITM CA.
		staged, readErr := os.ReadFile(filepath.Join(cfg.CABundleDir, store.staged))
		if readErr != nil {
			// Nothing staged either; the raw CA alone still establishes trust
			// for the proxy, which is the only thing this exists to do.
			body = ca
		} else {
			body = staged
		}
	default:
		return fmt.Errorf("read image bundle %s: %w", bundle, err)
	}
	//nolint:gosec // A CA bundle is public and must be readable by every user in the container.
	if err := os.WriteFile(bundle, body, 0o644); err != nil {
		return fmt.Errorf("write staged bundle %s: %w", bundle, err)
	}
	return nil
}

// copyTree copies a trust-store directory's entries. Symlinks are recreated as
// symlinks: in a full Debian image most entries are hash symlinks pointing
// either within the directory (still resolvable after the bind) or into
// /usr/share/ca-certificates (still present in the container's own rootfs).
// A missing source is not an error -- the image simply has no such store.
func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read trust store %s: %w", src, err)
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		info, err := os.Lstat(from)
		if err != nil {
			continue // vanished mid-copy; not worth failing a container start
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(from)
			if err != nil {
				continue
			}
			_ = os.Symlink(target, to)
		case info.IsDir():
			if err := os.MkdirAll(to, 0o755); err != nil {
				continue
			}
			if err := copyTree(from, to); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFile(from, to, info.Mode().Perm()); err != nil {
				continue
			}
		}
	}
	return nil
}

func copyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func existingMountDests(spec map[string]any) map[string]struct{} {
	out := map[string]struct{}{}
	mounts, _ := spec["mounts"].([]any)
	for _, m := range mounts {
		if mm, ok := m.(map[string]any); ok {
			if d, ok := mm["destination"].(string); ok {
				out[d] = struct{}{}
			}
		}
	}
	return out
}

// addEnv appends each proxy-trust variable the container does not already set.
func addEnv(spec map[string]any, env map[string]string) bool {
	if len(env) == 0 {
		return false
	}
	process, ok := spec["process"].(map[string]any)
	if !ok {
		return false
	}
	list, _ := process["env"].([]any)
	existing := map[string]struct{}{}
	for _, e := range list {
		if s, ok := e.(string); ok {
			if name, _, found := strings.Cut(s, "="); found {
				existing[name] = struct{}{}
			}
		}
	}

	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic, so the spec is stable across runs

	changed := false
	for _, name := range names {
		if _, taken := existing[name]; taken {
			continue
		}
		list = append(list, name+"="+env[name])
		changed = true
	}
	if changed {
		process["env"] = list
	}
	return changed
}

// proxyEnv returns the manifest's proxy-trust variables with any reference to
// the sandbox-local forwarder rewritten to the nested-Docker one.
//
// The rewrite is by address, not by variable name: inside a container
// 127.0.0.1 is the container's own loopback, so a value pointing at the
// sandbox's loopback forwarder would be unreachable. Both addresses are read
// from the bridge configs pool-agent already writes, so this holds no second
// copy of either the variable names or the addresses.
func proxyEnv(cfg Config) (map[string]string, error) {
	data, err := os.ReadFile(cfg.SandboxJSON)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sandbox config %s: %w", cfg.SandboxJSON, err)
	}
	var manifest sandboxconfig.Config
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse sandbox config %s: %w", cfg.SandboxJSON, err)
	}
	if len(manifest.ProxyEnvs) == 0 {
		return nil, nil
	}

	loopback := bridgeListenAddress(cfg.LoopbackBridge)
	nested := bridgeListenAddress(cfg.NestedBridge)

	env := make(map[string]string, len(manifest.ProxyEnvs))
	for _, name := range manifest.ProxyEnvs {
		value, ok := manifest.Env[name]
		if !ok {
			continue
		}
		// Resolve the local-subnets token first: it is opaque to pool-agent,
		// which wrote it, and this is the point where the real networks are
		// known. Enumerating here rather than at boot matters because the
		// nested-Docker bridge and any user-created networks appear later.
		if strings.Contains(value, sandboxconfig.LocalSubnetsToken) {
			value = strings.ReplaceAll(value, sandboxconfig.LocalSubnetsToken,
				strings.Join(nestedbridge.LocalSubnets(), ","))
			value = strings.Trim(strings.ReplaceAll(value, ",,", ","), ",")
		}
		if loopback != "" && strings.Contains(value, loopback) {
			// This value names the sandbox-local forwarder, which is the
			// container's *own* loopback once inside — unreachable. Rewrite it
			// to the bridge-facing forwarder, or drop it entirely if that
			// forwarder has not published an address yet. Injecting the
			// loopback value unchanged would point every client in the
			// container at nothing, which is worse than leaving it unset:
			// unset means direct egress and a clear failure, set-but-wrong
			// means a confusing hang.
			if nested == "" {
				continue
			}
			value = strings.ReplaceAll(value, loopback, nested)
		}
		env[name] = value
	}
	return env, nil
}

// bridgeListenAddress reads one forwarder's listen address, returning "" when
// the config is absent or unreadable so callers degrade to no rewrite.
func bridgeListenAddress(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		ListenAddress string `json:"listenAddress"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.ListenAddress
}
