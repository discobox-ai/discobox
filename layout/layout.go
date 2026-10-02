// Package layout is the single description of where Discobox stores state.
//
// Every path is resolved against a Root, and the pool's shape picks the root
// (ADR 0144 §1):
//
//   - A pool whose agent runs in a container addresses state as the container
//     sees it, under ContainerRoot. That view is deliberately invariant: the
//     pool agent, the proxy, and the sandbox agent all address state the same
//     way no matter which backend they run on. Only where that root is
//     *mounted from* varies, and only a driver decides it — see HostMapping.
//   - A pool whose agent runs on the host keeps state under the user's own
//     data directory on that machine — see Host. ContainerRoot describes the
//     container's view and is not one of the host roots.
//
// Both shapes lay out the same trees below their root, so everything an
// accessor says about scoping holds for either.
//
// # Scoping
//
// One Docker daemon can host pools from different projects at once: the local
// Docker provider runs every pool on the developer's daemon. Any state written
// to a path shared by two pools is therefore a correctness bug, not merely
// untidy — two pools would overwrite each other's material.
//
// So every accessor here requires the scope it belongs to, and there is
// deliberately no exported accessor that returns a writable unscoped directory
// at all: a caller cannot address pool state without naming the pool.
package layout

import (
	"path"
	"path/filepath"
	"strings"
)

// ContainerRoot is where Discobox state appears inside every container. It does
// not vary by backend, platform, or driver.
const ContainerRoot = "/var/lib/discobox"

// Root is where one process finds Discobox state. The zero value is the
// container's view, Container.
type Root struct {
	// state is the directory the trees below are rooted at, as this process
	// sees it.
	state string
	// system is where the pool container's own filesystem appears to this
	// process: empty for "/", and a directory only for ContainerAt.
	system string
	// host marks a root on the user's own machine, which has no container
	// filesystem around it.
	host bool
	// native marks a root on this machine's own filesystem, whose paths use
	// the platform's separator. The container's view is slash-separated on
	// every platform, including a Windows server rendering a pool's mounts.
	native bool
}

func (r Root) join(elem ...string) string {
	if r.native {
		return filepath.Join(elem...)
	}
	return path.Join(elem...)
}

func (r Root) clean(p string) string {
	if r.native {
		return filepath.Clean(p)
	}
	return path.Clean(p)
}

// Container is the container's view of state: ContainerRoot, and the pool
// container's filesystem at "/". It is the root of every pool whose agent runs
// in a container, and of every server-side caller that hands a pool container
// its mounts.
func Container() Root {
	return Root{}
}

// ContainerAt is the container's view with its whole filesystem relocated under
// dir: state at dir + ContainerRoot, and every system path (System) under dir
// too. It exists for tests, which have no container mounts and cannot write to
// absolute container paths; production code has no reason to relocate a pool
// container's state.
func ContainerAt(dir string) Root {
	dir = filepath.Clean(dir)
	return Root{state: filepath.Join(dir, filepath.FromSlash(ContainerRoot)), system: dir, native: true}
}

// Dir is the directory every tree is rooted at, as this process sees it.
func (r Root) Dir() string {
	if r.state == "" {
		return ContainerRoot
	}
	return r.state
}

// System is p, an absolute path in the pool container's own filesystem outside
// the state trees (its /etc/discobox, /run/discobox, /proc), as this process
// sees it. A host root has no container filesystem: a host pool runs none of
// the units those files configure (ADR 0144 §3), so asking for one is a bug.
func (r Root) System(p string) string {
	if r.host {
		panic("layout: a host root has no container filesystem, so no " + p)
	}
	if r.system == "" || p == "" {
		return p
	}
	return filepath.Join(r.system, filepath.FromSlash(p))
}

// The four trees under a root. They are separate top-level roots so a backend
// can mount them from different storage — durable state and disposable cache
// do not have to share a device.
func (r Root) dataTree() string  { return r.join(r.Dir(), "projects") }
func (r Root) cacheTree() string { return r.join(r.Dir(), "cache") }
func (r Root) proxyTree() string { return r.join(r.Dir(), "proxy") }

// identityTree holds credentials that authenticate a pool to the control
// plane. It is deliberately not under dataTree: the pool-sync reaper and the
// volume reaper both enumerate that tree in order to delete from it, and a
// sandbox's own subtree is derived from it, so a key that authenticates as the
// pool belongs in neither (ADR 0063).
func (r Root) identityTree() string { return r.join(r.Dir(), "identity") }

// MountRoots returns the trees a backend must make available to a pool. Docker
// does not create a missing bind source, so a driver whose host lacks these has
// to create them before the pool container starts.
func (r Root) MountRoots() []string {
	return []string{r.dataTree(), r.cacheTree(), r.proxyTree(), r.identityTree()}
}

// PoolIdentity is one pool's private credential directory. It is pool-scoped
// because a shared host daemon runs every pool's container against the same
// tree.
func (r Root) PoolIdentity(projectID, poolID string) string {
	return r.join(r.identityTree(), projectID, poolID)
}

