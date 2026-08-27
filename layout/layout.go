// Package layout is the single description of where Discobox stores state.
//
// Every path is expressed as the container sees it, rooted at ContainerRoot.
// That root is deliberately invariant: the pool agent, the proxy, and the
// sandbox agent all address state the same way no matter which backend they run
// on. Only where that root is *mounted from* varies, and only a driver decides
// it — see HostMapping.
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
	"strings"
)

// ContainerRoot is where Discobox state appears inside every container. It does
// not vary by backend, platform, or driver.
const ContainerRoot = "/var/lib/discobox"

// The three trees under ContainerRoot. They are separate top-level roots so a
// backend can mount them from different storage — durable state and disposable
// cache do not have to share a device.
const (
	dataTree  = ContainerRoot + "/projects"
	cacheTree = ContainerRoot + "/cache"
	proxyTree = ContainerRoot + "/proxy"
	// identityTree holds credentials that authenticate a pool to the control
	// plane. It is deliberately not under dataTree: the pool-sync reaper and the
	// volume reaper both enumerate that tree in order to delete from it, and a
	// sandbox's own subtree is derived from it, so a key that authenticates as
	// the pool belongs in neither (ADR 0063).
	identityTree = ContainerRoot + "/identity"
)

// MountRoots returns the trees a backend must make available to a pool. Docker
// does not create a missing bind source, so a driver whose host lacks these has
// to create them before the pool container starts.
func MountRoots() []string {
	return []string{dataTree, cacheTree, proxyTree, identityTree}
}

// PoolIdentity is one pool's private credential directory. It is pool-scoped
// because a shared host daemon runs every pool's container against the same
// tree.
func PoolIdentity(projectID, poolID string) string {
	return path.Join(identityTree, projectID, poolID)
}

// PoolIdentityKey is the pool agent's Ed25519 identity key: the key whose
// public half the control plane records as Pool.PublicKey, and whose signature
// authenticates every agent request afterwards.
func PoolIdentityKey(projectID, poolID string) string {
	return path.Join(PoolIdentity(projectID, poolID), "agent.key")
}

// --- durable pool and sandbox state ----------------------------------------

// ProjectData is the root of one project's durable state. It is the shallowest
// path the pool-sync reaper needs, and it is already project-scoped so a reaper
// can never see another project's pools.
func ProjectData(projectID string) string {
	return path.Join(dataTree, projectID)
}

// ProjectPools is the parent of every pool's durable subtree in a project. The
// reaper enumerates it to find pools with no live counterpart.
func ProjectPools(projectID string) string {
	return path.Join(ProjectData(projectID), "pools")
}

// PoolData is one pool's durable subtree.
func PoolData(projectID, poolID string) string {
	return path.Join(ProjectPools(projectID), poolID)
}

// PoolSandboxes is the parent of every sandbox's tree for one pool. The volume
// reaper scans only here, which is what stops it touching another pool's data.
func PoolSandboxes(projectID, poolID string) string {
	return path.Join(PoolData(projectID, poolID), "sandboxes")
}

// PoolSourceData is the parent of durable data shared by sandboxes in one pool
// according to source identity. Unlike PoolSandboxes, deleting one sandbox
// must not remove anything below this tree.
func PoolSourceData(projectID, poolID string) string {
	return path.Join(PoolData(projectID, poolID), "data-per-source")
}

// SourceData is one source's durable pool-local data directory. sourceKey is
// an opaque key resolved before the pool boundary; layout does not interpret
// source identity.
func SourceData(projectID, poolID, sourceKey string) string {
	return path.Join(PoolSourceData(projectID, poolID), sourceKey)
}

// Sandbox is one sandbox's tree.
func Sandbox(projectID, poolID, sandboxID string) string {
	return path.Join(PoolSandboxes(projectID, poolID), sandboxID)
}

// SandboxData, SandboxConfig, SandboxSecrets, and SandboxSources are the
// per-sandbox subtrees mounted into a sandbox container.
func SandboxData(projectID, poolID, sandboxID string) string {
	return path.Join(Sandbox(projectID, poolID, sandboxID), "data")
}

func SandboxConfig(projectID, poolID, sandboxID string) string {
	return path.Join(Sandbox(projectID, poolID, sandboxID), "config")
}

func SandboxSecrets(projectID, poolID, sandboxID string) string {
	return path.Join(Sandbox(projectID, poolID, sandboxID), "secrets")
}

func SandboxSources(projectID, poolID, sandboxID string) string {
	return path.Join(Sandbox(projectID, poolID, sandboxID), "sources")
}

// SandboxOrigins holds one bare repository per push-delivered source, which the
// client pushes into and the sandbox sees as its `origin` (ADR 0058). Unlike
// the subtrees above it is not itself mounted: each repository under it is
// bound individually, read-only, at /.discobox/origins/<slug>.
func SandboxOrigins(projectID, poolID, sandboxID string) string {
	return path.Join(Sandbox(projectID, poolID, sandboxID), "origins")
}

