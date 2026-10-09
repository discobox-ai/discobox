package sandboxruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxpath"
	"github.com/discobox-ai/discobox/sandboxuser"
	"github.com/discobox-ai/discobox/tarsums"
	"github.com/discobox-ai/x/id"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
	"github.com/discobox-ai/discobox/pool-agent/childproc"
	"github.com/discobox-ai/discobox/pool-agent/execidentity"
	"github.com/discobox-ai/discobox/pool-agent/imagereap"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

const (
	// SandboxAgentPort is the port the sandbox-agent's HTTP API listens on
	// inside every sandbox, which callers of SandboxDialer name to reach it.
	SandboxAgentPort         = 3003
	sandboxAgentReadyTimeout = 30 * time.Second
	// A pass is one container inspect and one loopback GET, so the interval is
	// what actually bounds how late a ready sandbox is noticed. It is short
	// because this is create latency a person waits through, and because a pass
	// lists no containers and costs well under a millisecond.
	sandboxAgentPollInterval = 25 * time.Millisecond

	// The pool host provisions host-backed roots and mounts them at these
	// fixed container paths. The sandbox-agent (running as PID 1) wires
	// everything else from these primary volumes; see ADR 0007.
	sandboxDataMount    = "/.discobox/data"
	sandboxCacheMount   = "/.discobox/cache"
	sandboxConfigMount  = "/.discobox/config"
	sandboxSourcesMount = "/.discobox/sources"
	// Source data is not a primary volume mounted whole. Each source with a
	// resolved data key gets its pool-shared backing directory bound beneath
	// this root by its sandbox-local slug.
	sandboxSourceDataMount = "/.discobox/data-per-source"

	// sandboxSecretsMount is bound outside /run: systemd (PID 1 inside the
	// sandbox) mounts a fresh tmpfs over /run early in boot, which would
	// shadow a Docker bind mount placed directly at /run/discobox/secrets.
	// sandbox-agent's boot process rebinds this onto /run/discobox/secrets
	// after that tmpfs is up, the same way it already does for
	// /.discobox/config -> /etc/discobox. It is a separate mount from the
	// config volume (not nested under it) because it is live-refreshed
	// independently of sandbox.json — a resolved sentinel can change
	// (rotation, grant approval, OAuth refresh) without touching the
	// sandbox's static config (ADR 0012 §3).
	sandboxSecretsMount = "/.discobox/secrets" //nolint:gosec // Filesystem path, not a credential.

	sandboxLabelManaged = "discobox.sandbox.managed"
	sandboxLabelProject = "discobox.project_id"
	sandboxLabelPool    = "discobox.pool_id"
	sandboxLabelSandbox = "discobox.sandbox_id"
	// sandboxLabelSpec records the spec fingerprint the container was built
	// from (ADR 0017 §5). Comparing it is how the runtime decides drift: one
	// check covers the image pin, resources, sources, and anything added to the
	// spec later, because the control plane hashes the whole manifest.
	sandboxLabelSpec = "discobox.spec_fingerprint"
)

var (
	ErrNotFound = errors.New("sandbox not found")
	// ErrNoContainer is a sandbox this pool holds the tree of and has no
	// container for: one being rebuilt, or one whose container was lost and
	// that nothing is rebuilding. Separate from ErrNotFound because the sandbox
	// is not missing, and the answer the caller can act on is repair.
	ErrNoContainer = errors.New("sandbox has no container on this pool: it is being rebuilt, or it needs repair")
	// ErrRepositoryNotFound is the sandbox being there and the repository asked
	// of it not being. Separate from ErrNotFound because the two send a client
	// somewhere completely different: one means the sandbox is gone, the other
	// that this slug names no source on it.
	ErrRepositoryNotFound = errors.New("sandbox git repository not found")
	ErrAlreadyExists      = errors.New("sandbox already exists")
	// ErrWorktreeUnsupported is a sandbox whose image's agent predates serving
	// the sandbox's own repository. Its work is still in it; an upgrade, which
	// keeps the workspace, is what makes it reachable again; SandboxServesWorktree
	// wraps it with the command that does that for the sandbox it names.
	ErrWorktreeUnsupported = errors.New("the sandbox's image predates serving its own repository")
	// ErrImageUnavailable is the pool being unable to obtain the image a
	// sandbox is pinned to. It is not transient: the pin names an image this
	// pool does not have and cannot get, so the way forward is an upgrade that
	// re-pins the sandbox, not another attempt.
	ErrImageUnavailable = errors.New("sandbox image is not available")
)

// Sandbox is the pool-local runtime view of a sandbox instance.
type Sandbox struct {
	ID        string
	SandboxID string
	Status    Status
	Image     string
	CreatedAt time.Time
	StartedAt *time.Time
	StoppedAt *time.Time
	Error     string
	Metadata  map[string]string
	Ports     []AssignedPort
	Env       map[string]string
}

// GitRepositoryLocation is the on-host location of a source's origin
// repository, together with the OS identity that owns it: the owner of the
// developer's Git directory for a live origin, the sandbox user for the bare
// origin the pool holds. The git CGI backend must run as this identity, not as
// the pool-agent process's own identity, or it trips git's dubious-ownership
// check against the repository.
type GitRepositoryLocation struct {
	Path string
	// UID and GID are the owning user. A negative value means the caller
	// should not attempt to change identity — used by the in-memory runtime,
	// whose repository paths are simply owned by whichever user is running
	// the process (there is no sandbox container user to impersonate).
	UID int
	GID int
	// Live marks the developer's own Git directory served as a source's
	// origin, rather than a repository this pool owns (ADR 0126 §4): it is
	// served fetch-only, and advertises HEAD, the branch HEAD names, and Refs.
	Live bool
	// Refs are the refs the source declares, for a live origin.
	Refs []string
}

// AssignedPort describes a runtime-assigned port mapping.
type AssignedPort struct {
	ContainerPort int
	HostPort      int
	HostIP        string
	Protocol      string
}

// Status is the pool-local runtime status.
type Status string

const (
	StatusCreated Status = "created"
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
	StatusFailed  Status = "failed"
	StatusRemoved Status = "removed"
)

// Runtime performs local sandbox operations for one pool agent.
type Runtime interface {
	// ListSandboxes is every sandbox with a container on this pool, running or
	// stopped. It is the runtime half of a sandbox, and an archived one has
	// none — see StoredSandboxIDs for the durable half.
	ListSandboxes(ctx context.Context) ([]*Sandbox, error)
	// StoredSandboxIDs is every sandbox whose durable tree this pool holds,
	// container or not.
	//
	// It is a superset of ListSandboxes and exists because the two halves of a
	// sandbox do not end together: archiving drops the container and keeps the
	// tree by intent (ADR 0022 §6), and a sandbox whose container was lost out
	// of band keeps one too until the reaper's retention expires. Both are
	// still occupying the disk they occupied while running, so anything
	// accounting for storage has to see them (ADR 0071 resource accounting §7).
	StoredSandboxIDs(ctx context.Context) ([]string, error)
	GetSandbox(ctx context.Context, sandboxID string) (*Sandbox, error)
	CreateSandbox(ctx context.Context, req *workerapimodel.PoolSandboxCreateRequest) (*Sandbox, error)
	UpdateSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxUpdateRequest) (*Sandbox, error)
	// ArchiveSandbox drops the sandbox's container and disposable state and
	// keeps its durable tree, so it can be reinstantiated by a later create
	// (ADR 0022 §6).
	ArchiveSandbox(ctx context.Context, sandboxID string) error
	// DeleteSandbox removes the container AND the durable tree, and returns
	// only once the data is gone: the control plane's delete is confirmed
	// rather than accepted (ADR 0022 §3).
	DeleteSandbox(ctx context.Context, sandboxID string) error
	// ExportTree streams the sandbox's durable tree as a tar archive, and
	// ImportTree restores one for a sandbox this pool does not hold yet. They
	// are the two halves of moving a discobox between servers (ADR 0123), and
	// they deal only in the tree: neither touches a container.
	//
	// An export reads `data` and `sources` with the sandbox's own image, which
	// is why it is told the image (ADR 0129 §1).
	ExportTree(ctx context.Context, sandboxID string, image TreeImage) (io.ReadCloser, error)
	ImportTree(ctx context.Context, sandboxID string, tree io.Reader) error
	// SyncKnownPools reaps the agent-created footprint (sandbox containers and
	// host data/proxy subtrees) of any pool on this shared host daemon whose ID
	// is not in knownPoolIDs. It is how a shared-daemon (local docker) setup
	// reclaims whole orphaned pools; the caller is the control plane, which owns
	// the authoritative pool set.
	SyncKnownPools(ctx context.Context, knownPoolIDs []string) error
	// ClearCache stops every running sandbox on this pool, empties the pool's
	// caches — the sandbox cache, the build cache, the registry, the proxy's
	// response cache and unused images — and returns the sandboxes it stopped.
	// It answers once they are empty and starts nothing again afterwards (see
	// clearcache.go).
	ClearCache(ctx context.Context) ([]string, error)
	// Power operations instruct and report acceptance only; the resulting state
	// is published by the state reporter (ADR 0017 §§9-10, see power.go).
	StartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error
	StopSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error
	RestartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error
	// EnsureSandboxRunning starts a stopped sandbox on demand, for the
	// sandbox-directed routes (ADR 0017 §12). awaitContainer says whether a
	// sandbox this pool holds without a container is waited on as a rebuild
	// in progress (ADR 0039 tier 2) or answered at once with ErrNoContainer.
	EnsureSandboxRunning(ctx context.Context, sandboxID string, awaitContainer bool) error
	// SandboxBooting reports whether the sandbox's container is up but its
	// sandbox agent has not answered yet: Docker calls it running, and nothing
	// in it can be reached.
	SandboxBooting(sandboxID string) bool
	// GitOriginPath is the repository behind a source's origin route: the
	// developer's live Git directory when this pool can see it (ADR 0126 §4),
	// and otherwise the bare origin repository the client pushes into
	// (ADR 0058 §3).
	GitOriginPath(ctx context.Context, sandboxID, slug string) (GitRepositoryLocation, error)
	// SandboxServesWorktree is nil when the sandbox's agent serves its own
	// repository, which the worktree route forwards to (ADR 0126 §4), and
	// ErrWorktreeUnsupported when the sandbox runs an agent from before it did.
	SandboxServesWorktree(ctx context.Context, sandboxID string) error
	// SandboxDialer resolves how to reach port inside the sandbox — its agent
	// on SandboxAgentPort, or a port something in it listens on — and returns
	// the dial for it (ADR 0126 §5). A sandbox that cannot be reached at all
	// is an error here, so a caller can answer it before sending anything.
	SandboxDialer(ctx context.Context, sandboxID string, port int) (Dialer, error)
	// ConvergeRuntimeConfig delivers the sandbox's runtime-config document when
	// the revision its agent reports having applied is older than the one the
	// pool has decided (ADR 0126 §3). The status poll calls it with what each
	// sandbox reports, so a delivery that did not land is repaired rather than
	// assumed, and a changed idle timeout or renewed credential reaches a
	// sandbox that is already running.
	ConvergeRuntimeConfig(ctx context.Context, sandboxID string, applied int64) error

	// The rest run until ctx ends, and the agent starts each once.
	//
	// WatchSandboxStates is the state channel (ADR 0017 §10, statereport.go):
	// what the runtime observes about power state, as deltas and as a complete
	// sync on start and on an interval, from whatever source it watches.
	WatchSandboxStates(ctx context.Context, logger *slog.Logger, publish func(context.Context, SandboxStateBatch) error)
	// WatchSandboxProgress holds the sink provisioning progress is reported to
	// by whatever is doing the work (ADR 0039).
	WatchSandboxProgress(ctx context.Context, publish func(context.Context, SandboxProgressObservation) error)
	// WatchSandboxVolumes reaps the durable trees of sandboxes the control
	// plane no longer holds (ADR 26-10-01-876).
	WatchSandboxVolumes(ctx context.Context, logger *slog.Logger, held HeldSandboxes)
	// WatchProxyMaterial reclaims the pool proxy's per-sandbox material for
	// sandboxes this runtime no longer has.
	WatchProxyMaterial(ctx context.Context, logger *slog.Logger)
}

var (
	_ Runtime = (*DockerSandboxRuntime)(nil)
	_ Runtime = (*MemorySandboxRuntime)(nil)
)

// DockerSandboxRuntime launches sandboxes as Docker containers inside a pool.
type DockerSandboxRuntime struct {
	client *client.Client
	// paths judges every path this runtime names inside a sandbox: by the
	// platform its sandboxes run on, the one the pool hosts (ADR 0145 §6).
	paths                 sandboxpath.Paths
	projectID             string
	poolID                string
	controlPlanePublicKey string
	hostMountPrefix       string
	// sharedMemoryBytes sizes each sandbox container's /dev/shm; zero is
	// Docker's 64 MiB default.
	sharedMemoryBytes int64
	// root is where this pool's state is, as this process sees it.
	root layout.Root
	// hostStateRoot is where the daemon sees root's state; empty means it
	// sees the paths root itself names. It is applied, through root, only where a path is
	// handed to the daemon (daemonPath), so root and its translation are one.
	hostStateRoot string
	// powerLocks serializes power operations per sandbox (see power.go).
	powerLocks sync.Map
	// booting holds the boot under way of each sandbox whose container is
	// started and whose sandbox agent has not answered yet (see beginBoot).
	booting sync.Map
	// starts holds every operation that can start a container, so a cache
	// clear can keep sandboxes down while it empties the caches; clearRun is
	// the clear under way, which concurrent requests share (see clearcache.go).
	starts   startGate
	clearMu  sync.Mutex
	clearRun *cacheClear
	// statePublisher is the state channel's sink while a watcher is running
	// (see statereport.go). Power operations use it to announce a transition
	// they are about to make.
	statePublisher atomic.Value
	// progressPublisher is the same channel's sink for provisioning progress
	// that has no state transition to hang off — an image pull, above all
	// (see statereport.go, ADR 0039).
	progressPublisher atomic.Value
	// sandboxIdleTimeout is the pool policy's idle timeout, delivered in each
	// sandbox's runtime-config document; zero leaves the sandbox-agent's
	// default (ADR 0108).
	sandboxIdleTimeout time.Duration
	// identityKey is the pool's identity key. It signs runtime-config
	// deliveries, and its public half is in every sandbox's bootstrap
	// (ADR 26-10-08-127 §3).
	identityKey ed25519.PrivateKey
	// runtimeConfigLocks serializes deciding and delivering each sandbox's
	// runtime-config document (see runtimeconfig.go).
	runtimeConfigLocks sync.Map
}

type DockerSandboxRuntimeConfig struct {
	ProjectID             string
	PoolID                string
	ControlPlanePublicKey string
	HostMountPrefix       string
	// Root is where the runtime reads and writes pool state: the pool
	// container's, layout.Container().
	Root layout.Root
	// HostStateRoot is where this pool's Docker daemon sees Root's state.
	// Empty means it sees the paths Root itself names.
	HostStateRoot string
	// SandboxIdleTimeout is how long a sandbox runs idle before it powers
	// itself off (ADR 0108). Zero leaves the sandbox-agent's default.
	SandboxIdleTimeout time.Duration
	// IdentityKey is the pool's identity key, which signs the runtime-config
	// documents the pool delivers its sandboxes (ADR 26-10-08-127 §3).
	IdentityKey ed25519.PrivateKey
	// SharedMemoryBytes is the size of every sandbox container's /dev/shm.
	// Zero leaves Docker's default of 64 MiB.
	SharedMemoryBytes int64
	// Platform is the one platform the pool hosts, which every sandbox it
	// runs is (ADR 0145 §1), and so the one every path inside them is judged
	// by.
	Platform platform.Platform
}

func NewDockerSandboxRuntime(cfg DockerSandboxRuntimeConfig) (*DockerSandboxRuntime, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &DockerSandboxRuntime{
		client:                cli,
		paths:                 sandboxpath.For(cfg.Platform),
		projectID:             cfg.ProjectID,
		poolID:                cfg.PoolID,
		controlPlanePublicKey: cfg.ControlPlanePublicKey,
		sandboxIdleTimeout:    cfg.SandboxIdleTimeout,
		identityKey:           cfg.IdentityKey,
		sharedMemoryBytes:     cfg.SharedMemoryBytes,
		hostMountPrefix:       cleanAbsPath(cfg.HostMountPrefix),
		root:                  cfg.Root,
		hostStateRoot:         cfg.HostStateRoot,
	}, nil
}