// PoolIdentityKey is the pool agent's Ed25519 identity key: the key whose
// public half the control plane records as Pool.PublicKey, and whose signature
// authenticates every agent request afterwards.
func (r Root) PoolIdentityKey(projectID, poolID string) string {
	return r.join(r.PoolIdentity(projectID, poolID), "agent.key")
}

// --- durable pool and sandbox state ----------------------------------------

// ProjectData is the root of one project's durable state. It is the shallowest
// path the pool-sync reaper needs, and it is already project-scoped so a reaper
// can never see another project's pools.
func (r Root) ProjectData(projectID string) string {
	return r.join(r.dataTree(), projectID)
}

// ProjectPools is the parent of every pool's durable subtree in a project. The
// reaper enumerates it to find pools with no live counterpart.
func (r Root) ProjectPools(projectID string) string {
	return r.join(r.ProjectData(projectID), "pools")
}

// PoolData is one pool's durable subtree.
func (r Root) PoolData(projectID, poolID string) string {
	return r.join(r.ProjectPools(projectID), poolID)
}

// PoolSandboxes is the parent of every sandbox's tree for one pool. The volume
// reaper scans only here, which is what stops it touching another pool's data.
func (r Root) PoolSandboxes(projectID, poolID string) string {
	return r.join(r.PoolData(projectID, poolID), "sandboxes")
}

// PoolSourceData is the parent of durable data shared by sandboxes in one pool
// according to source identity. Unlike PoolSandboxes, deleting one sandbox
// must not remove anything below this tree.
func (r Root) PoolSourceData(projectID, poolID string) string {
	return r.join(r.PoolData(projectID, poolID), "data-per-source")
}

// SourceData is one source's durable pool-local data directory. sourceKey is
// an opaque key resolved before the pool boundary; layout does not interpret
// source identity.
func (r Root) SourceData(projectID, poolID, sourceKey string) string {
	return r.join(r.PoolSourceData(projectID, poolID), sourceKey)
}

// Sandbox is one sandbox's tree.
func (r Root) Sandbox(projectID, poolID, sandboxID string) string {
	return r.join(r.PoolSandboxes(projectID, poolID), sandboxID)
}

// SandboxData, SandboxConfig, SandboxSecrets, and SandboxSources are the
// per-sandbox subtrees mounted into a sandbox container.
func (r Root) SandboxData(projectID, poolID, sandboxID string) string {
	return r.join(r.Sandbox(projectID, poolID, sandboxID), "data")
}

// SandboxSourceData is the source data a sandbox keeps to itself, mounted at
// `/.discobox/data-per-source/<slug>` when the source has no key to share it
// under — a sandbox with no primary source, or one with no origin. No source
// is a private source: it lives inside SandboxData, at the path it is mounted
// on, so it is the sandbox's alone and survives and travels exactly as the
// rest of that tree does.
func (r Root) SandboxSourceData(projectID, poolID, sandboxID, slug string) string {
	return r.join(r.SandboxData(projectID, poolID, sandboxID), ".discobox", "data-per-source", slug)
}

func (r Root) SandboxConfig(projectID, poolID, sandboxID string) string {
	return r.join(r.Sandbox(projectID, poolID, sandboxID), "config")
}

func (r Root) SandboxSecrets(projectID, poolID, sandboxID string) string {
	return r.join(r.Sandbox(projectID, poolID, sandboxID), "secrets")
}

func (r Root) SandboxSources(projectID, poolID, sandboxID string) string {
	return r.join(r.Sandbox(projectID, poolID, sandboxID), "sources")
}

// SandboxOrigins holds one bare repository per push-delivered source, which the
// client pushes into and the sandbox sees as its `origin` (ADR 0058). Unlike
// the subtrees above it is not itself mounted: each repository under it is
// bound individually, read-only, at /.discobox/origins/<slug>.
func (r Root) SandboxOrigins(projectID, poolID, sandboxID string) string {
	return r.join(r.Sandbox(projectID, poolID, sandboxID), "origins")
}

// --- disposable cache ------------------------------------------------------

// ProjectCachePools is the parent of every pool's cache in a project.
func (r Root) ProjectCachePools(projectID string) string {
	return r.join(r.cacheTree(), "projects", projectID, "pools")
}

// PoolCache is the cache shared by every sandbox one pool runs. It is separate
// from the durable tree so a backend can back it with disposable storage.
//
// It is bind-mounted whole into every sandbox, so everything under it is a
// harness-declared target path mirrored onto the host, and nothing under it is
// privileged. Pool-agent state belongs in PoolBuild instead (ADR 0050).
func (r Root) PoolCache(projectID, poolID string) string {
	return r.join(r.ProjectCachePools(projectID), poolID, "cache")
}

// PoolBuild holds the pool's own build machinery: BuildKit's state and the pool
// registry's blobs. A sibling of PoolCache rather than a child, because no
// sandbox may reach it -- PoolCache is mounted into every sandbox, and mode
// bits are not a boundary against a sandbox user holding sudo (ADR 0050).
//
// Disposable like its sibling: everything here rebuilds from the sandboxes'
// sources.
func (r Root) PoolBuild(projectID, poolID string) string {
	return r.join(r.ProjectCachePools(projectID), poolID, "build")
}