// --- disposable cache ------------------------------------------------------

// ProjectCachePools is the parent of every pool's cache in a project.
func ProjectCachePools(projectID string) string {
	return path.Join(cacheTree, "projects", projectID, "pools")
}

// PoolCache is the cache shared by every sandbox one pool runs. It is separate
// from the durable tree so a backend can back it with disposable storage.
//
// It is bind-mounted whole into every sandbox, so everything under it is a
// harness-declared target path mirrored onto the host, and nothing under it is
// privileged. Pool-agent state belongs in PoolBuild instead (ADR 0050).
func PoolCache(projectID, poolID string) string {
	return path.Join(ProjectCachePools(projectID), poolID, "cache")
}

// PoolBuild holds the pool's own build machinery: BuildKit's state and the pool
// registry's blobs. A sibling of PoolCache rather than a child, because no
// sandbox may reach it -- PoolCache is mounted into every sandbox, and mode
// bits are not a boundary against a sandbox user holding sudo (ADR 0050).
//
// Disposable like its sibling: everything here rebuilds from the sandboxes'
// sources.
func PoolBuild(projectID, poolID string) string {
	return path.Join(ProjectCachePools(projectID), poolID, "build")
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
func ProxyCerts(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "certs")
}

// ProxyProjectPools is the parent of every pool's proxy subtree in a project.
func ProxyProjectPools(projectID string) string {
	return path.Join(proxyTree, "projects", projectID, "pools")
}

// ProxyPool is one pool's proxy subtree.
func ProxyPool(projectID, poolID string) string {
	return path.Join(ProxyProjectPools(projectID), poolID)
}

// ProxyPoolSandboxes is where one pool stages each sandbox's proxy material.
func ProxyPoolSandboxes(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "sandboxes")
}

// ProxyAuditDB is one pool's proxy audit database.
//
// It is pool-scoped because it records the requests that pool's sandboxes made.
// A database shared across pools on one daemon would interleave the audit trails
// of different projects, which is a disclosure problem as much as a storage one.
func ProxyAuditDB(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "audit.db")
}

// ProxyCache, ProxyStreams, and ProxyBodies hold one pool's proxy response
// cache and recorded traffic. Each is pool-scoped for the same reason as the
// audit database: the contents are that pool's traffic.
func ProxyCache(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "cache")
}

func ProxyStreams(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "streams")
}

func ProxyBodies(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "bodies")
}

// ProxySecretsFile is the sentinel registry the proxy watches for one pool.
//
// It is pool-scoped because the file names sandboxes belonging to that pool. A
// shared file would be overwritten by whichever pool wrote last, silently losing
// another pool's sentinels.
func ProxySecretsFile(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "secrets.json")
}

// ProxyResolveContextFile is the credential the proxy unit reads to resolve
// secrets for one pool.
//
// It is pool-scoped because it carries that pool's ID and its scoped
// secret-resolve token. Sharing it across pools on one daemon would let a pool's
// proxy present another pool's credential — an authorization bug, not just lost
// state.
func ProxyResolveContextFile(projectID, poolID string) string {
	return path.Join(ProxyPool(projectID, poolID), "resolve-context.json")
}

// --- host translation ------------------------------------------------------

// HostMapping translates a container path into the path the Docker daemon sees.
//
// The pool agent creates sandbox containers through the daemon, so any path it
// hands over must be valid on the *daemon's* filesystem, which is not
// necessarily where the agent reads and writes. This is the only place that
// difference is expressed: everything else uses container paths.
type HostMapping struct {
	hostRoot string
}

// NewHostMapping maps ContainerRoot onto hostRoot. An empty hostRoot means the
// daemon sees the same paths the container does, which is the case whenever the
// state root is bind-mounted at the same location.
func NewHostMapping(hostRoot string) HostMapping {
	return HostMapping{hostRoot: strings.TrimRight(strings.TrimSpace(hostRoot), "/")}
}

// HostPath converts a container path under ContainerRoot into the daemon's view
// of it. Paths outside ContainerRoot are returned unchanged: they are already
// daemon paths, such as a developer's own source directory bound into a sandbox.
func (m HostMapping) HostPath(containerPath string) string {
	if m.hostRoot == "" || containerPath == "" {
		return containerPath
	}
	cleaned := path.Clean(containerPath)
	if cleaned != ContainerRoot && !strings.HasPrefix(cleaned, ContainerRoot+"/") {
		return containerPath
	}
	return m.hostRoot + strings.TrimPrefix(cleaned, ContainerRoot)
}

// HostRoot is where the daemon sees ContainerRoot, or ContainerRoot itself when
// no translation applies.
func (m HostMapping) HostRoot() string {
	if m.hostRoot == "" {
		return ContainerRoot
	}
	return m.hostRoot
}