func (r *DockerSandboxRuntime) ListSandboxes(ctx context.Context) ([]*Sandbox, error) {
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.filters("")})
	if err != nil {
		return nil, err
	}
	out := make([]*Sandbox, 0, len(containers.Items))
	for _, ctr := range containers.Items {
		inspect, err := r.client.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, err
		}
		out = append(out, r.sandboxFromInspect(ctx, inspect.Container))
	}
	return out, nil
}

// StoredSandboxIDs reads the per-sandbox volume root, which is the same tree
// the volume reaper scans and is scoped to this pool — so it can never see
// another pool's data even on a shared daemon.
func (r *DockerSandboxRuntime) StoredSandboxIDs(context.Context) ([]string, error) {
	return storedSandboxIDs(r.sandboxesRoot())
}

// storedSandboxIDs is the pool-scoped core of the above, split out the way the
// reaper's is: root is the caller's, so it is testable without the absolute
// container path the runtime addresses its disk by.
//
// Every directory under it is a sandbox that has taken disk here, whatever
// became of its container. The names are deliberately not filtered against the
// control plane's list: the agent reports what is on its disk, and which of
// those still answer to a sandbox is the control plane's judgement to make,
// against the rows it holds.
func storedSandboxIDs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			// A pool that has never created a sandbox has no root yet, which
			// is no sandboxes rather than a failure.
			return nil, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			ids = append(ids, entry.Name())
		}
	}
	return ids, nil
}

func (r *DockerSandboxRuntime) CreateSandbox(ctx context.Context, req *workerapimodel.PoolSandboxCreateRequest) (*Sandbox, error) {
	sandboxID := ""
	if req != nil {
		sandboxID = strings.TrimSpace(req.SandboxId)
	}
	if sandboxID == "" {
		return nil, fmt.Errorf("sandbox ID is required")
	}
	// Validated before any container work, so a malformed request costs nothing
	// and fails saying what was wrong with it.
	if err := validateCreateRequest(sandboxID, req); err != nil {
		return nil, err
	}
	// A create can start a container, so it waits out a cache clear and holds
	// one off until it is done (see clearcache.go).
	leave, err := r.starts.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	// Replacing a container is a power operation on this sandbox as much as a
	// start or a stop is, and it reads the power state it is preserving
	// (ADR 0021 §3). Taking the same per-sandbox lock is what stops an auto-start
	// (ADR 0017 §12) from racing the replacement — starting the container being
	// removed, or finding none at all.
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	// Before anything reads a source path: a sandbox from before the slug was
	// sent holds its sources under their old names, and every check below --
	// what is materialized, what is mounted, what the git route serves --
	// asks by slug.
	if err := r.adoptSourcePaths(ctx, sandboxID, sandboxSources(r.paths, req)); err != nil {
		return nil, err
	}
	// A container this create replaces takes its power state with it: an upgrade
	// restarts a running sandbox into the new image and leaves a stopped one
	// stopped (ADR 0021 §3).
	replacedRunning := false
	var replaced *Sandbox
	if existing, err := r.GetSandbox(ctx, sandboxID); err == nil {
		drifted, err := r.containerSpecDrifted(ctx, existing, req)
		if err != nil {
			return nil, err
		}
		// The control plane changed this sandbox's spec — an image upgrade
		// (ADR 0021 §1) or any other manifest edit.
		reason := "a spec change"
		if !drifted {
			// The container already exists, but a push-delivered source is only
			// materialized once the client has pushed, which necessarily happens
			// after the container was created and parked. This create is that
			// resume, so settle those sources rather than returning a sandbox
			// whose workspace is still empty.
			r.publishSandboxPhase(ctx, sandboxID, PhaseMaterializingSource)
			rebuild, err := r.settleSources(ctx, existing, req)
			if err != nil {
				return nil, err
			}
			if !rebuild {
				return existing, nil
			}
			reason = "the project configuration its delivered source declares"
		}
		// The container is replaced below, once the image the new one needs is
		// in hand; the sandbox's state lives in the pool-host binds prepared
		// below, not in the container, so it survives.
		replaced = existing
		replacedRunning = existing.Status == StatusRunning
		slog.InfoContext(ctx, "replacing sandbox container",
			"sandboxId", sandboxID,
			"reason", reason,
			"imageDigest", strings.TrimSpace(optString(req.Config.ImageDigest)),
			"specFingerprint", strings.TrimSpace(optString(req.Config.SpecFingerprint)),
			"running", replacedRunning)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	// A container is built at most twice per create: once from the project
	// layer recorded so far, and once more when the sandbox, having cloned its
	// primary source, reads a different one from it (settleSources). The
	// rebuild records the layer it read, so a third would mean the layer
	// changed under the create, which is answered as a failure rather than a
	// loop.
	for builds := 0; ; builds++ {
		sb, rebuild, err := r.buildSandboxContainer(ctx, sandboxID, req, replaced, replacedRunning)
		if err != nil || !rebuild {
			return sb, err
		}
		if builds > 0 {
			return nil, fmt.Errorf("sandbox %s: its project layer changed again after the container was rebuilt for it", sandboxID)
		}
		slog.InfoContext(ctx, "replacing sandbox container", "sandboxId", sandboxID,
			"reason", "the project configuration its delivered source declares", "running", true)
		replaced, replacedRunning = sb, sb.Status == StatusRunning
	}
}

// buildSandboxContainer builds the sandbox's container, replacing replaced
// when there is one, and starts it when the create asks for that or the
// container it replaces was running. A container that started is settled on
// its sources before this returns (settleSources); it reports a rebuild when
// the project layer its primary source declares is not the one it was built
// with, and returns that container for the caller to replace.
func (r *DockerSandboxRuntime) buildSandboxContainer(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxCreateRequest, replaced *Sandbox, replacedRunning bool) (*Sandbox, bool, error) {
	normalizeSandboxConfig(r.paths, &req.Config)
	// Before anything is stopped or pulled: a working directory that names no
	// place in the sandbox fails the create as the container runtime would
	// have, rather than after the running container is gone.
	workingDir, err := sourceWorkingDirectory(r.paths, req)
	if err != nil {
		return nil, false, err
	}
	config := req.Config
	imageName := strings.TrimSpace(optString(config.Image))
	imageName, err = r.resolveSandboxImage(ctx, sandboxID, imageName, strings.TrimSpace(optString(config.ImageDigest)))
	if err != nil {
		return nil, false, err
	}
	if replaced != nil {
		// Only now, with the new image on the host: obtaining it is the step a
		// re-pin fails at, and a sandbox that cannot get its new image keeps the
		// container it had rather than being left with none (ADR 26-10-01-876 §4).
		if replacedRunning {
			// Stop it the way a stop would, so the sandbox-agent tears its execs
			// down and flushes their logs instead of being killed outright.
			r.publishSandboxState(ctx, sandboxID, StateStopping)
			timeout := sandboxStopTimeoutSeconds
			if _, err := r.client.ContainerStop(ctx, replaced.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
				return nil, false, fmt.Errorf("stop sandbox container for a spec change: %w", err)
			}
		}
		if _, err := r.client.ContainerRemove(ctx, replaced.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return nil, false, fmt.Errorf("remove sandbox container for a spec change: %w", err)
		}
	}
	user := resolveSandboxUser(r.paths, req)
	r.publishSandboxPhase(ctx, sandboxID, PhasePreparingVolumes)
	mounts, err := r.prepareSandboxVolumes(ctx, sandboxID, req, user)
	if err != nil {
		return nil, false, err
	}
	// The bootstrap is built from the project layer recorded for the sandbox:
	// none until it has cloned its primary source, and from then on the one
	// settling read from it (settleSources).
	project, err := r.readProjectLayerRecord(sandboxID)
	if err != nil {
		return nil, false, err
	}
	primary := ""
	if _, hasPrimary := req.Config.Source.Get(); hasPrimary {
		// The primary source is always first when present (sandboxSources).
		primary = sandboxSources(r.paths, req)[0].slug
	} else {
		// Only the primary source carries a project layer.
		project.Project = nil
	}
	project.Source = &primary
	if err := r.writeProjectLayerRecord(sandboxID, project); err != nil {
		return nil, false, err
	}
	proxyMaterial, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID)
	if err != nil {
		return nil, false, err
	}
	sentinels, _ := req.Sentinels.Get()
	if err := proxyagent.UpsertSandboxSentinels(r.root, r.projectID, r.poolID, sandboxID, sentinels); err != nil {
		return nil, false, err
	}
	// The bootstrap: placed before the container exists, and the one thing the
	// pool ever writes into the sandbox's config volume (ADR 0126 §3).
	if err := r.writeSandboxHarnessConfig(ctx, sandboxID, imageName, req, proxyMaterial.Env, project.Project); err != nil {
		return nil, false, err
	}
	// Everything else the sandbox is told is its runtime-config document,
	// delivered once its agent answers (finishBoot). The secrets the control
	// plane sends a create are the sandbox's whole set. Each source is named
	// with where its origin is, which the sandbox clones it from; it stays
	// undelivered until settling has read its project layer.
	secretEnv, _ := req.SecretEnv.Get()
	if err := r.recordRuntimeConfig(sandboxID, func(doc *sandboxconfig.RuntimeConfig) {
		doc.SecretEnv = cloneStringMap(secretEnv)
		doc.Sources = r.runtimeSources(sandboxID, sandboxSources(r.paths, req), doc.Sources)
	}); err != nil {
		return nil, false, err
	}
	baseEnv := mergeEnv(map[string]string(optSandboxConfigEnv(config.Env)), proxyMaterial.Env)
	name := sandboxContainerName(r.poolID, sandboxID)
	cfg := &container.Config{
		Image:        imageName,
		Hostname:     sandboxHostname(sandboxID),
		Labels:       r.labels(sandboxID, strings.TrimSpace(optString(config.SpecFingerprint)), projectLayerDigest(project.Project)),
		Env:          envList(envWithSandboxUser(baseEnv, user)),
		WorkingDir:   workingDir,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	}
	hostCfg := &container.HostConfig{
		Mounts:     mounts,
		Privileged: true,
		// Docker's 64 MiB /dev/shm is too small for what a sandbox runs:
		// Chromium and Electron put their shared pixel buffers there, and on
		// the desktop's 3840x2432 display one frame is ~37 MiB, so VS Code's
		// renderer dies on start. The size is a tmpfs cap, not a reservation.
		ShmSize: r.sharedMemoryBytes,
		// Docker's embedded resolver answers container names itself and
		// forwards every other name here. On an internal network it forwards
		// nowhere else, so without this no external name resolves. The
		// address is the sandbox's own: its DNS stub claims it on loopback
		// and carries each query to the pool over mTLS.
		DNS: []netip.Addr{proxyagent.SandboxDNSAddress},
	}
	// No CPU/memory limit is set here: a sandbox container shares its pool
	// container's cgroup rather than reserving a nested slice of it
	// (ADR 0029).
	// Attach the sandbox to the per-pool internal network only: it reaches the
	// pool proxy (resolved as discobox-pool-proxy via Docker embedded DNS)
	// and the pool's DNS forwarder, but has no route off-box, so all egress is
	// forced through the proxy.
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			proxyagent.SandboxNetworkName(r.poolID): {},
		},
	}
	r.publishSandboxPhase(ctx, sandboxID, PhaseCreatingContainer)
	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: name})
	if err != nil {
		return nil, false, err
	}
	// A create that is not asked to start leaves the container built and down.
	// That is what makes rebuilding a sandbox whose container was lost a
	// restoration rather than a resurrection: the sandbox exists again, and
	// whoever wants it running starts it (ADR 0017 §13).
	//
	// Replacing a running container is the exception, and the flag cannot veto
	// it: Start is first-create intent, not a desired power state for a sandbox
	// that already exists, so the two inputs only ever add a start (ADR 0021 §4).
	if !config.Start.Or(true) && !replacedRunning {
		sb, err := r.observedSandbox(ctx, sandboxID)
		return sb, false, err
	}
	r.publishSandboxState(ctx, sandboxID, StateStarting)
	r.publishSandboxPhase(ctx, sandboxID, PhaseStartingContainer)
	boot := r.beginBoot(sandboxID)
	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		r.endBoot(sandboxID, boot, err)
		return nil, false, err
	}
	// The container is up and the agent inside it is not yet answering. This is
	// the last phase the pool agent can see: what happens after it is the
	// sandbox agent's own boot, which reports on no channel this one owns
	// (ADR 0060).
	r.publishSandboxPhase(ctx, sandboxID, PhaseWaitingForAgent)
	if err := r.finishBoot(ctx, sandboxID, boot); err != nil {
		return nil, false, err
	}
	// The sandbox clones its sources now that its agent has the document
	// naming them, and the create settles on them before it returns, as a
	// pool that cloned them itself used to before the container existed.
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, false, err
	}
	r.publishSandboxPhase(ctx, sandboxID, PhaseMaterializingSource)
	rebuild, err := r.settleSources(ctx, sb, req)
	if err != nil || rebuild {
		return sb, rebuild, err
	}
	sb, err = r.observedSandbox(ctx, sandboxID)
	return sb, false, err
}

// observedSandbox reads the sandbox back and reports what it sees before
// returning it.
//
// Every create ends here, including the one that was not asked to start
// anything. That create is the case the Docker event stream cannot cover: no
// container transition happens, so an unarchive or a rebuild after the
// container was lost would produce no observation at all and the control plane
// would carry its previous belief until the next complete sync, up to a full
// interval later (ADR 0034 §4).
//
// A create that did start the container publishes a state the `start` event
// already reported. That duplicate is harmless — the report is idempotent and
// carries a newer sequence — and it is worth more than the alternative, which
// is a create path where whether an observation gets published depends on which
// branch it took.
func (r *DockerSandboxRuntime) observedSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	r.publishSandboxState(ctx, sandboxID, stateFromStatus(sb.Status))
	return sb, nil
}

// resolveSandboxImage resolves what a sandbox must actually run and returns the
// image ID to launch it from.
//
// The pinned digest is the identity; the reference is only a way to obtain it
// (ADR 0016 §6). Launching the reference instead would let a rebuilt tag change
// a sandbox underneath its user — and, because containerImageDrifted compares
// against the pin, would make every such sandbox look drifted and be replaced
// silently, performing an upgrade nobody asked for. An empty pin means unpinned:
// resolve the reference and run whatever it names.
func (r *DockerSandboxRuntime) resolveSandboxImage(ctx context.Context, sandboxID, imageName, pinnedDigest string) (string, error) {
	if pinnedDigest != "" {
		// A pinned image already on the host is authoritative, whatever the tag
		// points at now.
		if inspected, err := r.client.ImageInspect(ctx, pinnedDigest); err == nil {
			return inspected.ID, nil
		} else if !cerrdefs.IsNotFound(err) {
			return "", err
		}
	}
	if err := r.ensureImageAvailable(ctx, sandboxID, imageName); err != nil {
		return "", err
	}
	inspected, err := r.client.ImageInspect(ctx, imageName)
	if err != nil {
		return "", fmt.Errorf("inspect image %q: %w", imageName, err)
	}
	if !imageMatchesPinDigests(inspected.ID, inspected.RepoDigests, pinnedDigest) {
		return "", fmt.Errorf(
			"%w: the sandbox is pinned to image %s but %q now resolves to %s, and the pinned image is not on this pool; upgrade the sandbox to move it to the current image",
			ErrImageUnavailable, pinnedDigest, imageName, inspected.ID)
	}
	return inspected.ID, nil
}

// imageMatchesPin reports whether an image ID is the one a sandbox is pinned to.
//
// An empty pin matches anything: unpinned sandboxes (the default image, or
// sandboxes created before pinning existed) run whatever their reference names.
// This is the single comparison behind both enforcement points — refusing to
// launch the wrong image, and replacing a container built from one — so the two
// can never disagree about what "the pinned image" means.
func imageMatchesPin(imageID, pinnedDigest string) bool {
	return imageMatchesPinDigests(imageID, nil, pinnedDigest)
}