// --- proxy material --------------------------------------------------------

// ProxyCerts holds one pool's proxy CA bundle and server certificate.
//
// It is pool-scoped because a pool's proxy is its own trust domain: the proxy
// runs inside the pool container, on a per-pool internal network, and every
// pool's proxy presents a certificate for the same DNS name. A CA shared across
// pools would leave a sandbox trusting another pool's proxy — a trust boundary
// wider than the isolation boundary it is meant to enforce.
//
// Per-client certificates live below it, keyed by sandbox ID.
func (r Root) ProxyCerts(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "certs")
}

// ProxyControlKey is the private key the pool agent signs proxy control API
// tokens with; the proxy trusts its public half (ADR 0130 §4). It sits in
// ProxyCerts because that directory already holds the MITM CA's private key,
// which is the same custody: readable on the pool, never mounted into a
// sandbox.
func (r Root) ProxyControlKey(projectID, poolID string) string {
	return r.join(r.ProxyCerts(projectID, poolID), "control-ed25519.key")
}

// ProxyProjectPools is the parent of every pool's proxy subtree in a project.
func (r Root) ProxyProjectPools(projectID string) string {
	return r.join(r.proxyTree(), "projects", projectID, "pools")
}

// ProxyPool is one pool's proxy subtree.
func (r Root) ProxyPool(projectID, poolID string) string {
	return r.join(r.ProxyProjectPools(projectID), poolID)
}

// ProxyPoolSandboxes is where one pool stages each sandbox's proxy material.
func (r Root) ProxyPoolSandboxes(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "sandboxes")
}

// ProxyAuditDB is one pool's proxy audit database.
//
// It is pool-scoped because it records the requests that pool's sandboxes made.
// A database shared across pools on one daemon would interleave the audit trails
// of different projects, which is a disclosure problem as much as a storage one.
func (r Root) ProxyAuditDB(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "audit.db")
}

// ProxyCache, ProxyStreams, and ProxyBodies hold one pool's proxy response
// cache and recorded traffic. Each is pool-scoped for the same reason as the
// audit database: the contents are that pool's traffic.
func (r Root) ProxyCache(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "cache")
}

func (r Root) ProxyStreams(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "streams")
}

func (r Root) ProxyBodies(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "bodies")
}

// ProxySecretsFile is the sentinel registry the proxy watches for one pool.
//
// It is pool-scoped because the file names sandboxes belonging to that pool. A
// shared file would be overwritten by whichever pool wrote last, silently losing
// another pool's sentinels.
func (r Root) ProxySecretsFile(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "secrets.json")
}

// ProxyResolveContextFile is the credential the proxy unit reads to resolve
// secrets for one pool.
//
// It is pool-scoped because it carries that pool's ID and its scoped
// secret-resolve token. Sharing it across pools on one daemon would let a pool's
// proxy present another pool's credential — an authorization bug, not just lost
// state.
func (r Root) ProxyResolveContextFile(projectID, poolID string) string {
	return r.join(r.ProxyPool(projectID, poolID), "resolve-context.json")
}

// --- host translation ------------------------------------------------------

// HostMapping translates a path under a container root into the path the
// Docker daemon sees.
//
// The pool agent creates sandbox containers through the daemon, so any path it
// hands over must be valid on the *daemon's* filesystem, which is not
// necessarily where the agent reads and writes. This is the only place that
// difference is expressed: everything else uses the root's own paths.
type HostMapping struct {
	// from is the root mapped from.
	from Root
	// hostRoot is where the daemon sees it, or empty for ContainerRoot.
	hostRoot string
}

// HostMapping maps this root's state directory onto hostRoot. An empty hostRoot
// means the daemon sees state at ContainerRoot, which is the case whenever the
// state root is bind-mounted at the same location in the pool container.
func (r Root) HostMapping(hostRoot string) HostMapping {
	return HostMapping{from: r, hostRoot: strings.TrimRight(strings.TrimSpace(hostRoot), "/")}
}

// HostPath converts a path under the root into the daemon's view of it. Paths
// outside the root are returned unchanged: they are already daemon paths, such
// as a developer's own source directory bound into a sandbox.
func (m HostMapping) HostPath(p string) string {
	if p == "" || (m.hostRoot == "" && m.from.Dir() == ContainerRoot) {
		return p
	}
	rest, ok := strings.CutPrefix(m.from.clean(p), m.from.Dir())
	if !ok || (rest != "" && rest[0] != '/' && rest[0] != filepath.Separator) {
		return p
	}
	return m.HostRoot() + filepath.ToSlash(rest)
}

// HostRoot is where the daemon sees the root's state directory: ContainerRoot
// itself when no translation applies.
func (m HostMapping) HostRoot() string {
	if m.hostRoot == "" {
		return ContainerRoot
	}
	return m.hostRoot
}