// imageMatchesPinDigests is imageMatchesPin given everything the daemon knows
// an image by: its ID, and the registry digests it was pulled under.
//
// Both are needed because "the image ID" is not one thing. The classic image
// store reports the config digest; the containerd store, which is the default
// in current Docker, reports the index digest. A pin recorded as one and
// compared against the other never matches, and every sandbox on a published
// multi-arch image refused to launch with "the pinned image is not available on
// this pool" — for an image sitting on the daemon, correctly pulled.
//
// RepoDigests is what settles it: both stores put the registry digest there, so
// a pin recorded from the registry matches on either. The ID is still consulted
// for locally built images, which were never pushed and so have no RepoDigests
// at all.
func imageMatchesPinDigests(imageID string, repoDigests []string, pinnedDigest string) bool {
	pinned := strings.TrimSpace(pinnedDigest)
	if pinned == "" {
		return true
	}
	if strings.TrimSpace(imageID) == pinned {
		return true
	}
	for _, repoDigest := range repoDigests {
		if _, digest, ok := strings.Cut(repoDigest, "@"); ok && strings.TrimSpace(digest) == pinned {
			return true
		}
	}
	return false
}

// containerSpecDrifted reports whether the existing container was built from a
// different spec than the one this request describes (ADR 0017 §5).
//
// The comparison is against the fingerprint label, not against any individual
// field. That is the point of hashing the whole manifest in the control plane:
// a spec field added later is covered here for free, where a per-field check
// would silently keep serving a stale container until somebody remembered to
// extend it.
//
// An empty fingerprint in the request means the caller does not pin a spec, so
// nothing has drifted — the same "unpinned means run what you have" rule the
// image digest already follows.
func (r *DockerSandboxRuntime) containerSpecDrifted(ctx context.Context, existing *Sandbox, req *workerapimodel.PoolSandboxCreateRequest) (bool, error) {
	if req == nil {
		return false, nil
	}
	inspect, err := r.client.ContainerInspect(ctx, existing.ID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	// A container built from a bootstrap that names no pool key cannot take
	// the pool's runtime-config documents, so it runs on whatever was staged
	// for it before; the create that reaches it is what moves it onto the
	// bootstrap that can (ADR 26-10-08-127).
	if r.poolPublicKey() != "" && inspect.Container.Config != nil && inspect.Container.Config.Labels[sandboxLabelRuntimeConfig] == "" {
		return true, nil
	}
	fingerprint := strings.TrimSpace(optString(req.Config.SpecFingerprint))
	if fingerprint == "" {
		return false, nil
	}
	return specDrifted(
		inspect.Container.Config.Labels[sandboxLabelSpec],
		inspect.Container.Image,
		fingerprint,
		strings.TrimSpace(optString(req.Config.ImageDigest)),
	), nil
}

// specDrifted decides drift from what the container can say about itself.
//
// A recorded fingerprint answers the question outright. Without one the
// container predates fingerprinting, and the answer is not "nothing drifted" —
// it is that the label cannot tell us, so we fall back to the comparison that
// needs no label: the image the container was built from against the digest the
// request pins. That is the check this one generalized (ADR 0016), and dropping
// it for unlabeled containers left them permanently stranded — the control plane
// records a re-pin, the runtime declines to act on it, `ObservedGeneration`
// catches up, and upgrade then reports the sandbox as already current forever
// because it compares the record against the harness config, never against the
// container.
//
// Falling back narrows the rebuild to containers actually running the wrong
// image rather than every container an upgraded control plane first talks to,
// and a replacement keeps the pool-host volumes and the power state either way
// (ADR 0021 §3).
func specDrifted(recordedFingerprint, containerImageID, fingerprint, pinnedDigest string) bool {
	if recorded := strings.TrimSpace(recordedFingerprint); recorded != "" {
		return recorded != fingerprint
	}
	return !imageMatchesPin(containerImageID, pinnedDigest)
}

// ensureImageAvailable pulls the sandbox's image if the host does not have it,
// reporting progress as it goes.
//
// An image pull is the longest thing an attach can end up waiting behind, so it
// is also the one that most needs to be visible: a multi-gigabyte pull and a
// hung control plane look identical to a client watching a silent socket
// (ADR 0039). Progress is reported per sandbox because that is what a waiting
// client asked about; the pull itself is per image and may well be feeding
// several sandboxes at once.
func (r *DockerSandboxRuntime) ensureImageAvailable(ctx context.Context, sandboxID, imageName string) error {
	if _, err := r.client.ImageInspect(ctx, imageName); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	pull, err := r.client.ImagePull(ctx, imageName, client.ImagePullOptions{})
	if err != nil {
		// The daemon answers a reference no registry has -- no such repository
		// or tag, which is also what it reports for "pull access denied ...
		// repository does not exist" -- as not found, before any progress. That
		// is an answer about the image, not a failed attempt, and it is the one
		// a locally built image nobody pushed always gets.
		//
		// Unauthorized and forbidden are not: they are this pool's credentials
		// for an image that may well exist, and fixing the credentials is what
		// helps. An upgrade would not, since the harness's current image sits
		// behind the same ones.
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("%w: %q is not on this pool and its registry does not have it: %w", ErrImageUnavailable, imageName, err)
		}
		return fmt.Errorf("pull image %q: %w", imageName, err)
	}
	defer pull.Close()
	// JSONMessages rather than Wait: Wait drains the daemon's progress stream
	// and discards it, which is exactly the information a waiting client needs.
	// Draining is what runs the pull, so this replaces Wait rather than adding
	// to it.
	err = consumePullProgress(ctx, pull.JSONMessages(ctx), imageName, func(progress PullProgress) {
		r.publishSandboxPullProgress(ctx, sandboxID, progress)
	}, nil)
	if err != nil {
		return fmt.Errorf("pull image %q: %w", imageName, err)
	}
	return nil
}

// prepareSandboxVolumes provisions the host-backed roots and returns their
// container mounts. The pool host does not decide in-sandbox paths (home,
// /var/lib/docker, sources targets); it only supplies the primary volumes. The
// sandbox-agent wires everything else from the image's declarative volume list
// and the manifest's source list (ADR 0007).
//
// Each source gets an empty directory and nothing more: the sandbox clones it
// there itself, as the user boot gives the directory to, from the origin its
// runtime-config document names (ADR 0126 §4). Nothing here runs git in it,
// chowns what is in it, or binds an origin beside it.
func (r *DockerSandboxRuntime) prepareSandboxVolumes(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxCreateRequest, user sandboxuser.User) ([]mount.Mount, error) {
	// Creating a container against this tree is what unarchiving is: the tree is
	// reused as it stands, and clearing the marker is the whole of what makes it
	// a live sandbox again (ADR 0022 §6). Clearing it first means a create that
	// fails part way leaves the tree unmarked and container-less, which the
	// reaper handles as the ordinary failed-create case.
	if err := clearSandboxArchiveMarker(r.sandboxRoot(sandboxID)); err != nil {
		return nil, fmt.Errorf("clear sandbox archive marker: %w", err)
	}
	// Only the root itself. What lives under it is the sandbox's home, written
	// by the sandbox user, and this agent never writes a byte of it -- so the
	// create path has nothing to assert there and taking ownership away is a
	// correctness bug, not merely a slow one. Unarchiving is a create against a
	// tree that is already full (above), which is exactly when a recursive chown
	// here reached files: it handed everything the sandbox had ever written to
	// root, and the only thing that gave it back was the sandbox's own boot-time
	// walk -- which covers home's own filesystem and nothing mounted under it.
	dataHostPath := r.sandboxDataRootPath(sandboxID)
	if err := prepareOwnedMountpoint(dataHostPath, 0, 0); err != nil {
		return nil, fmt.Errorf("prepare sandbox data volume: %w", err)
	}
	cacheHostPath := r.poolCacheRoot()
	if err := prepareOwnedMountpoint(cacheHostPath, 0, 0); err != nil {
		return nil, fmt.Errorf("prepare pool cache volume: %w", err)
	}
	configHostPath := r.sandboxConfigRoot(sandboxID)
	if err := prepareOwnedTree(ctx, configHostPath, 0, 0); err != nil {
		return nil, fmt.Errorf("prepare sandbox config volume: %w", err)
	}
	// Only the root itself: what is under it is each source's checkout, which
	// the sandbox writes as its own user, and this agent never writes a byte
	// of it.
	sourcesHostPath := r.sandboxSourcesRoot(sandboxID)
	if err := prepareOwnedMountpoint(sourcesHostPath, 0, 0); err != nil {
		return nil, fmt.Errorf("prepare sandbox sources volume: %w", err)
	}
	secretsHostPath := r.sandboxSecretsRoot(sandboxID)
	if err := prepareOwnedTree(ctx, secretsHostPath, 0, 0); err != nil {
		return nil, fmt.Errorf("prepare sandbox secrets volume: %w", err)
	}
	sources := sandboxSources(r.paths, req)
	_, hasPrimary := req.Config.Source.Get()
	for _, source := range sources {
		// The origin repository has to exist before the container does: the
		// client pushes into it while the sandbox parks (ADR 0058 §1).
		if gitSourceAwaitsPush(source.git) {
			if err := r.initGitOrigin(ctx, r.sandboxOriginPath(sandboxID, source.slug), gitSourceInitialBranch(source.git), user); err != nil {
				return nil, fmt.Errorf("prepare source origin %q: %w", source.slug, err)
			}
		}
		// A live origin is served from the developer's own Git directory, which
		// has to be a real one (ADR 0093); a create says so now rather than the
		// sandbox's clone answering a missing repository.
		if err := r.checkLocalGitDirectory(source.git); err != nil {
			return nil, fmt.Errorf("source %q: %w", source.slug, err)
		}
		// The directory boot binds onto the source's target, which it gives to
		// the sandbox user. It exists from the start, even for a source still
		// on its way, because the resume that fills it does not rebuild the
		// container, and a bind skipped at boot is never made at all.
		if err := os.MkdirAll(r.sandboxSourcePath(sandboxID, source.slug), 0o755); err != nil {
			return nil, fmt.Errorf("prepare source %q: %w", source.slug, err)
		}
		if dataKey := optString(source.git.DataKey); dataKey != "" {
			if !validSourceDataKey(dataKey) {
				return nil, fmt.Errorf("source %q has invalid data key", source.slug)
			}
			if err := prepareOwnedMountpoint(r.sourceDataPath(dataKey), chownID(user.UID), chownID(user.GID)); err != nil {
				return nil, fmt.Errorf("prepare source data %q: %w", source.slug, err)
			}
		}
	}
	if err := r.writeLiveOrigins(sandboxID, sources); err != nil {
		return nil, fmt.Errorf("record live origins: %w", err)
	}
	// These sources are resolved by the Docker daemon, so they are the only
	// place a container path has to become a daemon path.
	mounts := []mount.Mount{
		{Type: mount.TypeBind, Source: r.daemonPath(dataHostPath), Target: sandboxDataMount},
		{Type: mount.TypeBind, Source: r.daemonPath(cacheHostPath), Target: sandboxCacheMount},
		{Type: mount.TypeBind, Source: r.daemonPath(configHostPath), Target: sandboxConfigMount},
		{Type: mount.TypeBind, Source: r.daemonPath(sourcesHostPath), Target: sandboxSourcesMount},
		{Type: mount.TypeBind, Source: r.daemonPath(secretsHostPath), Target: sandboxSecretsMount},
	}
	for _, data := range sourceDataPlan(sources, hasPrimary) {
		hostPath := r.sourceDataPath(data.key)
		if data.key == "" {
			hostPath = r.sandboxSourceDataPath(sandboxID, data.slug)
			// A keyed source's mountpoint was prepared with the source above.
			if err := prepareOwnedMountpoint(hostPath, chownID(user.UID), chownID(user.GID)); err != nil {
				return nil, fmt.Errorf("prepare private source data %q: %w", data.slug, err)
			}
		}
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: r.daemonPath(hostPath),
			Target: path.Join(sandboxSourceDataMount, data.slug),
		})
	}
	return mounts, nil
}

// sourceData is one `/.discobox/data-per-source/<slug>` mount: the pool-local
// data shared under key, or, when key is empty, data private to the sandbox.
type sourceData struct {
	slug string
	key  string
}

// sourceDataPlan lays out a sandbox's source-data mounts. The contents are
// opaque to the pool and sandbox agents; consumers such as harnesses own
// everything below each mount.
//
// Every sandbox has primary source data, at sandboxconfig.PrimarySourceSlug
// whatever the primary's own slug, because that fixed path is what harness
// images read. It is shared by key when the primary has one; a primary the
// control plane could give no key -- the sandbox has no origin -- and a sandbox
// with no primary source at all get a private one instead of none. No source
// is a private source. A source code reference's data is mounted under its own
// slug, and only when it has a key.
//
// The control plane has reserved the primary's name since this layout began,
// but a sandbox created before then may have a reference that holds it. That
// reference keeps its mount, and the primary's data stays under the primary's
// own slug -- or, with no primary, there is none -- exactly as it was laid out.
func sourceDataPlan(sources []sandboxSource, hasPrimary bool) []sourceData {
	var referenceHoldsPrimary bool
	for i, source := range sources {
		// The primary source is always first when present (sandboxSources).
		if (i > 0 || !hasPrimary) && source.slug == sandboxconfig.PrimarySourceSlug {
			referenceHoldsPrimary = true
		}
	}
	var plan []sourceData
	for i, source := range sources {
		key := optString(source.git.DataKey)
		if i == 0 && hasPrimary {
			slug := sandboxconfig.PrimarySourceSlug
			if referenceHoldsPrimary {
				slug = source.slug
			}
			plan = append(plan, sourceData{slug: slug, key: key})
			continue
		}
		if key != "" {
			plan = append(plan, sourceData{slug: source.slug, key: key})
		}
	}
	if !hasPrimary && !referenceHoldsPrimary {
		plan = append(plan, sourceData{slug: sandboxconfig.PrimarySourceSlug})
	}
	return plan
}

func validSourceDataKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, char := range key {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func (r *DockerSandboxRuntime) writeSandboxHarnessConfig(ctx context.Context, sandboxID, resolvedImage string, req *workerapimodel.PoolSandboxCreateRequest, proxyEnv map[string]string, project *sandboxconfig.ProjectLayer) error {
	configDir := r.sandboxConfigRoot(sandboxID)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	doc := buildSandboxDocument(r.paths, r.projectID, sandboxID, r.poolID, r.controlPlanePublicKey, r.poolPublicKey(), resolvedImage, req, proxyEnv, project)
	data, err := marshalSandboxDocument(doc)
	if err != nil {
		return err
	}
	path := filepath.Join(configDir, sandboxDocumentName)
	if err := writeSandboxManifest(path, data); err != nil {
		return err
	}
	// A readiness marker left in the volume by an earlier container — or by
	// the pool, before the intake wrote it — would open this container's gate
	// before its agent has applied anything. The agent puts back what its own
	// kept document grants when it starts.
	if err := os.Remove(filepath.Join(configDir, sandboxconfig.SourcesReadyFileName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear readiness from before this container: %w", err)
	}
	return chownRecursive(ctx, configDir, 0, 0)
}

// sandboxDocumentName is the sandbox's effective configuration — its static
// bootstrap — in the config volume the sandbox sees at /etc/discobox.
const sandboxDocumentName = "sandbox.json"

// sandboxDocumentFile is the on-disk sandbox.json shape (ADR 0012 §8): the
// effective config's fields sit at the top level, with a diagnostic
// _provenance sibling carrying the raw per-layer inputs. sandbox-agent
// decodes the embedded Config fields and ignores _provenance entirely.
type sandboxDocumentFile struct {
	sandboxconfig.Config
	Provenance sandboxconfig.Provenance `json:"_provenance"`
}

func marshalSandboxDocument(doc sandboxconfig.Document) ([]byte, error) {
	cfg, provenance := sandboxconfig.Effective(doc)
	return json.MarshalIndent(&sandboxDocumentFile{Config: cfg, Provenance: provenance}, "", "  ")
}

func writeSandboxManifest(path string, data []byte) error {
	//nolint:gosec // sandbox.json is a public runtime contract consumed by the unprivileged sandbox user.
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	// WriteFile applies its mode only when creating a file. Enforce the public
	// manifest mode when replacing a sandbox.json previously created as 0600.
	return os.Chmod(path, 0o644)
}

// documentHarnessSecrets carries the harness's declared credentials onto the
// image layer. The sandbox reads them for Delivery alone: a file-delivered
// secret must not also be exported as an environment variable, and only this
// declaration says which (harness.SecretDeliveryFile).
func documentHarnessSecrets(secrets []workerapimodel.HarnessSecret) []harness.Secret {
	if len(secrets) == 0 {
		return nil
	}
	out := make([]harness.Secret, 0, len(secrets))
	for _, secret := range secrets {
		out = append(out, harness.Secret{
			Name:       secret.Name,
			Required:   secret.Required.Or(false),
			OneOfGroup: secret.OneOfGroup.Or(""),
			Delivery:   secret.Delivery.Or(""),
		})
	}
	return out
}

func documentFiles(files []workerapimodel.HarnessConfigFile) []sandboxconfig.File {
	if len(files) == 0 {
		return nil
	}
	out := make([]sandboxconfig.File, 0, len(files))
	for _, file := range files {
		out = append(out, sandboxconfig.File{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: file.CreateOnly.Or(false),
			Template:   file.Template.Or(false),
		})
	}
	return out
}

func documentVolumes(volumes []workerapimodel.HarnessVolume) []harness.Volume {
	if len(volumes) == 0 {
		return nil
	}
	out := make([]harness.Volume, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, harness.Volume{
			Path:              v.Path,
			Volume:            harness.VolumeKind(v.Volume),
			Scope:             harness.VolumeScope(v.Scope.Or("")),
			ExcludeFromExport: v.ExcludeFromExport.Or(false),
			UID:               harness.ScalarToken(optString(v.UID)),
			GID:               harness.ScalarToken(optString(v.Gid)),
			Mode:              optString(v.Mode),
		})
	}
	return out
}

// buildSandboxDocument assembles the three attribute-owned layers (ADR 0012)
// for one sandbox: RuntimeLayer from the create request, ImageLayer from the
// harness config the control plane already resolved and snapshotted from the
// image's OCI label, and the caller-supplied ProjectLayer (read once from the
// resolved source repository at clone time; nil when the project supplies
// nothing).
//
// It is the sandbox's static bootstrap (ADR 26-10-08-127 §1): public keys to
// trust — the control plane's, and the pool's for runtime-config deliveries —
// where the pool serves it, and the create-time config, and no private key or
// secret.
func buildSandboxDocument(paths sandboxpath.Paths, projectID, sandboxID, poolID, controlPlanePublicKey, poolPublicKey, resolvedImage string, req *workerapimodel.PoolSandboxCreateRequest, proxyEnv map[string]string, project *sandboxconfig.ProjectLayer) sandboxconfig.Document {
	publicKeys := map[string]string{sandboxconfig.ControlPlanePublicKeyName: controlPlanePublicKey}
	if poolPublicKey != "" {
		publicKeys[sandboxconfig.PoolPublicKeyName] = poolPublicKey
	}
	pool := proxyagent.PoolEndpoints()
	doc := sandboxconfig.Document{
		Runtime: sandboxconfig.RuntimeLayer{
			SandboxID: sandboxID,
			Image:     resolvedImage,
			Provider: sandboxconfig.Provider{
				Kind:       "discobox-pool",
				ProjectID:  projectID,
				PoolID:     poolID,
				PublicKeys: publicKeys,
				Pool:       &pool,
			},
			AgentRuntime: sandboxconfig.AgentRuntime{
				ListenAddress:          fmt.Sprintf(":%d", SandboxAgentPort),
				WorkingRoot:            paths.WorkingRoot(),
				RuntimeDir:             "/run/discobox/agent-terminals",
				DatabasePath:           "/var/lib/discobox/sandbox-agent.db",
				ResourceSampleInterval: time.Second.String(),
				ResourceRetentionCount: 300,
			},
		},
		Project: project,
	}
	if req != nil {
		config := req.Config
		doc.Runtime.Model = optString(config.Model)
		doc.Runtime.ModelReasoningLevel = optString(config.ModelReasoningLevel)
		doc.Runtime.ModelServiceTier = optString(config.ModelServiceTier)
		doc.Runtime.Prompt = append([]string{}, config.Prompt...)
		doc.Runtime.Description = optString(config.Description)
		if mode, ok := config.HarnessMode.Get(); ok {
			doc.Runtime.HarnessMode = string(mode)
		}
		if env, ok := config.Env.Get(); ok {
			doc.Runtime.Env = map[string]string(env)
		}
		// Authorship, forwarded verbatim. Unlike the run user below there is
		// nothing here for the pool to resolve or complete: git identity is
		// whatever the caller said it was, and boot writes exactly that
		// (ADR 0042 §3).
		if git, ok := config.Git.Get(); ok {
			doc.Runtime.Git = sandboxconfig.GitIdentity{
				UserName:  optString(git.UserName),
				UserEmail: optString(git.UserEmail),
			}
		}
		// Skills, forwarded verbatim into the bootstrap; the sandbox installs
		// them on its first launch (ADR 26-10-09-395 §2).
		if skills, ok := config.Skills.Get(); ok {
			doc.Runtime.Skills = sandboxSkills(skills)
		}
		// The run user is the request's config.user, trimmed and passed through
		// sandboxuser.Merge as its only layer. The pool resolves nothing further:
		// what the request left unset stays unset here, for the sandbox-agent to
		// resolve against the image (ADR 0033 §5). It is the same value the
		// create path uses for the home mount and container environment.
		user := resolveSandboxUser(paths, req)
		doc.Runtime.User = user
		// The sandbox-agent bind-mounts each source's directory from
		// /.discobox/sources/<slug> onto its target as this same user (ADR
		// 0007), and clones the source into it (ADR 0126 §4).
		for _, source := range sandboxSources(paths, req) {
			doc.Runtime.Sources = append(doc.Runtime.Sources, sandboxconfig.Source{
				Slug:   source.slug,
				Target: source.target,
				// Absent when the request gave no ids: boot then chowns with
				// the identity it resolved, which it has in hand and which is
				// the better answer anyway (ADR 0033 §5).
				UID: user.UID,
				GID: user.GID,
				// Where the agent's reported diff stat measures from: the
				// commit the source was spawned at, forwarded to the merge
				// base with the upstream tracking ref once the sandbox has
				// fetched.
				BaseCommit:  sourceBaseCommit(source.git),
				UpstreamRef: sourceUpstreamRef(source.git),
				// How the sandbox agent checks the source out when it clones
				// it from the origin the runtime-config document names (ADR
				// 0126 §4).
				RefName:     sourceRefName(source.git),
				RefType:     sourceRefType(source.git),
				UpstreamURL: strings.TrimSpace(optString(source.git.UpstreamUrl)),
				Workspace:   sourceWorkspace(source.git),
				// The client still owes this one: the sandbox holds its harness
				// launch until the push lands and this pool agent reports the
				// sandbox settled.
				AwaitsDelivery: gitSourceAwaitsPush(source.git),
			})
		}
		if resolved, ok := req.ResolvedHarnessConfig.Get(); ok {
			doc.Image = sandboxconfig.ImageLayer{
				HarnessID:          resolved.ID,
				HarnessName:        resolved.Name,
				HarnessDescription: optString(resolved.Description),
			}
			if runCommand, ok := resolved.RunCommand.Get(); ok {
				doc.Image.RunCommand = runCommand
			}
			if relaunchCommand, ok := resolved.RelaunchCommand.Get(); ok {
				doc.Image.RelaunchCommand = relaunchCommand
			}
			if configCommand, ok := resolved.ConfigCommand.Get(); ok {
				doc.Image.ConfigCommand = configCommand
			}
			if files, ok := resolved.Files.Get(); ok {
				doc.Image.Files = documentFiles(files)
			}
			if configuredFiles, ok := resolved.ConfiguredFiles.Get(); ok {
				doc.Runtime.Files = documentFiles(configuredFiles)
			}
			if secrets, ok := resolved.Secrets.Get(); ok {
				doc.Image.Secrets = documentHarnessSecrets(secrets)
			}
			if env, ok := resolved.Env.Get(); ok {
				doc.Image.Env = harness.ExpandEnvHomeTokens(map[string]string(env), user.HomeDirectory)
			}
			if volumes, ok := resolved.Volumes.Get(); ok {
				doc.Image.Volumes = documentVolumes(volumes)
			}
			if groups, ok := resolved.AdditionalGroups.Get(); ok {
				doc.Image.AdditionalGroups = groups
			}
		}
	}
	// Inject the pool-proxy environment so sandbox-agent-spawned terminals and
	// execs route outbound traffic through the local forwarder and trust the
	// MITM CA.
	if len(proxyEnv) > 0 {
		env := map[string]string{}
		for key, value := range doc.Runtime.Env {
			env[key] = value
		}
		for key, value := range proxyEnv {
			env[key] = value
		}
		doc.Runtime.Env = env
		// ProxyEnvs names which of the keys just merged into Env are
		// proxy-trust vars, so sandbox-agent's runc wrapper knows which names
		// to republish into a nested Docker container without hardcoding
		// them itself. See docs/adr/0015.
		proxyEnvNames := make([]string, 0, len(proxyEnv))
		for key := range proxyEnv {
			proxyEnvNames = append(proxyEnvNames, key)
		}
		sort.Strings(proxyEnvNames)
		doc.Runtime.ProxyEnvs = proxyEnvNames
	}
	return doc
}

// prepareOwnedTree creates dir and asserts ownership over everything inside it.
// Use it only for roots this agent itself materializes end to end and whose size
// one sandbox bounds -- the per-sandbox config and secrets trees, and a pushed
// source's bare origin. A tree the sandbox writes is not one of them, no
// matter how small: ownership there is the sandbox's answer, not this agent's.
func prepareOwnedTree(ctx context.Context, dir string, uid, gid int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	return chownRecursive(ctx, dir, uid, gid)
}

// prepareOwnedMountpoint creates dir and owns the directory itself, never what
// is inside it. A bind-mount source only needs its own ownership to be right:
// the contents belong to whoever legitimately wrote them.
//
// This is what the shared pool cache needs. Asserting root ownership over that
// whole tree was both unbounded and pointless: it grows without limit (tens of
// gigabytes and ~10^6 inodes on a working machine, so ~37s of every create on a
// cold page cache), and sandbox-agent's seedHome immediately chowned the cache
// back to the sandbox user on the way up. The two passes fought over the same
// inodes on every single start.
//
// It is also what the sandbox's data root needs, for the stronger reason: that
// tree is the sandbox's home, and a create that walks it takes files away from
// the user who wrote them. Being repaired on the way back up is not a defense --
// the repair is a different component's walk, with its own limits, and a rule
// that only holds while two passes agree exactly is not a rule.
func prepareOwnedMountpoint(dir string, uid, gid int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	if uid == unsetID && gid == unsetID {
		return nil
	}
	//nolint:gosec // dir is a pool-owned root path, not attacker-controlled.
	return os.Lchown(dir, uid, gid)
}

func (r *DockerSandboxRuntime) GetSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.filters(sandboxID)})
	if err != nil {
		return nil, err
	}
	if len(containers.Items) == 0 {
		return nil, ErrNotFound
	}
	inspect, err := r.client.ContainerInspect(ctx, containers.Items[0].ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return r.sandboxFromInspect(ctx, inspect.Container), nil
}

func (r *DockerSandboxRuntime) UpdateSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxUpdateRequest) (*Sandbox, error) {
	if req != nil {
		if sentinels, ok := req.Sentinels.Get(); ok {
			// Re-register the sandbox's sentinel set with the proxy so newly bound
			// secrets resolve without a restart.
			if err := proxyagent.UpsertSandboxSentinels(r.root, r.projectID, r.poolID, sandboxID, sentinels); err != nil {
				return nil, err
			}
		}
		if secretEnv, ok := req.SecretEnv.Get(); ok {
			// Newly bound secrets, or a rotated sentinel, reach the sandbox in
			// its runtime-config document: delivered now to a sandbox that is
			// up, and at its next start to one that is not.
			if err := r.updateSandboxSecretEnv(ctx, sandboxID, cloneStringMap(secretEnv)); err != nil {
				return nil, err
			}
		}
	}
	return r.GetSandbox(ctx, sandboxID)
}

// updateSandboxSecretEnv replaces the sandbox's secret environment in its
// runtime-config document and delivers it when the sandbox is running.
//
// It holds the sandbox's power lock, so it is ordered against a delete or an
// archive rather than racing it, and a boot that a start under way began has
// already delivered by the time it runs. A sandbox with no container — archived,
// or between containers — is left alone: the create that gives it one sends its
// whole secret set.
func (r *DockerSandboxRuntime) updateSandboxSecretEnv(ctx context.Context, sandboxID string, secretEnv map[string]string) error {
	power := r.sandboxLock(sandboxID)
	power.Lock()
	defer power.Unlock()
	set := func(doc *sandboxconfig.RuntimeConfig) { doc.SecretEnv = secretEnv }
	sb, err := r.GetSandbox(ctx, sandboxID)
	if errors.Is(err, ErrNotFound) || (err == nil && r.isArchived(sandboxID)) {
		return nil
	}
	if err != nil {
		return err
	}
	if sb.Status != StatusRunning {
		// The next start delivers what is recorded.
		return r.recordRuntimeConfig(sandboxID, set)
	}
	return logRuntimeConfigFailure(ctx, sandboxID, r.deliverRuntimeConfig(ctx, sandboxID, set))
}

// DeleteSandbox removes the sandbox's container, its proxy material, and its
// durable tree, and returns only once all three are gone.
//
// The durable removal is the point: the control plane's delete is synchronous
// and reports success to the user on the strength of this call returning
// (ADR 0022 §3). Until it did, the tree was left to the volume reaper's
// 24-hour retention, so a sandbox could be absent from the API and present on
// disk — including its resolved secrets — for a day.
func (r *DockerSandboxRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()

	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil {
		if _, err := r.client.ContainerRemove(ctx, sb.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			return err
		}
	}
	// Clean up proxy material even if the container was already gone, so a
	// repeated delete still reclaims the client certificate and staged files.
	if err := proxyagent.RemoveSandboxSentinels(r.root, r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	if err := proxyagent.RemoveSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	// Before the tree goes, since the tree is what names them: every image this
	// sandbox published into the pool registry to build from (ADR 0047) is
	// removed with it. A purge that left them would leave repositories nothing
	// can name — the namespace is unguessable and its only record was here — and
	// so nothing could ever clean up.
	if err := r.removePublishedImages(sandboxID); err != nil {
		return err
	}
	if err := os.RemoveAll(r.sandboxRoot(sandboxID)); err != nil {
		return fmt.Errorf("remove sandbox data for %s: %w", sandboxID, err)
	}
	return nil
}

// removePublishedImages drops the sandbox's registry namespace and everything
// under it. A sandbox that never built has no namespace staged, which is not an
// error: there is nothing of its to remove.
func (r *DockerSandboxRuntime) removePublishedImages(sandboxID string) error {
	namespace, err := proxyagent.ReadRegistryNamespace(proxyagent.RegistryNamespacePath(r.root, r.projectID, r.poolID, sandboxID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return buildkitagent.RemoveRegistryNamespace(r.root, r.projectID, r.poolID, namespace)
}

const (
	// proxyMaterialGracePeriod protects freshly staged material from being
	// pruned while its sandbox container is still being created.
	proxyMaterialGracePeriod = 15 * time.Minute
	// proxyMaterialEventDebounce coalesces a burst of container-destroy events
	// into a single reconcile pass.
	proxyMaterialEventDebounce = 5 * time.Second
	// proxyMaterialWatchBackoff paces reconnection to the Docker event stream.
	proxyMaterialWatchBackoff = 5 * time.Second
	// proxyMaterialBackstopInterval rechecks persisted material so removals that
	// happened while the pool was down are reported after the creation grace
	// period, even when the Docker event stream remains healthy.
	proxyMaterialBackstopInterval = time.Minute
	// sandboxVolumeRetention is how long a sandbox tree
	// (data/config/sources/secrets) is kept once the control plane no longer
	// holds its sandbox, and how long an orphaned pool's data subtree is kept.
	//
	// It is not how long a sandbox without a container survives: one the
	// control plane holds keeps its tree indefinitely (ADR 26-10-01-876). The
	// window is the margin against a wrong answer from the control plane — a
	// restored database, a bad query — and covers an import whose tree is
	// restored before its row exists.
	sandboxVolumeRetention = 24 * time.Hour
	// sandboxVolumeReapInterval paces the sandbox tree reaper. Each pass asks
	// the control plane, and nothing it collects is younger than a day.
	sandboxVolumeReapInterval = 10 * time.Minute
	// sandboxVolumeHeldTimeout bounds one ask for the held set. A request that
	// connects and never answers would otherwise stop the reaper for good, with
	// nothing logged; timing out is a pass that reaps nothing, and the next one
	// asks again.
	sandboxVolumeHeldTimeout = 30 * time.Second
	// sandboxVolumeUnheldMarker records, inside a sandbox's tree, when the tree
	// was first seen outside the control plane's held set. The tree is reaped
	// once it predates sandboxVolumeRetention.
	sandboxVolumeUnheldMarker = ".discobox-unheld-at"
	// legacySandboxVolumeTombstone is the clock the reaper kept when it judged a
	// tree by its container alone. It is removed, never read: a clock started by
	// container absence says nothing about whether the sandbox is held.
	legacySandboxVolumeTombstone = ".discobox-orphaned-at"
	// poolDataTombstone records, inside an orphaned pool's data subtree, when
	// pool-sync first found the pool outside the known set.
	poolDataTombstone = ".discobox-orphaned-at"
)

func (r *DockerSandboxRuntime) liveSandboxIDs(ctx context.Context) ([]string, error) {
	sandboxes, err := r.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	live := make([]string, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if sb.SandboxID != "" {
			live = append(live, sb.SandboxID)
		}
	}
	return live, nil
}

// WatchProxyMaterial reclaims orphaned pool-local proxy material after
// establishing a Docker event subscription, on managed sandbox destroy events,
// and on a slow level-triggered backstop.
//
// It reports nothing to the control plane: a sandbox whose container is gone
// is an observation, and observations travel on the state channel
// (statereport.go). This is only about reclaiming disk.
func (r *DockerSandboxRuntime) WatchProxyMaterial(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	for ctx.Err() == nil {
		if err := r.watchProxyMaterialEvents(ctx, logger); err != nil && ctx.Err() == nil {
			logger.Warn("watch sandbox container events", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(proxyMaterialWatchBackoff):
		}
	}
}

// reconcileProxyMaterial prunes proxy material for sandboxes whose containers
// no longer exist. It is the recovery path for containers deleted out of band
// or while the pool was down, which never run through DeleteSandbox.
func (r *DockerSandboxRuntime) reconcileProxyMaterial(ctx context.Context, logger *slog.Logger) {
	live, err := r.liveSandboxIDs(ctx)
	if err == nil {
		err = proxyagent.PruneOrphanedMaterial(r.root, r.projectID, r.poolID, live, proxyMaterialGracePeriod)
	}
	if err != nil {
		logger.Warn("reconcile proxy material", "error", err)
	}
}

// reconcileSandboxMaterial reclaims the proxy material of sandboxes that no
// longer have a container here. It is the level-triggered backstop for material
// whose destroy event was missed while the agent was down.
func (r *DockerSandboxRuntime) reconcileSandboxMaterial(ctx context.Context, logger *slog.Logger, minAge time.Duration) {
	live, err := r.liveSandboxIDs(ctx)
	if err != nil {
		logger.Warn("list sandbox containers", "error", err)
		return
	}
	orphans, err := proxyagent.OrphanedSandboxIDs(r.root, r.projectID, r.poolID, live, minAge)
	if err != nil {
		logger.Warn("scan orphaned sandbox material", "error", err)
	}
	for _, sandboxID := range orphans {
		if err := proxyagent.RemoveSandboxSentinels(r.root, r.projectID, r.poolID, sandboxID); err != nil {
			logger.Warn("remove sandbox proxy sentinels", "sandboxID", sandboxID, "error", err)
		}
		if err := proxyagent.RemoveSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID); err != nil {
			logger.Warn("remove sandbox proxy material", "sandboxID", sandboxID, "error", err)
		}
	}
}

// HeldSandboxes answers which sandboxes the control plane holds on this pool,
// in any state. An error is no answer, and is never read as an empty set.
type HeldSandboxes func(ctx context.Context) ([]string, error)

// WatchSandboxVolumes reaps the durable trees
// (pools/{poolID}/sandboxes/{sandboxID}) of sandboxes the control plane no
// longer holds, on a slow interval (ADR 26-10-01-876).
//
// Whether a container exists plays no part. A sandbox the control plane holds
// — failed and awaiting repair, archived, or on its way out — keeps its tree
// however long it has had no container; deletion removes the tree itself,
// through DeleteSandbox, and confirms it (ADR 0022 §3).
func (r *DockerSandboxRuntime) WatchSandboxVolumes(ctx context.Context, logger *slog.Logger, held HeldSandboxes) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(sandboxVolumeReapInterval)
	defer ticker.Stop()
	for {
		reapUnheldSandboxVolumes(ctx, r.sandboxesRoot(), held, sandboxVolumeRetention, time.Now(), logger)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// WatchImages reclaims unused Discobox images from this pool's Docker daemon on
// a slow interval (ADR 0040).
//
// The pool agent owns this daemon, so it is the thing that reclaims it: images
// land here by sync and by pull, they persist in the pool's /var/lib/docker
// volume across pool container replacement, and nothing else is in a position to
// clean them up when the control plane cannot be reached.
func (r *DockerSandboxRuntime) WatchImages(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	retention, err := imagereap.RetentionFromEnv()
	if err != nil {
		// A bad value must not take the pool down, and refusing to reclaim is
		// the safe direction, so fall back to the default and say so.
		logger.Warn("invalid image retention, using default", "error", err, "retention", imagereap.DefaultRetention)
		retention = imagereap.DefaultRetention
	}
	// Derived from the window, so a development pool — which is handed a short
	// retention by the control plane and has no other way to know it is one —
	// reclaims on a development cadence too.
	ticker := time.NewTicker(imagereap.ReclaimInterval(retention))
	defer ticker.Stop()
	for {
		r.reclaimImages(ctx, logger, retention)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *DockerSandboxRuntime) reclaimImages(ctx context.Context, logger *slog.Logger, retention time.Duration) {
	// Every image this daemon still needs is one a container refers to, and
	// imagereap already treats a stopped container as usage, so this pool needs
	// no keep set of its own. An image synced or pulled but not yet run is
	// covered by retention, and re-synced or re-pulled on demand if it does age
	// out.
	if _, err := imagereap.Reclaim(ctx, r.client, imagereap.Options{Retention: retention, Logger: logger}); err != nil {
		logger.Warn("reclaim unused Discobox images", "error", err)
	}
}

// SyncKnownPools reaps whole orphaned pools on this shared host daemon: any pool
// whose ID is not in knownPoolIDs (and is not this agent's own pool) has its
// sandbox containers removed and its data/proxy subtrees reclaimed. Sandbox
// containers are ephemeral and removed immediately; the persistent data subtree
// is kept for the retention window (via a tombstone), as an unheld sandbox tree
// is.
func (r *DockerSandboxRuntime) SyncKnownPools(ctx context.Context, knownPoolIDs []string) error {
	logger := slog.Default()
	known := make(map[string]struct{}, len(knownPoolIDs)+1)
	for _, id := range knownPoolIDs {
		if id = strings.TrimSpace(id); id != "" {
			known[id] = struct{}{}
		}
	}
	// Never reap this agent's own pool, even if the caller omitted it.
	known[r.poolID] = struct{}{}

	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.projectFilters()})
	if err != nil {
		return err
	}
	for _, ctr := range containers.Items {
		poolID := strings.TrimSpace(ctr.Labels[sandboxLabelPool])
		if poolID == "" {
			continue
		}
		if _, ok := known[poolID]; ok {
			continue
		}
		if _, err := r.client.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			logger.Warn("remove orphan pool sandbox container", "poolID", poolID, "container", ctr.ID, "error", err)
		}
	}

	reapUnknownPools(
		r.poolsRoot(),
		r.cachePoolsRoot(),
		proxyagent.PoolsRoot(r.root, r.projectID),
		known, sandboxVolumeRetention, time.Now(), logger,
	)
	return nil
}

// reapUnknownPools reclaims the data, cache, and proxy subtrees of pools not in the
// known set. Data subtrees hold persistent sandbox data, so they get the same
// retention as an unheld sandbox tree; cache and proxy material
// are regenerable, so their subtrees are reaped once no retained data subtree
// remains.
func reapUnknownPools(dataPoolsRoot, cachePoolsRoot, proxyPoolsRoot string, known map[string]struct{}, retention time.Duration, now time.Time, logger *slog.Logger) {
	for _, poolID := range unknownPoolDirs(dataPoolsRoot, known, logger) {
		dir := filepath.Join(dataPoolsRoot, poolID)
		tombstone := filepath.Join(dir, poolDataTombstone)
		diedAt, ok := readSandboxTombstone(tombstone)
		if !ok {
			writeSandboxTombstone(tombstone, now, logger)
			continue
		}
		if now.Sub(diedAt) < retention {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			logger.Warn("reap orphan pool data", "poolID", poolID, "error", err)
			continue
		}
		_ = os.RemoveAll(filepath.Join(cachePoolsRoot, poolID))
		_ = os.RemoveAll(filepath.Join(proxyPoolsRoot, poolID))
		logger.Info("reaped orphan pool", "poolID", poolID, "deadFor", now.Sub(diedAt).Truncate(time.Minute).String())
	}
	// Cache and proxy leftovers whose data subtree is already gone are
	// regenerable: reap them immediately.
	for _, root := range []string{cachePoolsRoot, proxyPoolsRoot} {
		for _, poolID := range unknownPoolDirs(root, known, logger) {
			if _, err := os.Stat(filepath.Join(dataPoolsRoot, poolID)); err == nil {
				continue // its retention is tracked on the data side above
			}
			_ = os.RemoveAll(filepath.Join(root, poolID))
		}
	}
}

// unknownPoolDirs returns the pool-ID subdirectories under root that are not in
// the known set.
func unknownPoolDirs(root string, known map[string]struct{}, logger *slog.Logger) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("scan pools root", "root", root, "error", err)
		}
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := known[entry.Name()]; ok {
			continue
		}
		out = append(out, entry.Name())
	}
	return out
}

// reapUnheldSandboxVolumes is one pass of the sandbox tree reaper. root is this
// pool's own sandboxes directory and held answers for this pool alone, so the
// set is exactly as wide as the tree it is judged against.
//
// The trees are listed before the control plane is asked. A sandbox's row is
// written before any create reaches this pool, so every tree listed here whose
// sandbox exists is in the answer that follows; an import restores its tree
// before its row (ADR 0123 §3), and the retention window covers that.
func reapUnheldSandboxVolumes(ctx context.Context, root string, held HeldSandboxes, retention time.Duration, now time.Time, logger *slog.Logger) {
	trees, err := storedSandboxIDs(root)
	if err != nil {
		logger.Warn("scan sandbox volume root", "root", root, "error", err)
		return
	}
	if len(trees) == 0 {
		return
	}
	heldCtx, cancel := context.WithTimeout(ctx, sandboxVolumeHeldTimeout)
	ids, err := held(heldCtx)
	cancel()
	if err != nil {
		logger.Warn("list the sandboxes the control plane holds; reaping no sandbox trees this pass", "error", err)
		return
	}
	heldSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		heldSet[strings.TrimSpace(id)] = struct{}{}
	}
	for _, sandboxID := range trees {
		dir := filepath.Join(root, sandboxID)
		_ = os.Remove(filepath.Join(dir, legacySandboxVolumeTombstone))
		marker := filepath.Join(dir, sandboxVolumeUnheldMarker)
		if _, ok := heldSet[sandboxID]; ok {
			// Held, or held again: whatever clock an earlier answer started
			// no longer applies.
			_ = os.Remove(marker)
			continue
		}
		unheldAt, ok := readSandboxTombstone(marker)
		if !ok {
			// Said out loud: this starts the only window anyone has to notice
			// a control plane that has forgotten a sandbox it should hold.
			logger.Warn("sandbox tree is not held by the control plane; reaping it after retention", "sandboxID", sandboxID, "retention", retention.String())
			writeSandboxTombstone(marker, now, logger)
			continue
		}
		if now.Sub(unheldAt) < retention {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			logger.Warn("reap unheld sandbox volume", "sandboxID", sandboxID, "error", err)
			continue
		}
		logger.Info("reaped unheld sandbox volume", "sandboxID", sandboxID, "unheldFor", now.Sub(unheldAt).Truncate(time.Minute).String())
	}
}

func readSandboxTombstone(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func writeSandboxTombstone(path string, at time.Time, logger *slog.Logger) {
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		logger.Warn("stamp sandbox volume tombstone", "path", path, "error", err)
	}
}

// watchProxyMaterialEvents subscribes to the Docker event stream for managed
// sandbox container destroy events, then reconciles, then blocks kicking a
// debounced reconcile for each burst. It returns when the stream ends or errors
// so the caller can reconnect.
//
// The reconcile runs after the subscription is opened, and the subscription uses
// a Since timestamp captured before opening it, so the daemon replays any
// destroy that occurs in the window between opening the stream and the reconcile
// completing. This closes the race where a container deleted just after a
// startup reconcile would otherwise be missed until the next reconnect.
func (r *DockerSandboxRuntime) watchProxyMaterialEvents(ctx context.Context, logger *slog.Logger) error {
	since := time.Now()
	filters := client.Filters{}
	filters = filters.Add("type", string(events.ContainerEventType))
	filters = filters.Add("event", string(events.ActionDestroy))
	filters = filters.Add("label", sandboxLabelManaged+"=true")
	filters = filters.Add("label", sandboxLabelProject+"="+r.projectID)
	filters = filters.Add("label", sandboxLabelPool+"="+r.poolID)
	result := r.client.Events(ctx, client.EventsListOptions{
		Since:   fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
		Filters: filters,
	})

	// Reconcile once the subscription is established. Destroys from `since`
	// onward are buffered by the daemon and delivered on the stream below, so the
	// reconcile and the replayed events together cover every deletion.
	r.reconcileSandboxMaterial(ctx, logger, proxyMaterialGracePeriod)

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	backstop := time.NewTicker(proxyMaterialBackstopInterval)
	defer backstop.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-result.Err:
			return err
		case <-result.Messages:
			if !pending {
				pending = true
				debounce.Reset(proxyMaterialEventDebounce)
			}
		case <-debounce.C:
			pending = false
			r.reconcileProxyMaterial(ctx, logger)
		case <-backstop.C:
			r.reconcileSandboxMaterial(ctx, logger, proxyMaterialGracePeriod)
		}
	}
}

// adoptSourcePaths moves a source that was materialized under its seed-derived
// name to the slug the control plane assigned it.
//
// It exists because the pool was not told the slug at all until this change:
// every sandbox created before it holds its non-primary sources under the
// slugified reference key, which is the wrong directory for every path that
// addresses a source by slug -- the git route a client fetches from, the
// materialized marker, the mounts. Renaming is what makes those sandboxes whole
// again; materializing afresh would clone over the top of them and strand the
// work already committed there.
//
// Nothing to adopt is the overwhelmingly common case and costs one stat per
// source. A rename is only ever made into a name that is free, so a sandbox
// already holding both is left exactly as it is.
func (r *DockerSandboxRuntime) adoptSourcePaths(ctx context.Context, sandboxID string, sources []sandboxSource) error {
	for _, source := range sources {
		if source.keySlug == "" {
			continue
		}
		for _, pair := range [][2]string{
			{r.sandboxSourcePath(sandboxID, source.keySlug), r.sandboxSourcePath(sandboxID, source.slug)},
			{r.sandboxOriginPath(sandboxID, source.keySlug), r.sandboxOriginPath(sandboxID, source.slug)},
		} {
			from, to := pair[0], pair[1]
			if _, err := os.Stat(from); err != nil {
				continue
			}
			if _, err := os.Stat(to); err == nil {
				continue
			}
			if err := os.Rename(from, to); err != nil {
				return fmt.Errorf("adopt source %q from %s: %w", source.slug, filepath.Base(from), err)
			}
			slog.InfoContext(ctx, "adopted a sandbox source materialized under its old name",
				"sandboxId", sandboxID, "slug", source.slug, "from", from, "to", to)
		}
	}
	return nil
}

// GitOriginPath is where a source's origin is served from; see
// originLocation. A bare origin is probed for HEAD rather than `.git`, which
// is what every repository has and a directory does not, and it exists from
// provisioning onwards, before the source has been delivered, which is the
// whole point — it is what the client pushes into while the sandbox parks
// (ADR 0058 §4).
func (r *DockerSandboxRuntime) GitOriginPath(ctx context.Context, sandboxID, slug string) (GitRepositoryLocation, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return GitRepositoryLocation{}, err
	}
	return r.originLocation(sandboxID, slug, sb.Env)
}

// sandboxUserFromEnv recovers the sandbox's resolved user from the
// DISCOBOX_USER_UID/DISCOBOX_USER_GID env vars envWithSandboxUser stamped onto
// the container, defaulting to root (uid/gid 0) to match resolveSandboxUser.
func sandboxUserFromEnv(env map[string]string) (uid, gid int) {
	if parsed, err := strconv.Atoi(env["DISCOBOX_USER_UID"]); err == nil {
		uid = parsed
	}
	if parsed, err := strconv.Atoi(env["DISCOBOX_USER_GID"]); err == nil {
		gid = parsed
	}
	return uid, gid
}

func (r *DockerSandboxRuntime) SandboxDialer(ctx context.Context, sandboxID string, port int) (Dialer, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid sandbox port %d", port)
	}
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.filters(sandboxID)})
	if err != nil {
		return nil, err
	}
	if len(containers.Items) == 0 {
		return nil, ErrNotFound
	}
	inspect, err := r.client.ContainerInspect(ctx, containers.Items[0].ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return containerDialer(sandboxID, inspect.Container, port)
}

// SandboxServesWorktree reads the label the sandbox's image carries: the
// container's labels are its image's, so an image too old to have the label
// runs an agent too old to serve the route (harness.WorktreeGitLabel).
func (r *DockerSandboxRuntime) SandboxServesWorktree(ctx context.Context, sandboxID string) error {
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.filters(sandboxID)})
	if err != nil {
		return err
	}
	if len(containers.Items) == 0 {
		return ErrNotFound
	}
	if containers.Items[0].Labels[harness.WorktreeGitLabel] != harness.WorktreeGitLabelValue {
		return fmt.Errorf("%w; run `discobox admin box upgrade %s`, then try again", ErrWorktreeUnsupported, sandboxID)
	}
	return nil
}

// containerDialer reaches a port in a sandbox container at its address on the
// pool's network, the one shape of reachability a Docker pool has: the pool
// and its sandboxes share that network.
func containerDialer(sandboxID string, inspect container.InspectResponse, port int) (Dialer, error) {
	ip := containerIPAddress(inspect)
	if ip == "" {
		return nil, fmt.Errorf("sandbox %q does not have an inspectable IP address", sandboxID)
	}
	address := net.JoinHostPort(ip, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", address)
	}, nil
}

// waitForSandboxAgent blocks until the sandbox-agent answers /healthz, and is
// on the critical path of every create: nothing the caller asked for exists
// until it returns.
//
// The container is resolved once, by ID, rather than looked up each pass.
// Finding a sandbox by label costs a ContainerList -- ~20ms against a local
// daemon, against ~0.7ms for inspecting a container already named -- and the
// previous shape paid it twice per pass, once in GetSandbox and again to
// resolve the sandbox all over again for its address. That was ~43ms of
// Docker traffic per pass on a loop whose whole job is to notice a state
// change quickly.
//
// The per-pass inspect stays. It is what detects a container that died on the
// way up, and at sub-millisecond cost there is nothing to gain by sampling the
// container's fate less often than its agent's health.
//
// This does not make a create much faster, and it was not expected to once the
// wait was measured: ~859ms of it is the sandbox genuinely booting -- ~433ms of
// PID 1 provisioning before systemd starts, ~372ms of systemd, then the agent
// binding its port. Polling overhead never had more than ~120ms to give back.
// Shortening this loop is not the lever; the boot is.
func (r *DockerSandboxRuntime) waitForSandboxAgent(ctx context.Context, sandboxID string) error {
	ctx, cancel := context.WithTimeout(ctx, sandboxAgentReadyTimeout)
	defer cancel()
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.filters(sandboxID)})
	if err != nil {
		return err
	}
	if len(containers.Items) == 0 {
		return ErrNotFound
	}
	containerID := containers.Items[0].ID
	var lastErr error
	for {
		inspect, err := r.client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			lastErr = err
		} else {
			sb := r.sandboxFromInspect(ctx, inspect.Container)
			if err := sandboxAgentTerminalStateError(sb); err != nil {
				return err
			}
			// Dial what the same inspect that just reported the container's
			// state says, rather than asking the daemon a second time for an
			// address that cannot change while it runs. It is the dial
			// SandboxDialer hands everyone else.
			dial, dialErr := containerDialer(sandboxID, inspect.Container, SandboxAgentPort)
			if dialErr != nil {
				lastErr = dialErr
			} else {
				lastErr = sandboxAgentHealthy(ctx, dial)
				if lastErr == nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("wait for sandbox-agent: %w", lastErr)
			}
			return ctx.Err()
		case <-time.After(sandboxAgentPollInterval):
		}
	}
}

// sandboxAgentHealthy asks the sandbox-agent's /healthz once, over dial.
func sandboxAgentHealthy(ctx context.Context, dial Dialer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, HTTPURL(SandboxAgentPort, "/healthz").String(), nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: dial.Transport()}).Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sandbox-agent health returned %s", resp.Status)
	}
	return nil
}

func sandboxAgentTerminalStateError(sb *Sandbox) error {
	if sb == nil {
		return nil
	}
	switch sb.Status {
	case StatusFailed, StatusStopped, StatusRemoved:
		message := fmt.Sprintf("sandbox %q reached terminal status %q before sandbox-agent became healthy", sb.SandboxID, sb.Status)
		if sb.Error != "" {
			message += ": " + sb.Error
		}
		return errors.New(message)
	default:
		return nil
	}
}

func (r *DockerSandboxRuntime) filters(sandboxID string) client.Filters {
	args := client.Filters{}
	args = args.Add("label", sandboxLabelManaged+"=true")
	args = args.Add("label", sandboxLabelProject+"="+r.projectID)
	args = args.Add("label", sandboxLabelPool+"="+r.poolID)
	if strings.TrimSpace(sandboxID) != "" {
		args = args.Add("label", sandboxLabelSandbox+"="+sandboxID)
	}
	return args
}

func (r *DockerSandboxRuntime) labels(sandboxID, specFingerprint, projectLayer string) map[string]string {
	labels := map[string]string{
		sandboxLabelManaged: "true",
		sandboxLabelProject: r.projectID,
		sandboxLabelPool:    r.poolID,
		sandboxLabelSandbox: sandboxID,
	}
	if specFingerprint != "" {
		labels[sandboxLabelSpec] = specFingerprint
	}
	if r.poolPublicKey() != "" {
		labels[sandboxLabelRuntimeConfig] = "true"
	}
	if projectLayer != "" {
		labels[sandboxLabelProjectLayer] = projectLayer
	}
	return labels
}

func (r *DockerSandboxRuntime) sandboxFromInspect(ctx context.Context, inspect container.InspectResponse) *Sandbox {
	createdAt, _ := time.Parse(time.RFC3339Nano, inspect.Created)
	sandboxID := inspect.Config.Labels[sandboxLabelSandbox]
	sb := &Sandbox{
		ID:        inspect.ID,
		SandboxID: sandboxID,
		Status:    StatusCreated,
		Image:     inspect.Config.Image,
		CreatedAt: createdAt,
		Metadata: map[string]string{
			"pool_id": r.poolID,
		},
		Env: envMap(inspect.Config.Env),
	}
	if inspect.State != nil {
		if started, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt); err == nil && !started.IsZero() {
			sb.StartedAt = &started
		}
		if stopped, err := time.Parse(time.RFC3339Nano, inspect.State.FinishedAt); err == nil && !stopped.IsZero() {
			sb.StoppedAt = &stopped
		}
		sb.Error = inspect.State.Error
		switch {
		case inspect.State.Running:
			sb.Status = StatusRunning
		case inspect.State.Dead || inspect.State.OOMKilled || inspect.State.Error != "":
			sb.Status = StatusFailed
		case inspect.State.Status == "created":
			sb.Status = StatusCreated
		default:
			sb.Status = StatusStopped
		}
		if sb.Status == StatusFailed || sb.Status == StatusStopped {
			sb.Error = dockerSandboxExitError(inspect, r.containerLogTail(ctx, inspect))
		}
	}
	return sb
}

func (r *DockerSandboxRuntime) containerLogTail(ctx context.Context, inspect container.InspectResponse) string {
	logs, err := r.client.ContainerLogs(ctx, inspect.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "20",
	})
	if err != nil {
		return ""
	}
	defer logs.Close()

	var buf bytes.Buffer
	if inspect.Config != nil && inspect.Config.Tty {
		_, _ = io.Copy(&buf, io.LimitReader(logs, 64*1024))
	} else {
		_, _ = stdcopy.StdCopy(&buf, &buf, io.LimitReader(logs, 64*1024))
	}
	return compactLogTail(buf.String())
}

func dockerSandboxExitError(inspect container.InspectResponse, logTail string) string {
	if inspect.State == nil {
		return ""
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("container exited with status %q and exit code %d", inspect.State.Status, inspect.State.ExitCode))
	if inspect.State.OOMKilled {
		parts = append(parts, "oom killed")
	}
	if stateErr := strings.TrimSpace(inspect.State.Error); stateErr != "" {
		parts = append(parts, "state error: "+stateErr)
	}
	if logTail != "" {
		parts = append(parts, "last logs: "+logTail)
	}
	return strings.Join(parts, "; ")
}

func compactLogTail(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return ""
	}
	text := strings.Join(out, " | ")
	if len(text) > 2048 {
		return text[:2048] + "..."
	}
	return text
}

// MemorySandboxRuntime is a lightweight runtime for tests and non-Docker embeds.
type MemorySandboxRuntime struct {
	mu        sync.Mutex
	sandboxes map[string]*Sandbox
	// archived stands in for the on-disk marker: an entry here has no sandbox
	// in the map (archiving drops the container) but is still held as data.
	archived    map[string]struct{}
	gitOrigins  map[string]map[string]string
	liveOrigins map[string]map[string]GitRepositoryLocation
	// trees stands in for the durable tree on disk: the tar bytes a sandbox was
	// imported with, handed back by an export.
	trees map[string][]byte
	// appliedRuntimeConfig is the revision the status poll last reported for
	// each sandbox (ConvergeRuntimeConfig).
	appliedRuntimeConfig map[string]int64
}

func NewMemorySandboxRuntime() *MemorySandboxRuntime {
	return &MemorySandboxRuntime{
		sandboxes:   map[string]*Sandbox{},
		archived:    map[string]struct{}{},
		gitOrigins:  map[string]map[string]string{},
		liveOrigins: map[string]map[string]GitRepositoryLocation{},
		trees:       map[string][]byte{},
	}
}

func (r *MemorySandboxRuntime) ListSandboxes(context.Context) ([]*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Sandbox, 0, len(r.sandboxes))
	for _, sb := range r.sandboxes {
		out = append(out, cloneSandbox(sb))
	}
	return out, nil
}

// StoredSandboxIDs is the containers plus the archived entries, which is what
// this runtime has instead of a disk: an archived sandbox has no entry in the
// map and is still held as data.
func (r *MemorySandboxRuntime) StoredSandboxIDs(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.sandboxes)+len(r.archived))
	for id := range r.sandboxes {
		ids = append(ids, id)
	}
	for id := range r.archived {
		if _, ok := r.sandboxes[id]; !ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (r *MemorySandboxRuntime) CreateSandbox(_ context.Context, req *workerapimodel.PoolSandboxCreateRequest) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req == nil {
		return nil, fmt.Errorf("sandbox create request is required")
	}
	now := time.Now().UTC()
	sb := &Sandbox{ID: req.SandboxId, SandboxID: req.SandboxId, Status: StatusRunning, Image: optString(req.Config.Image), CreatedAt: now, StartedAt: &now, Env: copyMap(map[string]string(optSandboxConfigEnv(req.Config.Env)))}
	r.sandboxes[req.SandboxId] = sb
	// Creating against a retained tree is what unarchive is (ADR 0022 §6).
	delete(r.archived, req.SandboxId)
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) GetSandbox(_ context.Context, sandboxID string) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, ErrNotFound
	}
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) UpdateSandbox(_ context.Context, sandboxID string, req *workerapimodel.PoolSandboxUpdateRequest) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, ErrNotFound
	}
	if req != nil {
		if config, ok := req.Config.Get(); ok {
			if image := optString(config.Image); image != "" {
				sb.Image = image
			}
		}
	}
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) ArchiveSandbox(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sandboxes, sandboxID)
	r.archived[sandboxID] = struct{}{}
	return nil
}

func (r *MemorySandboxRuntime) DeleteSandbox(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sandboxes, sandboxID)
	delete(r.archived, sandboxID)
	delete(r.trees, sandboxID)
	return nil
}

// ExportTree hands back whatever tree this sandbox was imported with, and an
// empty archive for one that was created here: there is no disk to walk, so
// what an export means for this runtime is exactly what an import put in.
func (r *MemorySandboxRuntime) ExportTree(_ context.Context, sandboxID string, _ TreeImage) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sb, ok := r.sandboxes[sandboxID]; ok && sb.Status == StatusRunning {
		return nil, ErrSandboxRunning
	}
	tree, ok := r.trees[sandboxID]
	if !ok {
		if _, known := r.sandboxes[sandboxID]; !known {
			if _, archived := r.archived[sandboxID]; !archived {
				return nil, ErrNotFound
			}
		}
		tree = emptyTarArchive()
	}
	return io.NopCloser(bytes.NewReader(tree)), nil
}

func (r *MemorySandboxRuntime) ImportTree(_ context.Context, sandboxID string, tree io.Reader) error {
	data, err := io.ReadAll(tree)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.trees[sandboxID]; ok {
		return ErrTreeExists
	}
	if _, ok := r.sandboxes[sandboxID]; ok {
		return ErrTreeExists
	}
	r.trees[sandboxID] = data
	return nil
}

// emptyTarArchive is a tree archive with no files: nothing but the SHA256SUMS
// every reader of a tree requires.
func emptyTarArchive() []byte {
	var buf bytes.Buffer
	_ = tarsums.NewWriter(&buf).Close()
	return buf.Bytes()
}

func (r *MemorySandboxRuntime) SyncKnownPools(context.Context, []string) error {
	return nil
}

// ClearCache stops every running sandbox. There is no cache on disk to empty.
func (r *MemorySandboxRuntime) ClearCache(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var stopped []string
	now := time.Now().UTC()
	for sandboxID, sb := range r.sandboxes {
		if sb.Status != StatusRunning {
			continue
		}
		sb.Status = StatusStopped
		sb.StoppedAt = &now
		stopped = append(stopped, sandboxID)
	}
	sort.Strings(stopped)
	return stopped, nil
}

func (r *MemorySandboxRuntime) StartSandbox(_ context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		if _, archived := r.archived[sandboxID]; archived {
			return ErrArchived
		}
		return ErrNotFound
	}
	if sb.Status == StatusRunning {
		return nil
	}
	now := time.Now().UTC()
	sb.Status = StatusRunning
	sb.StartedAt = &now
	return nil
}

func (r *MemorySandboxRuntime) StopSandbox(_ context.Context, sandboxID string, _ *workerapimodel.PoolSandboxOperationRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return ErrNotFound
	}
	now := time.Now().UTC()
	sb.Status = StatusStopped
	sb.StoppedAt = &now
	return nil
}

func (r *MemorySandboxRuntime) RestartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error {
	if err := r.StopSandbox(ctx, sandboxID, req); err != nil {
		return err
	}
	return r.StartSandbox(ctx, sandboxID, req)
}

func (r *MemorySandboxRuntime) EnsureSandboxRunning(ctx context.Context, sandboxID string, _ bool) error {
	return r.StartSandbox(ctx, sandboxID, nil)
}

// SandboxBooting is always false: a memory sandbox is running the moment it is
// started.
func (r *MemorySandboxRuntime) SandboxBooting(string) bool {
	return false
}

func (r *MemorySandboxRuntime) GitOriginPath(_ context.Context, sandboxID, slug string) (GitRepositoryLocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sandboxes[sandboxID] == nil {
		return GitRepositoryLocation{}, ErrNotFound
	}
	if live, ok := r.liveOrigins[sandboxID][slug]; ok {
		return live, nil
	}
	origins := r.gitOrigins[sandboxID]
	if origins == nil || origins[slug] == "" {
		return GitRepositoryLocation{}, fmt.Errorf("%w: %s", ErrRepositoryNotFound, slug)
	}
	return GitRepositoryLocation{Path: origins[slug], UID: -1, GID: -1}, nil
}

// SandboxServesWorktree answers for any sandbox it holds: a memory sandbox has
// no image to be too old.
func (r *MemorySandboxRuntime) SandboxServesWorktree(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sandboxes[sandboxID] == nil {
		return ErrNotFound
	}
	return nil
}

// ConvergeRuntimeConfig records what the poll reported: a memory sandbox has no
// agent to deliver to.
func (r *MemorySandboxRuntime) ConvergeRuntimeConfig(_ context.Context, sandboxID string, applied int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.appliedRuntimeConfig == nil {
		r.appliedRuntimeConfig = map[string]int64{}
	}
	r.appliedRuntimeConfig[sandboxID] = applied
	return nil
}

// AppliedRuntimeConfig is the revision the status poll last reported for a
// sandbox, and false when it has reported none.
func (r *MemorySandboxRuntime) AppliedRuntimeConfig(sandboxID string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	applied, ok := r.appliedRuntimeConfig[sandboxID]
	return applied, ok
}

// SandboxDialer reaches nothing: a memory sandbox has nothing in it to dial.
func (r *MemorySandboxRuntime) SandboxDialer(context.Context, string, int) (Dialer, error) {
	return nil, ErrNotFound
}

// WatchSandboxStates publishes a complete sync on start and on the interval.
// There is no event stream behind this runtime to take deltas from, and the
// sync alone is what makes the channel correct.
func (r *MemorySandboxRuntime) WatchSandboxStates(ctx context.Context, logger *slog.Logger, publish func(context.Context, SandboxStateBatch) error) {
	if logger == nil {
		logger = slog.Default()
	}
	if publish == nil {
		return
	}
	sync := time.NewTicker(sandboxStateSyncInterval)
	defer sync.Stop()
	for {
		r.mu.Lock()
		states := make([]SandboxStateObservation, 0, len(r.sandboxes))
		for sandboxID, sb := range r.sandboxes {
			states = append(states, SandboxStateObservation{SandboxID: sandboxID, State: stateFromStatus(sb.Status)})
		}
		r.mu.Unlock()
		batch := SandboxStateBatch{Complete: true, ReportedAt: time.Now().UTC(), States: states}
		if err := publish(ctx, batch); err != nil && ctx.Err() == nil {
			logger.Warn("publish sandbox state sync", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-sync.C:
		}
	}
}

// WatchSandboxProgress holds nothing: no work done here takes long enough to
// report.
func (r *MemorySandboxRuntime) WatchSandboxProgress(ctx context.Context, _ func(context.Context, SandboxProgressObservation) error) {
	<-ctx.Done()
}

// WatchSandboxVolumes reaps nothing. The archived and imported trees this
// runtime holds stay until DeleteSandbox, whether the control plane holds them
// or not.
func (r *MemorySandboxRuntime) WatchSandboxVolumes(ctx context.Context, _ *slog.Logger, _ HeldSandboxes) {
	<-ctx.Done()
}

// WatchProxyMaterial reclaims nothing: this runtime stages no proxy material.
func (r *MemorySandboxRuntime) WatchProxyMaterial(ctx context.Context, _ *slog.Logger) {
	<-ctx.Done()
}

func (r *MemorySandboxRuntime) SetGitOriginPath(sandboxID, slug, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gitOrigins[sandboxID] == nil {
		r.gitOrigins[sandboxID] = map[string]string{}
	}
	r.gitOrigins[sandboxID][slug] = path
}

// SetLiveGitOrigin serves path as slug's live origin, advertising refs, ahead
// of any bare origin SetGitOriginPath gave it.
func (r *MemorySandboxRuntime) SetLiveGitOrigin(sandboxID, slug, path string, refs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.liveOrigins[sandboxID] == nil {
		r.liveOrigins[sandboxID] = map[string]GitRepositoryLocation{}
	}
	r.liveOrigins[sandboxID][slug] = GitRepositoryLocation{Path: path, UID: -1, GID: -1, Live: true, Refs: refs}
}

func sandboxContainerName(poolID, sandboxID string) string {
	name := "discobox-sandbox-" + poolID + "-" + sandboxID
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, name)
	return strings.Trim(name, "-_.")
}

// sandboxHostname is what `hostname` reports inside the sandbox: the sandbox
// ID spelled with a hyphen (id.Hostname, "sbx-<random>"), so a shell prompt
// names the sandbox a user can address instead of a random container ID — the
// CLI and the SSH ingress read that spelling back as the ID (id.Canonical). The
// underscore is not legal in a hostname. The result is reduced to a legal RFC
// 1123 label; an ID that survives nothing usable returns "", which leaves
// Docker's container-ID default in place.
func sandboxHostname(sandboxID string) string {
	host := strings.ToLower(strings.TrimSpace(id.Hostname(sandboxID)))
	host = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, host)
	if len(host) > 63 {
		host = host[:63]
	}
	return strings.Trim(host, "-")
}

// poolCacheRoot is the shared cache directory for every sandbox this pool runs
// in this project (ADRs 0007 and 0013). Its independent top-level root lets a
// provider mount disposable storage at /var/lib/discobox/cache without moving
// durable sandbox state.
func (r *DockerSandboxRuntime) poolCacheRoot() string {
	return r.root.PoolCache(r.projectID, r.poolID)
}

// sandboxesRoot is the parent of every sandbox's per-sandbox volume tree for
// this pool. The volume reaper scans only under here, which is why it can never
// touch another pool's data.
func (r *DockerSandboxRuntime) sandboxesRoot() string {
	return r.root.PoolSandboxes(r.projectID, r.poolID)
}

// poolsRoot is the parent of every pool's data subtree for this project on the
// shared host. The pool-sync reaper enumerates it to find orphaned pools.
func (r *DockerSandboxRuntime) poolsRoot() string {
	return r.root.ProjectPools(r.projectID)
}

func (r *DockerSandboxRuntime) cachePoolsRoot() string {
	return r.root.ProjectCachePools(r.projectID)
}

// projectFilters matches every managed sandbox container for this project
// across all pools (unlike filters, which is scoped to this agent's own pool).
func (r *DockerSandboxRuntime) projectFilters() client.Filters {
	args := client.Filters{}
	args = args.Add("label", sandboxLabelManaged+"=true")
	args = args.Add("label", sandboxLabelProject+"="+r.projectID)
	return args
}

func (r *DockerSandboxRuntime) sandboxDataRootPath(sandboxID string) string {
	return r.root.SandboxData(r.projectID, r.poolID, sandboxID)
}

func (r *DockerSandboxRuntime) sourceDataPath(sourceKey string) string {
	return r.root.SourceData(r.projectID, r.poolID, sourceKey)
}

func (r *DockerSandboxRuntime) sandboxSourceDataPath(sandboxID, slug string) string {
	return r.root.SandboxSourceData(r.projectID, r.poolID, sandboxID, slug)
}

func (r *DockerSandboxRuntime) sandboxConfigRoot(sandboxID string) string {
	return r.root.SandboxConfig(r.projectID, r.poolID, sandboxID)
}

func (r *DockerSandboxRuntime) sandboxSecretsRoot(sandboxID string) string {
	return r.root.SandboxSecrets(r.projectID, r.poolID, sandboxID)
}

func (r *DockerSandboxRuntime) sandboxSourcesRoot(sandboxID string) string {
	return r.root.SandboxSources(r.projectID, r.poolID, sandboxID)
}

func (r *DockerSandboxRuntime) sandboxSourcePath(sandboxID, slug string) string {
	return filepath.Join(r.sandboxSourcesRoot(sandboxID), slug)
}

// sandboxOriginPath is the bare repository a push-delivered source is pushed
// into and served out of, over the git-origins route the sandbox fetches from
// (ADR 0058 §1, ADR 0126 §4).
func (r *DockerSandboxRuntime) sandboxOriginPath(sandboxID, slug string) string {
	return filepath.Join(r.root.SandboxOrigins(r.projectID, r.poolID, sandboxID), slug+".git")
}

func containerIPAddress(inspect container.InspectResponse) string {
	if inspect.NetworkSettings == nil {
		return ""
	}
	names := make([]string, 0, len(inspect.NetworkSettings.Networks))
	for name := range inspect.NetworkSettings.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if endpoint := inspect.NetworkSettings.Networks[name]; endpoint != nil && endpoint.IPAddress.IsValid() {
			return endpoint.IPAddress.String()
		}
	}
	return ""
}

func optString(opt workerclient.OptString) string {
	v, _ := opt.Get()
	return v
}

// unsetID marks an id the request did not give. It is the POSIX chown sentinel
// for "leave this field unchanged", so an unknown id is passed through rather
// than guessed at.
const unsetID = -1

// chownID renders an id for chown(2), whose own vocabulary for "leave this
// field unchanged" is -1. That is the only place -1 survives: as a value the
// syscall defines, at the moment of the call. Everywhere else absent is nil
// (ADR 0033 §3), because a sentinel is only ever as good as every conversion
// between it and the real thing -- and the conversion that turned unset into 0
// is what chowned sandbox source trees to root.
func chownID(v *int64) int {
	if v == nil {
		return -1
	}
	return int(*v)
}

// sandboxSkills is the request's skills in the bootstrap's shape. The pool
// checks nothing about them: the control plane refused what it would not
// carry, and the sandbox refuses what it will not write.
func sandboxSkills(in workerclient.SandboxConfigSkills) sandboxconfig.Skills {
	if len(in) == 0 {
		return nil
	}
	out := make(sandboxconfig.Skills, len(in))
	for name, skill := range in {
		var files []sandboxconfig.SkillFile
		for _, file := range skill.Files {
			files = append(files, sandboxconfig.SkillFile{
				Path:       file.Path,
				Content:    file.Content,
				Executable: file.Executable.Or(false),
			})
		}
		out[name] = sandboxconfig.Skill{Skill: skill.Skill, Files: files}
	}
	return out
}

// resolveSandboxUser reads the request's user without completing it.
//
// The pool agent cannot complete one: the account and the group live in the
// image, and boot may still have to create them (ADR 0025 §4). It calls
// sandboxuser.Merge, which performs no lookups, so this is enforced by the API
// it is given rather than by remembering a rule -- and it holds one layer up
// too, since this module cannot import the resolver at all.
//
// Nothing is invented. A missing gid does not become the uid, a bare name does
// not become uid 1000, an absent user does not become root, and a name does not
// become /home/<name>: that last one was written here under a comment claiming
// nothing was invented, and it is a guess about the image's own passwd file
// made from outside the image. What the request did not say stays unset, and
// the sandbox answers for it later.
func resolveSandboxUser(paths sandboxpath.Paths, req *workerapimodel.PoolSandboxCreateRequest) sandboxuser.User {
	if req == nil {
		return sandboxuser.User{}
	}
	user, ok := req.Config.User.Get()
	if !ok {
		return sandboxuser.User{}
	}
	requested := &sandboxuser.User{
		Name:             strings.TrimSpace(optString(user.Name)),
		GroupName:        strings.TrimSpace(optString(user.GroupName)),
		HomeDirectory:    paths.Rooted(optString(user.HomeDirectory)),
		AdditionalGroups: append([]string(nil), user.AdditionalGroups...),
	}
	if uid, ok := user.UID.Get(); ok {
		requested.UID = sandboxuser.ID(uid)
	}
	if gid, ok := user.Gid.Get(); ok {
		requested.GID = sandboxuser.ID(gid)
	}
	return sandboxuser.Merge(sandboxuser.Layers{Request: requested})
}

// sourceWorkingDirectory is the directory the primary source asks a sandbox
// to start in, as it asked, when it is an absolute path in the sandbox: judged
// by the sandbox's platform. One that is not names no place there — a
// container runtime refuses a relative one outright — and is refused with the
// platform as the reason, rather than quietly replaced by the image's default.
func sourceWorkingDirectory(paths sandboxpath.Paths, req *workerapimodel.PoolSandboxCreateRequest) (string, error) {
	if req == nil {
		return "", nil
	}
	source, ok := req.Config.Source.Get()
	if !ok {
		return "", nil
	}
	destination, ok := source.Destination.Get()
	if !ok {
		return "", nil
	}
	directory := optString(destination.WorkingDirectory)
	if directory != "" && !paths.IsAbs(directory) {
		return "", fmt.Errorf("source working directory %q is not an absolute path in a %s sandbox", directory, paths.OS())
	}
	return directory, nil
}

// normalizeSandboxConfig applies provider-owned path defaults before the
// configuration is used for either bind mounts or the public sandbox manifest.
// This keeps manifest consumers on the documented SandboxConfig contract while
// ensuring they observe the paths the runtime actually mounted.
func normalizeSandboxConfig(paths sandboxpath.Paths, config *workerapimodel.SandboxConfig) {
	if config == nil {
		return
	}
	source, ok := config.Source.Get()
	if !ok {
		return
	}
	destination, _ := source.Destination.Get()
	directory := paths.Rooted(optString(destination.Directory))
	if directory == "" {
		directory = paths.WorkingRoot()
	}
	destination.Directory = workerclient.NewOptString(directory)
	source.Destination = workerclient.NewOptGitSourceDestination(destination)
	config.Source = workerclient.NewOptGitSource(source)
}

type sandboxSource struct {
	slug   string
	target string
	git    workerapimodel.GitSource
	// keySlug is the slug this source would have had from its seed alone. A
	// sandbox created before the control plane sent the slug at all was
	// materialized under this name, and adoptSourcePaths renames those
	// directories to the real slug rather than leaving the work behind them
	// unreachable. Empty when it is the slug already.
	keySlug string
}

func sandboxSources(paths sandboxpath.Paths, req *workerapimodel.PoolSandboxCreateRequest) []sandboxSource {
	if req == nil {
		return nil
	}
	var out []sandboxSource
	used := map[string]struct{}{}
	if source, ok := req.Config.Source.Get(); ok {
		out = append(out, sandboxSourceFor(paths, sandboxconfig.PrimarySourceSlug, source, paths.WorkingRoot(), used))
	}
	if refs, ok := req.Config.SourceCodeReferences.Get(); ok {
		keys := make([]string, 0, len(refs))
		for key := range refs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			source := refs[key]
			defaultTarget := paths.Rooted(key)
			if defaultTarget == "" {
				defaultTarget = paths.Join(paths.WorkingRoot(), defaultSourceSlug(source, key))
			}
			out = append(out, sandboxSourceFor(paths, key, source, defaultTarget, used))
		}
	}
	return out
}

func sandboxSourceFor(paths sandboxpath.Paths, seed string, source workerapimodel.GitSource, defaultTarget string, used map[string]struct{}) sandboxSource {
	slug := sourceSlug(source, seed, used)
	target := defaultTarget
	if destination, ok := source.Destination.Get(); ok {
		if directory := paths.Rooted(optString(destination.Directory)); directory != "" {
			target = directory
		}
	}
	if target == "" {
		target = paths.Join(paths.WorkingRoot(), slug)
	}
	keySlug := slugifySource(seed)
	if keySlug == slug {
		keySlug = ""
	}
	return sandboxSource{slug: slug, target: target, git: source, keySlug: keySlug}
}

func sourceSlug(source workerapimodel.GitSource, seed string, used map[string]struct{}) string {
	base := defaultSourceSlug(source, seed)
	slug := base
	for i := 2; ; i++ {
		if _, ok := used[slug]; !ok {
			used[slug] = struct{}{}
			return slug
		}
		slug = fmt.Sprintf("%s-%d", strings.TrimRight(base[:min(len(base), 61)], "-"), i)
	}
}

func defaultSourceSlug(source workerapimodel.GitSource, seed string) string {
	base := slugifySource(optString(source.Slug))
	if base == "" {
		base = slugifySource(seed)
	}
	if base == "" {
		base = "source"
	}
	return base
}

func slugifySource(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case b.Len() > 0 && !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
		if b.Len() >= 63 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func cleanAbsPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !filepath.IsAbs(value) {
		return ""
	}
	parts := make([]string, 0, strings.Count(value, string(filepath.Separator))+1)
	for _, part := range strings.Split(value, string(filepath.Separator)) {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return string(filepath.Separator)
	}
	return string(filepath.Separator) + filepath.Join(parts...)
}

// daemonPath converts a path under the runtime's root into the path this pool's
// Docker daemon sees. Every mount source handed to the daemon goes through it;
// nothing else needs to, because everything else is read and written here.
func (r *DockerSandboxRuntime) daemonPath(path string) string {
	return r.root.HostMapping(r.hostStateRoot).HostPath(path)
}

// ensureOriginHead points a pushed origin repository's HEAD at a branch that
// exists in it. HEAD is what a clone resolves origin/HEAD from, and origin/HEAD
// is the upstream ref a source checked out at a bare commit or tag tracks
// (sourceUpstreamRef), so a HEAD left pointing at `git init`'s
// init.defaultBranch — a branch the client may never push — costs that source its
// diff base and makes the clone warn.
//
// A source that names its branch has HEAD pointed at it when its origin is
// made (initGitOrigin), so the sandbox, which clones as soon as the push lands,
// clones a HEAD that already resolves; this asserts it again for an origin made
// before that. The branch a source that names none lands on is the client's
// choice, which is only known once the push has landed: that HEAD is pointed at
// the first branch that arrived, by the create that resumes the source and by
// the status poll's settle, whichever comes first (headOriginAtWhatArrived).
func ensureOriginHead(ctx context.Context, originPath string, source workerapimodel.GitSource, uid, gid int) error {
	if branch := gitSourceInitialBranch(source); branch != "" {
		return runGit(ctx, originPath, uid, gid, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	}
	return headOriginAtWhatArrived(ctx, originPath, uid, gid)
}

// headOriginAtWhatArrived points a bare origin's HEAD at the first branch the
// client pushed, unless it already resolves.
func headOriginAtWhatArrived(ctx context.Context, originPath string, uid, gid int) error {
	if gitHasCommits(ctx, originPath, uid, gid) {
		return nil
	}
	ref := firstGitBranch(ctx, originPath, uid, gid)
	if ref == "" {
		return nil
	}
	return runGit(ctx, originPath, uid, gid, "symbolic-ref", "HEAD", ref)
}

// gitOriginHasRefs reports whether the client has pushed anything into a bare
// origin repository yet.
//
// HEAD is no help here, which is why this is not gitHasCommits: a bare
// repository's HEAD points at init.defaultBranch until something sets it, and
// that is a branch the client may never push — so HEAD stays unresolvable no
// matter how much has arrived.
func gitOriginHasRefs(ctx context.Context, repo string, uid, gid int) bool {
	return firstGitBranch(ctx, repo, uid, gid) != ""
}

func firstGitBranch(ctx context.Context, repo string, uid, gid int) string {
	out, err := runGitOutput(ctx, repo, uid, gid, nil, "for-each-ref", "--count=1", "--format=%(refname)", "refs/heads")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// localGitDirectory is the Git directory of the repository at local, which is
// what a clone-delivered source's live origin serves (ADR 0126 §4).
// Only an absolute path names one: anything else would be resolved against
// whatever directory the process or the daemon happens to be in.
func localGitDirectory(local string) (string, error) {
	if !filepath.IsAbs(local) {
		return "", fmt.Errorf("source localDirectory %q is not an absolute path", local)
	}
	return filepath.Join(filepath.Clean(local), ".git"), nil
}

// checkLocalGitDirectory refuses a clone-delivered source whose .git is not a
// real directory, before a sandbox is made to clone from it.
//
// A linked worktree or a submodule checkout has a file there naming a Git
// directory elsewhere, and a symlink resolves wherever it points — the
// repository root itself included. Either way it would no longer be the
// repository's own Git directory, which is the whole of what ADR 0093 permits
// a sandbox to see, and the live origin refuses to serve it. A current client reports such a
// repository and the server delivers it by push, so reaching this is an older
// client, and the failure names the path.
func (r *DockerSandboxRuntime) checkLocalGitDirectory(source workerapimodel.GitSource) error {
	if gitSourceAwaitsPush(source) {
		return nil
	}
	local := strings.TrimSpace(optString(source.LocalDirectory))
	if local == "" {
		return nil
	}
	gitDir, err := localGitDirectory(local)
	if err != nil {
		return err
	}
	info, err := os.Lstat(hostMountedLocalDirectory(gitDir, r.hostMountPrefix))
	if err != nil {
		return fmt.Errorf("stat source Git directory %s: %w", gitDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source Git directory %s is not a directory (a linked worktree or submodule checkout is delivered by push)", gitDir)
	}
	return nil
}

// gitSourceAwaitsPush reports whether the client delivers this source by
// pushing it in, rather than the sandbox fetching it.
//
// Delivery is read from the source, never inferred from a missing URL: a source
// with nothing to clone from is a malformed request, and treating it as "wait
// for a push" would turn that mistake into a sandbox that silently starts with
// an empty workspace.
func gitSourceAwaitsPush(source workerapimodel.GitSource) bool {
	delivery, ok := source.Delivery.Get()
	return ok && string(delivery) == string(workerclient.GitSourceDeliveryPush)
}

// initGitOrigin creates the bare repository a client pushes a push-delivered
// source into, and which the sandbox sees as its origin (ADR 0058 §1). It is
// created at provisioning time, before the source exists, and survives every
// later create: a re-push lands in the same repository the sandbox is already
// fetching from.
//
// It is owned by the sandbox user, the identity the git-origins route serves it
// as: git http-backend runs as the repository's owner, for the client's push
// and the sandbox's fetch alike, so git's dubious-ownership check never trips.
//
// A branch the source names is HEAD from the start, so a clone made the moment
// the client's push lands resolves origin/HEAD (ensureOriginHead).
func (r *DockerSandboxRuntime) initGitOrigin(ctx context.Context, repoPath, branch string, user sandboxuser.User) error {
	if _, err := os.Stat(filepath.Join(repoPath, "HEAD")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return err
	}
	if err := runGit(ctx, "", -1, -1, "init", "--bare", repoPath); err != nil {
		return fmt.Errorf("initialize source origin repository: %w", err)
	}
	// Deletes are refused: nothing legitimate deletes a ref here, and it is the
	// one update whose effect the sandbox cannot see coming. Non-fast-forward is
	// allowed deliberately rather than by default — a client's branch moves by
	// rebase and amend, and refusing that would leave the sandbox with an origin
	// it could never catch up to (ADR 0058 §§1, 6).
	if err := runGit(ctx, repoPath, -1, -1, "config", "receive.denyDeletes", "true"); err != nil {
		return err
	}
	if err := runGit(ctx, repoPath, -1, -1, "config", "receive.denyNonFastForwards", "false"); err != nil {
		return err
	}
	if branch != "" {
		if err := runGit(ctx, repoPath, -1, -1, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
			return err
		}
	}
	return prepareOwnedTree(ctx, repoPath, chownID(user.UID), chownID(user.GID))
}

// gitHasCommits reports whether repo has a resolvable HEAD. A repository that
// was initialized for a push has none until the client delivers one.
func gitHasCommits(ctx context.Context, repo string, uid, gid int) bool {
	return runGit(ctx, repo, uid, gid, "rev-parse", "--verify", "--quiet", "HEAD") == nil
}

// sourceBaseCommit is the commit the create request pinned the source to,
// which the sandbox-agent's diff stat is measured against. Empty when the
// request recorded none.
func sourceBaseCommit(source workerapimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	return strings.TrimSpace(optString(checkout.Commit))
}

// sourceRefName and sourceRefType are the branch or tag the create request
// checked the source out at, empty when it named none.
func sourceRefName(source workerapimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	return strings.TrimSpace(optString(checkout.RefName))
}

func sourceRefType(source workerapimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(optString(checkout.RefType)))
}

// sourceWorkspace is a dirty workspace's snapshot, nil for a clean one. A
// dirty workspace missing either half is passed on as it is, for the clone
// to refuse, as restoreGitWorkspace does here.
func sourceWorkspace(source workerapimodel.GitSource) *sandboxconfig.SourceWorkspace {
	workspace, ok := source.Workspace.Get()
	if !ok || workspace.Mode.Or(workerclient.GitSourceWorkspaceModeClean) != workerclient.GitSourceWorkspaceModeDirty {
		return nil
	}
	return &sandboxconfig.SourceWorkspace{
		BaseCommit:  strings.TrimSpace(optString(workspace.BaseCommit)),
		SnapshotRef: strings.TrimSpace(optString(workspace.SnapshotRef)),
	}
}

// sourceUpstreamRef is the remote-tracking ref the source would fetch upstream
// into, derived from the branch it was cloned at — the same derivation
// `discobox diff` uses. Only a clone at a branch names one; anything else falls
// back to origin's default branch, which the sandbox-agent verifies in the
// repository before using.
func sourceUpstreamRef(source workerapimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return "refs/remotes/origin/HEAD"
	}
	refName := strings.TrimSpace(optString(checkout.RefName))
	if refName == "" || strings.TrimSpace(optString(checkout.RefType)) != "branch" {
		return "refs/remotes/origin/HEAD"
	}
	return "refs/remotes/origin/" + refName
}

// gitSourceInitialBranch returns the branch the client is expected to push, or
// empty when the source does not name one and git's default should stand.
func gitSourceInitialBranch(source workerapimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	if strings.TrimSpace(optString(checkout.RefType)) != "branch" {
		return ""
	}
	branch := strings.TrimSpace(optString(checkout.RefName))
	if branch == "" || strings.HasPrefix(branch, "-") {
		return ""
	}
	return branch
}

func hostMountedLocalDirectory(local, hostMountPrefix string) string {
	hostMountPrefix = cleanAbsPath(hostMountPrefix)
	if hostMountPrefix == "" {
		return local
	}
	local = strings.TrimSpace(local)
	if local == "" {
		return local
	}
	if strings.HasPrefix(local, "file://") {
		return local
	}
	if !filepath.IsAbs(local) {
		return local
	}
	local = cleanAbsPath(local)
	if local == "" || local == hostMountPrefix || strings.HasPrefix(local, hostMountPrefix+string(filepath.Separator)) {
		return local
	}
	return filepath.Join(hostMountPrefix, strings.TrimPrefix(local, string(filepath.Separator)))
}

// runGit and its variants run git against dir as uid/gid, or as the calling
// process's own identity when uid is negative. A repository's dubious-ownership
// check trips whenever the running process's identity doesn't match the
// directory's owner, so callers must pass whichever identity actually owns dir.
// They only ever run in repositories this pool owns — a pushed source's bare
// origin — and never in a sandbox's checkout (ADR 0126 §4).
func runGit(ctx context.Context, dir string, uid, gid int, args ...string) error {
	return runGitWithEnv(ctx, dir, uid, gid, nil, args...)
}

func runGitWithEnv(ctx context.Context, dir string, uid, gid int, env []string, args ...string) error {
	_, err := runGitOutputWithEnv(ctx, dir, uid, gid, nil, env, args...)
	return err
}

func runGitOutput(ctx context.Context, dir string, uid, gid int, stdin []byte, args ...string) ([]byte, error) {
	return runGitOutputWithEnv(ctx, dir, uid, gid, stdin, nil, args...)
}

func runGitOutputWithEnv(ctx context.Context, dir string, uid, gid int, stdin []byte, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.SysProcAttr = execidentity.SysProcAttr(uid, gid)
	// Through childproc, like every subprocess this agent runs: a git command
	// the pool agent's reaper collected would report no exit status at all, and
	// the caller would read that as the command having failed (ADR 0087).
	out, err := childproc.CombinedOutput(cmd)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// chownSpec renders the owner argument. An unset id is omitted rather than
// guessed at: "1000" leaves the group alone, and there is no owner to change at
// all when the uid is unset (ADR 0025 §4). The shell form cannot express -1,
// which the Lchown fallback takes directly.
func chownSpec(uid, gid int) string {
	switch {
	case uid == unsetID:
		return fmt.Sprintf(":%d", gid)
	case gid == unsetID:
		return fmt.Sprintf("%d", uid)
	default:
		return fmt.Sprintf("%d:%d", uid, gid)
	}
}

func chownRecursive(ctx context.Context, root string, uid, gid int) error {
	if uid == unsetID && gid == unsetID {
		// Nothing was given, so there is nothing to assert. Leaving ownership
		// alone is the honest answer; the sandbox sets it once it can resolve.
		return nil
	}
	if err := runChown(ctx, root, uid, gid); err == nil {
		return nil
	}
	return filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		//nolint:gosec // The tree is a pool-owned clone target; Lchown avoids following repository symlinks.
		return os.Lchown(p, uid, gid)
	})
}

func runChown(ctx context.Context, root string, uid, gid int) error {
	//nolint:gosec // root is a pool-owned source volume path and args are passed without a shell.
	cmd := exec.CommandContext(ctx, "chown", "-R", "--no-dereference", chownSpec(uid, gid), root)
	out, err := childproc.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("chown %s: %w: %s", root, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func optSandboxConfigEnv(opt workerclient.OptSandboxConfigEnv) workerclient.SandboxConfigEnv {
	v, _ := opt.Get()
	return v
}

func envList(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func envWithSandboxUser(values map[string]string, user sandboxuser.User) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		out[key] = value
	}
	if user.UID != nil {
		out["DISCOBOX_USER_UID"] = fmt.Sprintf("%d", *user.UID)
	}
	if user.GID != nil {
		out["DISCOBOX_USER_GID"] = fmt.Sprintf("%d", *user.GID)
	}
	if user.GroupName != "" {
		out["DISCOBOX_USER_GROUP"] = user.GroupName
	}
	if user.Name != "" {
		out["DISCOBOX_USER_NAME"] = user.Name
	}
	if user.HomeDirectory != "" {
		out["DISCOBOX_USER_HOME"] = user.HomeDirectory
	}
	if _, ok := out["HOME"]; !ok && user.HomeDirectory != "" {
		out["HOME"] = user.HomeDirectory
	}
	if _, ok := out["USER"]; !ok && user.Name != "" {
		out["USER"] = user.Name
	}
	return out
}

func envMap(values []string) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok {
			out[key] = val
		}
	}
	return out
}

// mergeEnv returns a new map containing base overlaid with overlay.
func mergeEnv(base, overlay map[string]string) map[string]string {
	out := copyMap(base)
	for key, value := range overlay {
		out[key] = value
	}
	return out
}

func copyMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneSandbox(sb *Sandbox) *Sandbox {
	if sb == nil {
		return nil
	}
	clone := *sb
	clone.Metadata = copyMap(sb.Metadata)
	clone.Env = copyMap(sb.Env)
	clone.Ports = append([]AssignedPort(nil), sb.Ports...)
	return &clone
}

// validateCreateRequest refuses a create the control plane did not fully
// resolve. The pool agent runs what it is told and invents nothing: it
// substitutes no image for a missing one, because a stand-in image cannot host
// a sandbox agent, and a container that can never answer says less than a
// failure naming the request that was wrong. Every sandbox carries a harness
// config (ADR 0032), so a request without one is a control plane that failed
// to resolve it, not a sandbox asking for a bare shell.
func validateCreateRequest(sandboxID string, req *workerapimodel.PoolSandboxCreateRequest) error {
	if strings.TrimSpace(optString(req.Config.Image)) == "" {
		return fmt.Errorf("sandbox %s: create request has no image", sandboxID)
	}
	if _, ok := req.ResolvedHarnessConfig.Get(); !ok {
		return fmt.Errorf("sandbox %s: create request has no resolved harness config", sandboxID)
	}
	return nil
}
