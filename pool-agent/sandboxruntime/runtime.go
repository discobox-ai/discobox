package sandboxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/layout"
	"github.com/obot-platform/discobox/sandboxconfig"
	"github.com/obot-platform/discobox/sandboxuser"

	"github.com/obot-platform/discobox/pool-agent/execidentity"
	"github.com/obot-platform/discobox/pool-agent/imagereap"
	"github.com/obot-platform/discobox/pool-agent/internalhttp"
	"github.com/obot-platform/discobox/pool-agent/proxyagent"

	workerclient "github.com/obot-platform/discobox/pool-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/pool-agent/api/model"
)

const (
	// SandboxAgentPort is exported so pool-agent's standing status-poll loop
	// (a package-external caller) can reach a sandbox-agent's HTTP API via
	// HTTPBaseURL without duplicating this value.
	SandboxAgentPort         = 3003
	sandboxAgentReadyTimeout = 30 * time.Second
	sandboxAgentPollInterval = 100 * time.Millisecond

	// The pool host provisions four host-backed roots and mounts them at these
	// fixed container paths. The sandbox-agent (running as PID 1) wires
	// everything else from these primary volumes; see ADR 0007.
	sandboxDataMount    = "/.discobox/data"
	sandboxCacheMount   = "/.discobox/cache"
	sandboxConfigMount  = "/.discobox/config"
	sandboxSourcesMount = "/.discobox/sources"

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

	// sandboxOriginsMount is where a clone-delivered local source's real host
	// origin directory is bound, read-only, at /.discobox/origins/<slug> (ADR
	// 0026). Unlike the primary roots above, this is not one pool-provisioned
	// volume: each eligible source gets its own independent bind straight from
	// an arbitrary external host directory, built in prepareSandboxVolumes.
	sandboxOriginsMount = "/.discobox/origins"

	sandboxLabelManaged = "discobox.sandbox.managed"
	sandboxLabelProject = "discobox.project_id"
	sandboxLabelPool    = "discobox.pool_id"
	sandboxLabelSandbox = "discobox.sandbox_id"
	// sandboxLabelSpec records the spec fingerprint the container was built
	// from (ADR 0017 §5). Comparing it is how the runtime decides drift: one
	// check covers the image pin, resources, sources, and anything added to the
	// spec later, because the control plane hashes the whole manifest.
	sandboxLabelSpec         = "discobox.spec_fingerprint"
	sandboxManifestPublicKey = "controlPlane"
)

var (
	ErrNotFound      = errors.New("sandbox not found")
	ErrAlreadyExists = errors.New("sandbox already exists")
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

// GitRepositoryLocation is the on-host location of a sandbox's git repository,
// together with the OS identity that owns it. The git CGI backend must run as
// this identity, not as the pool-agent process's own identity, or it trips
// git's dubious-ownership check against the sandbox user's checked-out worktree.
type GitRepositoryLocation struct {
	Path string
	// UID and GID are the owning user. A negative value means the caller
	// should not attempt to change identity — used by the in-memory runtime,
	// whose repository paths are simply owned by whichever user is running
	// the process (there is no sandbox container user to impersonate).
	UID int
	GID int
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
	ListSandboxes(ctx context.Context) ([]*Sandbox, error)
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
	// SyncKnownPools reaps the agent-created footprint (sandbox containers and
	// host data/proxy subtrees) of any pool on this shared host daemon whose ID
	// is not in knownPoolIDs. It is how a shared-daemon (local docker) setup
	// reclaims whole orphaned pools; the caller is the control plane, which owns
	// the authoritative pool set.
	SyncKnownPools(ctx context.Context, knownPoolIDs []string) error
	// Power operations instruct and report acceptance only; the resulting state
	// is published by the state reporter (ADR 0017 §§9-10, see power.go).
	StartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error
	StopSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error
	RestartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest) error
	// EnsureSandboxRunning starts a stopped sandbox on demand, for the
	// sandbox-directed routes (ADR 0017 §12).
	EnsureSandboxRunning(ctx context.Context, sandboxID string) error
	GitRepositoryPath(ctx context.Context, sandboxID, repositoryID string) (GitRepositoryLocation, error)
	HTTPBaseURL(ctx context.Context, sandboxID string, port int) (*url.URL, error)
}

// DockerSandboxRuntime launches sandboxes as Docker containers inside a pool.
type DockerSandboxRuntime struct {
	client                *client.Client
	projectID             string
	poolID                string
	controlPlanePublicKey string
	hostMountPrefix       string
	// hostState translates a container path into the daemon's view of it. It is
	// applied only where a path is handed to the daemon.
	hostState layout.HostMapping
	// powerLocks serializes power operations per sandbox (see power.go).
	powerLocks sync.Map
	// statePublisher is the state channel's sink while a watcher is running
	// (see statereport.go). Power operations use it to announce a transition
	// they are about to make.
	statePublisher atomic.Value
	// progressPublisher is the same channel's sink for provisioning progress
	// that has no state transition to hang off — an image pull, above all
	// (see statereport.go, ADR 0039).
	progressPublisher atomic.Value
}

type DockerSandboxRuntimeConfig struct {
	ProjectID             string
	PoolID                string
	ControlPlanePublicKey string
	HostMountPrefix       string
	// HostStateRoot is where this pool's Docker daemon sees
	// layout.ContainerRoot. Empty means no relocation.
	HostStateRoot string
}

func NewDockerSandboxRuntime(cfg DockerSandboxRuntimeConfig) (*DockerSandboxRuntime, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &DockerSandboxRuntime{
		client:                cli,
		projectID:             cfg.ProjectID,
		poolID:                cfg.PoolID,
		controlPlanePublicKey: cfg.ControlPlanePublicKey,
		hostMountPrefix:       cleanAbsPath(cfg.HostMountPrefix),
		hostState:             layout.NewHostMapping(cfg.HostStateRoot),
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
	// Replacing a container is a power operation on this sandbox as much as a
	// start or a stop is, and it reads the power state it is preserving
	// (ADR 0021 §3). Taking the same per-sandbox lock is what stops an auto-start
	// (ADR 0017 §12) from racing the replacement — starting the container being
	// removed, or finding none at all.
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	// A container this create replaces takes its power state with it: an upgrade
	// restarts a running sandbox into the new image and leaves a stopped one
	// stopped (ADR 0021 §3).
	replacedRunning := false
	if existing, err := r.GetSandbox(ctx, sandboxID); err == nil {
		drifted, err := r.containerSpecDrifted(ctx, existing, req)
		if err != nil {
			return nil, err
		}
		if !drifted {
			// The container already exists, but a push-delivered source is only
			// materialized once the client has pushed, which necessarily happens
			// after the container was created and parked. This create is that
			// resume, so finish those sources rather than returning a sandbox whose
			// workspace is still empty.
			if err := r.materializePushedSources(ctx, sandboxID, req); err != nil {
				return nil, err
			}
			return existing, nil
		}
		// The control plane changed this sandbox's spec — an image upgrade
		// (ADR 0021 §1) or any other manifest edit. Remove the container and
		// fall through to build a new one; the sandbox's state lives in the
		// pool-host binds prepared below, not in the container, so it survives.
		replacedRunning = existing.Status == StatusRunning
		slog.InfoContext(ctx, "replacing sandbox container for a spec change",
			"sandboxId", sandboxID,
			"imageDigest", strings.TrimSpace(optString(req.Config.ImageDigest)),
			"specFingerprint", strings.TrimSpace(optString(req.Config.SpecFingerprint)),
			"running", replacedRunning)
		if replacedRunning {
			// Stop it the way a stop would, so the sandbox-agent tears its execs
			// down and flushes their logs instead of being killed outright.
			r.PublishSandboxState(ctx, sandboxID, StateStopping)
			timeout := sandboxStopTimeoutSeconds
			if _, err := r.client.ContainerStop(ctx, existing.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
				return nil, fmt.Errorf("stop sandbox container for a spec change: %w", err)
			}
		}
		if _, err := r.client.ContainerRemove(ctx, existing.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("remove sandbox container for a spec change: %w", err)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	normalizeSandboxConfig(&req.Config)
	config := req.Config
	imageName := strings.TrimSpace(optString(config.Image))
	imageName, err := r.resolveSandboxImage(ctx, sandboxID, imageName, strings.TrimSpace(optString(config.ImageDigest)))
	if err != nil {
		return nil, err
	}
	user := resolveSandboxUser(req)
	mounts, project, err := r.prepareSandboxVolumes(ctx, sandboxID, req, user)
	if err != nil {
		return nil, err
	}
	proxyMaterial, err := proxyagent.EnsureSandboxMaterial(r.projectID, r.poolID, sandboxID)
	if err != nil {
		return nil, err
	}
	sentinels, _ := req.Sentinels.Get()
	if err := proxyagent.UpsertSandboxSentinels(r.projectID, r.poolID, sandboxID, sentinels); err != nil {
		return nil, err
	}
	// Nest the proxy material inside the config volume at /.discobox/config/proxy
	// so it rides along when the sandbox-agent recursively rebinds the config
	// volume onto /etc/discobox; the in-sandbox path stays proxyagent.SandboxProxyMount.
	mounts = append(mounts, mount.Mount{
		Type:     mount.TypeBind,
		Source:   r.daemonPath(proxyMaterial.MountSource),
		Target:   filepath.Join(sandboxConfigMount, "proxy"),
		ReadOnly: true,
	})
	if err := r.writeSandboxHarnessConfig(ctx, sandboxID, imageName, req, proxyMaterial.Env, project); err != nil {
		return nil, err
	}
	if secretEnv, ok := req.SecretEnv.Get(); ok {
		if err := r.writeSandboxSecrets(ctx, sandboxID, secretEnv); err != nil {
			return nil, err
		}
	}
	baseEnv := mergeEnv(map[string]string(optSandboxConfigEnv(config.Env)), proxyMaterial.Env)
	name := sandboxContainerName(r.poolID, sandboxID)
	cfg := &container.Config{
		Image:        imageName,
		Labels:       r.labels(sandboxID, strings.TrimSpace(optString(config.SpecFingerprint))),
		Env:          envList(envWithSandboxUser(baseEnv, user)),
		WorkingDir:   sourceWorkingDirectory(req),
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
	}
	hostCfg := &container.HostConfig{
		Mounts:     mounts,
		Privileged: true,
	}
	// No CPU/memory limit is set here: a sandbox container shares its worker
	// container's cgroup rather than reserving a nested slice of it
	// (docs/adr/0025).
	// Attach the sandbox to the per-pool internal network only: it reaches the
	// pool proxy (resolved as discobox-pool-proxy via Docker embedded DNS)
	// and DNS, but has no route off-box, so all egress is forced through the proxy.
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			proxyagent.SandboxNetworkName(r.poolID): {},
		},
	}
	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: name})
	if err != nil {
		return nil, err
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
		return r.observedSandbox(ctx, sandboxID)
	}
	r.PublishSandboxState(ctx, sandboxID, StateStarting)
	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	if err := r.waitForSandboxAgent(ctx, sandboxID); err != nil {
		return nil, err
	}
	return r.observedSandbox(ctx, sandboxID)
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
	r.PublishSandboxState(ctx, sandboxID, stateFromStatus(sb.Status))
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
	if !imageMatchesPin(inspected.ID, pinnedDigest) {
		return "", fmt.Errorf(
			"sandbox is pinned to image %s but %q now resolves to %s, and the pinned image is not available on this pool; upgrade the sandbox to move it to the current image",
			pinnedDigest, imageName, inspected.ID)
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
	pinned := strings.TrimSpace(pinnedDigest)
	return pinned == "" || strings.TrimSpace(imageID) == pinned
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
	fingerprint := strings.TrimSpace(optString(req.Config.SpecFingerprint))
	if fingerprint == "" {
		return false, nil
	}
	inspect, err := r.client.ContainerInspect(ctx, existing.ID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
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
		return fmt.Errorf("pull image %q: %w", imageName, err)
	}
	defer pull.Close()
	// JSONMessages rather than Wait: Wait drains the daemon's progress stream
	// and discards it, which is exactly the information a waiting client needs.
	// Draining is what runs the pull, so this replaces Wait rather than adding
	// to it.
	err = consumePullProgress(ctx, pull.JSONMessages(ctx), imageName, func(progress PullProgress) {
		r.PublishSandboxPullProgress(ctx, sandboxID, progress)
	}, nil)
	if err != nil {
		return fmt.Errorf("pull image %q: %w", imageName, err)
	}
	return nil
}

// materializePushedSources completes the push-delivered sources of a sandbox
// that already exists, checking out the commit the client pushed and restoring
// its workspace.
//
// Only push-delivered sources are touched: a clone-delivered source was fully
// materialized when the sandbox was created, so there is nothing here to
// finish.
//
// Materialization is idempotent, so a repeat create that has nothing new to
// deliver is a no-op; once a source has actually been finished, a marker (see
// gitMaterializedMarkerName) makes every later create a true no-op too, so a
// stray duplicate call can't reset/clean a workspace the sandbox has been
// using since.
func (r *DockerSandboxRuntime) materializePushedSources(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxCreateRequest) error {
	if req == nil {
		return nil
	}
	normalizeSandboxConfig(&req.Config)
	user := resolveSandboxUser(req)
	for _, source := range sandboxSources(req) {
		if !gitSourceAwaitsPush(source.git) {
			continue
		}
		sourcePoolPath := r.sandboxSourcePath(sandboxID, source.slug)
		if err := r.materializeGitSource(ctx, source.git, sourcePoolPath, source.slug, user); err != nil {
			return fmt.Errorf("materialize pushed source %q: %w", source.slug, err)
		}
	}
	return nil
}

// prepareSandboxVolumes provisions the four host-backed roots and returns their
// container mounts. The pool host no longer decides in-sandbox paths (home,
// /var/lib/docker, sources targets); it only supplies the primary volumes. The
// sandbox-agent wires everything else from the image's declarative volume list
// and the manifest's source list (ADR 0007).
// prepareSandboxVolumes also returns the primary source's ProjectLayer
// contribution (nil if the source has no .discobox/project.json), read once
// here at clone time — never inside the running sandbox (ADR 0012 §7).
func (r *DockerSandboxRuntime) prepareSandboxVolumes(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxCreateRequest, user sandboxuser.User) ([]mount.Mount, *sandboxconfig.ProjectLayer, error) {
	// Creating a container against this tree is what unarchiving is: the tree is
	// reused as it stands, and clearing the marker is the whole of what makes it
	// a live sandbox again (ADR 0022 §6). Clearing it first means a create that
	// fails part way leaves the tree unmarked and container-less, which the
	// reaper handles as the ordinary failed-create case.
	if err := clearSandboxArchiveMarker(r.sandboxRoot(sandboxID)); err != nil {
		return nil, nil, fmt.Errorf("clear sandbox archive marker: %w", err)
	}
	dataHostPath := r.sandboxDataRootPath(sandboxID)
	if err := prepareOwnedDirectory(ctx, dataHostPath, 0, 0); err != nil {
		return nil, nil, fmt.Errorf("prepare sandbox data volume: %w", err)
	}
	cacheHostPath := r.poolCacheRoot()
	if err := prepareOwnedDirectory(ctx, cacheHostPath, 0, 0); err != nil {
		return nil, nil, fmt.Errorf("prepare pool cache volume: %w", err)
	}
	configHostPath := r.sandboxConfigRoot(sandboxID)
	if err := prepareOwnedDirectory(ctx, configHostPath, 0, 0); err != nil {
		return nil, nil, fmt.Errorf("prepare sandbox config volume: %w", err)
	}
	sourcesHostPath := r.sandboxSourcesRoot(sandboxID)
	if err := prepareOwnedDirectory(ctx, sourcesHostPath, 0, 0); err != nil {
		return nil, nil, fmt.Errorf("prepare sandbox sources volume: %w", err)
	}
	secretsHostPath := r.sandboxSecretsRoot(sandboxID)
	if err := prepareOwnedDirectory(ctx, secretsHostPath, 0, 0); err != nil {
		return nil, nil, fmt.Errorf("prepare sandbox secrets volume: %w", err)
	}
	var project *sandboxconfig.ProjectLayer
	sources := sandboxSources(req)
	_, hasPrimary := req.Config.Source.Get()
	for i, source := range sources {
		sourcePoolPath := r.sandboxSourcePath(sandboxID, source.slug)
		if err := r.materializeGitSource(ctx, source.git, sourcePoolPath, source.slug, user); err != nil {
			return nil, nil, fmt.Errorf("materialize source %q: %w", source.slug, err)
		}
		if err := prepareOwnedDirectory(ctx, sourcePoolPath, chownID(user.UID), chownID(user.GID)); err != nil {
			return nil, nil, fmt.Errorf("set source ownership %q: %w", source.slug, err)
		}
		// The primary source is always first when present (sandboxSources).
		if i == 0 && hasPrimary {
			layer, err := readProjectLayer(sourcePoolPath)
			if err != nil {
				return nil, nil, fmt.Errorf("read project layer %q: %w", source.slug, err)
			}
			project = layer
		}
	}
	// These sources are resolved by the Docker daemon, so they are the only
	// place a container path has to become a daemon path. A local source bind
	// is already a daemon path and passes through untouched — including each
	// origin mount's raw host directory below.
	mounts := []mount.Mount{
		{Type: mount.TypeBind, Source: r.daemonPath(dataHostPath), Target: sandboxDataMount},
		{Type: mount.TypeBind, Source: r.daemonPath(cacheHostPath), Target: sandboxCacheMount},
		{Type: mount.TypeBind, Source: r.daemonPath(configHostPath), Target: sandboxConfigMount, ReadOnly: true},
		{Type: mount.TypeBind, Source: r.daemonPath(sourcesHostPath), Target: sandboxSourcesMount},
		{Type: mount.TypeBind, Source: r.daemonPath(secretsHostPath), Target: sandboxSecretsMount},
	}
	mounts = append(mounts, originMounts(sources, r.daemonPath)...)
	return mounts, project, nil
}

// originMounts builds one read-only bind per clone-delivered local source,
// each pointing straight at its real host directory rather than any
// pool-owned volume tree — not a copy, and not a sub-path of any pool-owned
// volume, unlike /.discobox/sources (ADR 0026). Push-delivered sources have no
// on-disk origin reachable from this host, which is precisely why push was
// chosen for them, so they are skipped. Pure and side-effect free, unlike the
// rest of prepareSandboxVolumes, so it is testable without the root privilege
// the primary volumes' ownership chowns require.
func originMounts(sources []sandboxSource, daemonPath func(string) string) []mount.Mount {
	var mounts []mount.Mount
	for _, source := range sources {
		local := strings.TrimSpace(optString(source.git.LocalDirectory))
		if gitSourceAwaitsPush(source.git) || local == "" {
			continue
		}
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   daemonPath(local),
			Target:   path.Join(sandboxOriginsMount, source.slug),
			ReadOnly: true,
		})
	}
	return mounts
}

// writeSandboxSecrets atomically writes the sandbox's secret-bound
// envName->sentinel map to its secrets volume, root-owned and mode 0600 so
// only sandbox-agent (running as root) can read it; the harness process
// (unprivileged) never gets filesystem access, only the env sandbox-agent
// injects at exec time (ADR 0012 §3).
func (r *DockerSandboxRuntime) writeSandboxSecrets(ctx context.Context, sandboxID string, secretEnv map[string]string) error {
	dir := r.sandboxSecretsRoot(sandboxID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(secretEnv, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "secrets.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return chownRecursive(ctx, dir, 0, 0)
}

// readProjectLayer reads .discobox/project.json from a materialized source's
// root, if present. A missing file is not an error: the project layer is
// optional (ADR 0012 §7).
func readProjectLayer(sourceDir string) (*sandboxconfig.ProjectLayer, error) {
	data, err := os.ReadFile(filepath.Join(sourceDir, ".discobox", "project.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var layer sandboxconfig.ProjectLayer
	if err := json.Unmarshal(data, &layer); err != nil {
		return nil, fmt.Errorf("parse .discobox/project.json: %w", err)
	}
	return &layer, nil
}

func (r *DockerSandboxRuntime) writeSandboxHarnessConfig(ctx context.Context, sandboxID, resolvedImage string, req *workerapimodel.PoolSandboxCreateRequest, proxyEnv map[string]string, project *sandboxconfig.ProjectLayer) error {
	configDir := r.sandboxConfigRoot(sandboxID)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	// The proxy material is bind-mounted at /.discobox/config/proxy, nested under
	// the config volume. Pre-create the mountpoint here so the container runtime
	// does not have to create it inside the read-only parent.
	if err := os.MkdirAll(filepath.Join(configDir, "proxy"), 0o755); err != nil {
		return err
	}
	doc := buildSandboxDocument(r.projectID, sandboxID, r.poolID, r.controlPlanePublicKey, resolvedImage, req, proxyEnv, project)
	data, err := marshalSandboxDocument(doc)
	if err != nil {
		return err
	}
	path := filepath.Join(configDir, "sandbox.json")
	if err := writeSandboxManifest(path, data); err != nil {
		return err
	}
	return chownRecursive(ctx, configDir, 0, 0)
}

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
			Path:   v.Path,
			Volume: harness.VolumeKind(v.Volume),
			UID:    harness.ScalarToken(optString(v.UID)),
			GID:    harness.ScalarToken(optString(v.Gid)),
			Mode:   optString(v.Mode),
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
func buildSandboxDocument(projectID, sandboxID, poolID, controlPlanePublicKey, resolvedImage string, req *workerapimodel.PoolSandboxCreateRequest, proxyEnv map[string]string, project *sandboxconfig.ProjectLayer) sandboxconfig.Document {
	doc := sandboxconfig.Document{
		Runtime: sandboxconfig.RuntimeLayer{
			SandboxID: sandboxID,
			Image:     resolvedImage,
			Provider: sandboxconfig.Provider{
				Kind:      "discobox-pool",
				ProjectID: projectID,
				PoolID:    poolID,
				PublicKeys: map[string]string{
					sandboxManifestPublicKey: controlPlanePublicKey,
				},
			},
			AgentRuntime: sandboxconfig.AgentRuntime{
				ListenAddress:          fmt.Sprintf(":%d", SandboxAgentPort),
				WorkingRoot:            "/workspace",
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
		// The pool owns the effective sandbox user used for the home mount and
		// container environment. Publish that fully resolved identity even when
		// the request omitted or partially specified config.user, so the
		// sandbox-agent installs harness files and launches commands against the
		// same home directory.
		user := resolveSandboxUser(req)
		doc.Runtime.User = user
		// The sandbox-agent bind-mounts each worker-materialized source from
		// /.discobox/sources/<slug> onto its target as this same user (ADR 0007).
		for _, source := range sandboxSources(req) {
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

func prepareOwnedDirectory(ctx context.Context, dir string, uid, gid int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	return chownRecursive(ctx, dir, uid, gid)
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
			if err := proxyagent.UpsertSandboxSentinels(r.projectID, r.poolID, sandboxID, sentinels); err != nil {
				return nil, err
			}
		}
		if secretEnv, ok := req.SecretEnv.Get(); ok {
			// Refresh the sandbox-agent-side secrets file so newly bound secrets
			// (or a rotated sentinel) resolve without a restart.
			if err := r.writeSandboxSecrets(ctx, sandboxID, secretEnv); err != nil {
				return nil, err
			}
		}
	}
	return r.GetSandbox(ctx, sandboxID)
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
	if err := proxyagent.RemoveSandboxSentinels(r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	if err := proxyagent.RemoveSandboxMaterial(r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	if err := os.RemoveAll(r.sandboxRoot(sandboxID)); err != nil {
		return fmt.Errorf("remove sandbox data for %s: %w", sandboxID, err)
	}
	return nil
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
	// sandboxVolumeRetention is how long a dead sandbox's persistent volume tree
	// (data/config/sources/secrets) is kept before reclamation, so its data survives an
	// accidental or transient removal and a same-day recreate.
	//
	// This is accident recovery only, and covers exactly the cases that never
	// run through DeleteSandbox: a container removed out of band, or lost while
	// the agent was down. Deliberate retention is archiving, whose window is a
	// control-plane policy the agent does not know (ADR 0022 §4) — archived
	// trees are skipped here, not timed out here.
	sandboxVolumeRetention = 24 * time.Hour
	// sandboxVolumeTombstone records, inside a sandbox's volume tree, when the
	// sandbox was first observed dead. The tree is reaped once the tombstone
	// predates sandboxVolumeRetention.
	sandboxVolumeTombstone = ".discobox-orphaned-at"
)

// ReconcileProxyMaterial prunes proxy material for sandboxes whose containers no
// longer exist. It is the recovery path for containers deleted out of band or
// while the pool was down, which never run through DeleteSandbox.
func (r *DockerSandboxRuntime) ReconcileProxyMaterial(ctx context.Context, minAge time.Duration) error {
	live, err := r.liveSandboxIDs(ctx)
	if err != nil {
		return err
	}
	return proxyagent.PruneOrphanedMaterial(r.projectID, r.poolID, live, minAge)
}

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
// It no longer reports anything to the control plane: a sandbox whose container
// is gone is an observation, and observations travel on the state channel
// (statereport.go). This is now only about reclaiming disk.
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

func (r *DockerSandboxRuntime) reconcileProxyMaterial(ctx context.Context, logger *slog.Logger) {
	if err := r.ReconcileProxyMaterial(ctx, proxyMaterialGracePeriod); err != nil {
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
	orphans, err := proxyagent.OrphanedSandboxIDs(r.projectID, r.poolID, live, minAge)
	if err != nil {
		logger.Warn("scan orphaned sandbox material", "error", err)
	}
	for _, sandboxID := range orphans {
		if err := proxyagent.RemoveSandboxSentinels(r.projectID, r.poolID, sandboxID); err != nil {
			logger.Warn("remove sandbox proxy sentinels", "sandboxID", sandboxID, "error", err)
		}
		if err := proxyagent.RemoveSandboxMaterial(r.projectID, r.poolID, sandboxID); err != nil {
			logger.Warn("remove sandbox proxy material", "sandboxID", sandboxID, "error", err)
		}
	}
}

// reconcileSandboxVolumes reaps the persistent volume trees
// (pools/{poolID}/sandboxes/{sandboxID}/{data,config,sources}) of this pool's
// dead sandboxes. It is scoped to this pool's own subtree and this pool's live
// containers, so pools sharing a host never reap each other's data.
//
// Each dead tree is kept for retention after it is first observed dead (a
// tombstone starts the clock), so persistent data survives an accidental or
// out-of-band removal and a same-day recreate. A tree whose sandbox is live
// again has its tombstone cleared.
func (r *DockerSandboxRuntime) reconcileSandboxVolumes(ctx context.Context, logger *slog.Logger, retention time.Duration) {
	live, err := r.liveSandboxIDs(ctx)
	if err != nil {
		logger.Warn("list sandbox containers for volume reconcile", "error", err)
		return
	}
	liveSet := make(map[string]struct{}, len(live))
	for _, id := range live {
		liveSet[id] = struct{}{}
	}
	reapDeadSandboxVolumes(r.sandboxesRoot(), liveSet, retention, time.Now(), logger)
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
// is kept for the retention window (via a tombstone) like the sandbox reaper.
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
		proxyagent.PoolsRoot(r.projectID),
		known, sandboxVolumeRetention, time.Now(), logger,
	)
	return nil
}

// reapUnknownPools reclaims the data, cache, and proxy subtrees of pools not in the
// known set. Data subtrees hold persistent sandbox data, so they get the same
// tombstone-based retention as the sandbox reaper; cache and proxy material
// are regenerable, so their subtrees are reaped once no retained data subtree
// remains.
func reapUnknownPools(dataPoolsRoot, cachePoolsRoot, proxyPoolsRoot string, known map[string]struct{}, retention time.Duration, now time.Time, logger *slog.Logger) {
	for _, poolID := range unknownPoolDirs(dataPoolsRoot, known, logger) {
		dir := filepath.Join(dataPoolsRoot, poolID)
		tombstone := filepath.Join(dir, sandboxVolumeTombstone)
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

// reapDeadSandboxVolumes is the pool-scoped core of the volume reaper: root is
// this pool's own sandboxes directory, liveSet is this pool's live containers,
// so it only ever tombstones and reaps this pool's data. A dead tree is kept
// for retention after it is first observed dead.
func reapDeadSandboxVolumes(root string, liveSet map[string]struct{}, retention time.Duration, now time.Time, logger *slog.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("scan sandbox volume root", "root", root, "error", err)
		}
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sandboxID := entry.Name()
		dir := filepath.Join(root, sandboxID)
		if sandboxIsArchived(dir) {
			// Held by intent, not orphaned. The control plane decides when an
			// archived tree expires, and purges it through DeleteSandbox.
			continue
		}
		tombstone := filepath.Join(dir, sandboxVolumeTombstone)
		if _, ok := liveSet[sandboxID]; ok {
			// The sandbox is alive (or came back): clear any stale tombstone.
			_ = os.Remove(tombstone)
			continue
		}
		diedAt, ok := readSandboxTombstone(tombstone)
		if !ok {
			// First time seen dead: start the retention clock.
			writeSandboxTombstone(tombstone, now, logger)
			continue
		}
		if now.Sub(diedAt) < retention {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			logger.Warn("reap dead sandbox volume", "sandboxID", sandboxID, "error", err)
			continue
		}
		logger.Info("reaped dead sandbox volume", "sandboxID", sandboxID, "deadFor", now.Sub(diedAt).Truncate(time.Minute).String())
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
	r.reconcileSandboxVolumes(ctx, logger, sandboxVolumeRetention)

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
			r.reconcileSandboxVolumes(ctx, logger, sandboxVolumeRetention)
		}
	}
}

func (r *DockerSandboxRuntime) GitRepositoryPath(ctx context.Context, sandboxID, repositoryID string) (GitRepositoryLocation, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return GitRepositoryLocation{}, err
	}
	repoPath := r.sandboxSourcePath(sandboxID, repositoryID)
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		if os.IsNotExist(err) {
			return GitRepositoryLocation{}, ErrNotFound
		}
		return GitRepositoryLocation{}, err
	}
	uid, gid := sandboxUserFromEnv(sb.Env)
	return GitRepositoryLocation{Path: repoPath, UID: uid, GID: gid}, nil
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

func (r *DockerSandboxRuntime) HTTPBaseURL(ctx context.Context, sandboxID string, port int) (*url.URL, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid sandbox HTTP port %d", port)
	}
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	inspect, err := r.client.ContainerInspect(ctx, sb.ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	ip := containerIPAddress(inspect.Container)
	if ip == "" {
		return nil, fmt.Errorf("sandbox %q does not have an inspectable IP address", sandboxID)
	}
	return &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", ip, port)}, nil
}

func (r *DockerSandboxRuntime) waitForSandboxAgent(ctx context.Context, sandboxID string) error {
	ctx, cancel := context.WithTimeout(ctx, sandboxAgentReadyTimeout)
	defer cancel()
	var lastErr error
	for {
		sb, err := r.GetSandbox(ctx, sandboxID)
		if err != nil {
			lastErr = err
		} else {
			if err := sandboxAgentTerminalStateError(sb); err != nil {
				return err
			}
			base, err := r.HTTPBaseURL(ctx, sandboxID, SandboxAgentPort)
			if err == nil {
				healthURL := *base
				healthURL.Path = "/healthz"
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, healthURL.String(), nil)
				if reqErr != nil {
					return reqErr
				}
				// Not http.DefaultClient: it honors HTTP_PROXY, and a pool
				// running inside a sandbox has proxy env injected for its
				// egress. This request stays on the pool's own network.
				resp, reqErr := internalhttp.Client.Do(req)
				if reqErr == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						return nil
					}
					lastErr = fmt.Errorf("sandbox-agent health returned %s", resp.Status)
				} else {
					lastErr = reqErr
				}
			} else {
				lastErr = err
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

func (r *DockerSandboxRuntime) labels(sandboxID, specFingerprint string) map[string]string {
	labels := map[string]string{
		sandboxLabelManaged: "true",
		sandboxLabelProject: r.projectID,
		sandboxLabelPool:    r.poolID,
		sandboxLabelSandbox: sandboxID,
	}
	if specFingerprint != "" {
		labels[sandboxLabelSpec] = specFingerprint
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
	archived        map[string]struct{}
	gitRepositories map[string]map[string]string
}

func NewMemorySandboxRuntime() *MemorySandboxRuntime {
	return &MemorySandboxRuntime{
		sandboxes:       map[string]*Sandbox{},
		archived:        map[string]struct{}{},
		gitRepositories: map[string]map[string]string{},
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
	return nil
}

func (r *MemorySandboxRuntime) SyncKnownPools(context.Context, []string) error {
	return nil
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

func (r *MemorySandboxRuntime) EnsureSandboxRunning(ctx context.Context, sandboxID string) error {
	return r.StartSandbox(ctx, sandboxID, nil)
}

func (r *MemorySandboxRuntime) GitRepositoryPath(_ context.Context, sandboxID, repositoryID string) (GitRepositoryLocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sandboxes[sandboxID] == nil {
		return GitRepositoryLocation{}, ErrNotFound
	}
	repositories := r.gitRepositories[sandboxID]
	if repositories == nil || repositories[repositoryID] == "" {
		return GitRepositoryLocation{}, ErrNotFound
	}
	return GitRepositoryLocation{Path: repositories[repositoryID], UID: -1, GID: -1}, nil
}

func (r *MemorySandboxRuntime) HTTPBaseURL(context.Context, string, int) (*url.URL, error) {
	return nil, ErrNotFound
}

func (r *MemorySandboxRuntime) SetGitRepositoryPath(sandboxID, repositoryID, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gitRepositories[sandboxID] == nil {
		r.gitRepositories[sandboxID] = map[string]string{}
	}
	r.gitRepositories[sandboxID][repositoryID] = path
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

// poolCacheRoot is the shared cache directory for every sandbox this pool runs
// in this project (ADRs 0007 and 0013). Its independent top-level root lets a
// provider mount disposable storage at /var/lib/discobox/cache without moving
// durable sandbox state.
func (r *DockerSandboxRuntime) poolCacheRoot() string {
	return resolve(layout.PoolCache(r.projectID, r.poolID))
}

// sandboxesRoot is the parent of every sandbox's per-sandbox volume tree for
// this pool. The volume reaper scans only under here, which is why it can never
// touch another pool's data.
func (r *DockerSandboxRuntime) sandboxesRoot() string {
	return resolve(layout.PoolSandboxes(r.projectID, r.poolID))
}

// poolsRoot is the parent of every pool's data subtree for this project on the
// shared host. The pool-sync reaper enumerates it to find orphaned pools.
func (r *DockerSandboxRuntime) poolsRoot() string {
	return resolve(layout.ProjectPools(r.projectID))
}

func (r *DockerSandboxRuntime) cachePoolsRoot() string {
	return resolve(layout.ProjectCachePools(r.projectID))
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
	return resolve(layout.SandboxData(r.projectID, r.poolID, sandboxID))
}

func (r *DockerSandboxRuntime) sandboxConfigRoot(sandboxID string) string {
	return resolve(layout.SandboxConfig(r.projectID, r.poolID, sandboxID))
}

func (r *DockerSandboxRuntime) sandboxSecretsRoot(sandboxID string) string {
	return resolve(layout.SandboxSecrets(r.projectID, r.poolID, sandboxID))
}

func (r *DockerSandboxRuntime) sandboxSourcesRoot(sandboxID string) string {
	return resolve(layout.SandboxSources(r.projectID, r.poolID, sandboxID))
}

func (r *DockerSandboxRuntime) sandboxSourcePath(sandboxID, slug string) string {
	return filepath.Join(r.sandboxSourcesRoot(sandboxID), slug)
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
func resolveSandboxUser(req *workerapimodel.PoolSandboxCreateRequest) sandboxuser.User {
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
		HomeDirectory:    cleanContainerPath(optString(user.HomeDirectory)),
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

func sourceWorkingDirectory(req *workerapimodel.PoolSandboxCreateRequest) string {
	if req == nil {
		return ""
	}
	source, ok := req.Config.Source.Get()
	if !ok {
		return ""
	}
	destination, ok := source.Destination.Get()
	if !ok {
		return ""
	}
	return optString(destination.WorkingDirectory)
}

// normalizeSandboxConfig applies provider-owned path defaults before the
// configuration is used for either bind mounts or the public sandbox manifest.
// This keeps manifest consumers on the documented SandboxConfig contract while
// ensuring they observe the paths the runtime actually mounted.
func normalizeSandboxConfig(config *workerapimodel.SandboxConfig) {
	if config == nil {
		return
	}
	source, ok := config.Source.Get()
	if !ok {
		return
	}
	destination, _ := source.Destination.Get()
	directory := cleanContainerPath(optString(destination.Directory))
	if directory == "" {
		directory = "/workspace"
	}
	destination.Directory = workerclient.NewOptString(directory)
	source.Destination = workerclient.NewOptGitSourceDestination(destination)
	config.Source = workerclient.NewOptGitSource(source)
}

type sandboxSource struct {
	slug   string
	target string
	git    workerapimodel.GitSource
}

func sandboxSources(req *workerapimodel.PoolSandboxCreateRequest) []sandboxSource {
	if req == nil {
		return nil
	}
	var out []sandboxSource
	used := map[string]struct{}{}
	if source, ok := req.Config.Source.Get(); ok {
		out = append(out, sandboxSourceFor("primary", source, "/workspace", used))
	}
	if refs, ok := req.Config.SourceCodeReferences.Get(); ok {
		keys := make([]string, 0, len(refs))
		for key := range refs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			source := refs[key]
			defaultTarget := cleanContainerPath(key)
			if defaultTarget == "" {
				defaultTarget = path.Join("/workspace", defaultSourceSlug(source, key))
			}
			out = append(out, sandboxSourceFor(key, source, defaultTarget, used))
		}
	}
	return out
}

func sandboxSourceFor(seed string, source workerapimodel.GitSource, defaultTarget string, used map[string]struct{}) sandboxSource {
	slug := sourceSlug(source, seed, used)
	target := defaultTarget
	if destination, ok := source.Destination.Get(); ok {
		if directory := cleanContainerPath(optString(destination.Directory)); directory != "" {
			target = directory
		}
	}
	if target == "" {
		target = path.Join("/workspace", slug)
	}
	return sandboxSource{slug: slug, target: target, git: source}
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

func cleanContainerPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return ""
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(value, "/"))
	if cleaned == "/" {
		return ""
	}
	return cleaned
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

// daemonPath converts a container path into the path this pool's Docker daemon
// sees. Every mount source handed to the daemon goes through it; nothing else
// needs to, because container paths are invariant.
func (r *DockerSandboxRuntime) daemonPath(containerPath string) string {
	return r.hostState.HostPath(containerPath)
}

// materializeGitSource brings target to the state source describes, running
// git as whichever identity actually owns target at each step.
//
// It does that exactly once per source. The first call clones (or, for a push
// delivery, parks an empty repository the client pushes into and finalizes on
// the resume) and then records a marker; every later call returns immediately.
// Re-materializing a workspace the sandbox has been using is destructive, not
// merely redundant — see gitMaterializedMarkerName.
//
// A push-delivered source's target is chowned to the sandbox user immediately
// after initGitSource creates it (prepareSandboxVolumes / materializePushedSources),
// so every operation below that point — reset, clean, checkout, workspace
// restore — runs as that user (identity), not as this process's own identity,
// or it trips the repository's dubious-ownership check against a directory it
// no longer owns. A clone-delivered source's target is still owned by this
// process at materialize time (prepareSandboxVolumes only chowns it after this
// function returns), so those same operations run under the calling process's
// own identity there, matching who actually created the clone.
func (r *DockerSandboxRuntime) materializeGitSource(ctx context.Context, source workerapimodel.GitSource, target, slug string, user sandboxuser.User) error {
	// git runs deliberately as the caller, not as the sandbox user.
	identity := sandboxuser.User{}
	if gitSourceAwaitsPush(source) {
		identity = user
	}
	// fetchURL is the real, pool-agent-resolvable location to clone/fetch from.
	// It is computed once here and reused for both the initial clone and every
	// restoreGitWorkspace fetch, rather than read back from the repository's
	// "origin" remote — rewriteOriginRemote repoints that remote at the
	// in-sandbox path /.discobox/origins/<slug>, which only the sandbox, not
	// this process, can resolve.
	var fetchURL string
	if !gitSourceAwaitsPush(source) {
		url, err := gitSourceCloneURL(source, r.hostMountPrefix)
		if err != nil {
			return err
		}
		fetchURL = url
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		// A source is materialized exactly once, whatever its delivery mode.
		// Every later create for the same sandbox — a resume, a re-pin, a
		// reconcile that re-drives create after a failure — must leave the
		// workspace alone: the sandbox has been using it since, so the
		// reset/clean/checkout below would discard uncommitted work and move
		// the branch off commits made inside the sandbox.
		if gitSourceMaterialized(target) {
			return nil
		}
		if gitSourceAwaitsPush(source) {
			// A push-delivered repository with no commits is still waiting for
			// the client. There is nothing to reset, clean, or check out yet,
			// and every one of those fails against an unborn branch.
			if !gitHasCommits(ctx, target, chownID(identity.UID), chownID(identity.GID)) {
				return nil
			}
		}
		// A prior create attempt may have already restored a dirty workspace.
		// Return the repository to a clean state before materializing the desired
		// checkout again so this operation remains retry-safe.
		if err := runGit(ctx, target, chownID(identity.UID), chownID(identity.GID), "reset", "--hard"); err != nil {
			return err
		}
		if err := runGit(ctx, target, chownID(identity.UID), chownID(identity.GID), "clean", "-fd"); err != nil {
			return err
		}
		if err := checkoutGitSource(ctx, target, source, chownID(identity.UID), chownID(identity.GID)); err != nil {
			return err
		}
		if err := r.restoreGitWorkspace(ctx, target, source, fetchURL, chownID(identity.UID), chownID(identity.GID)); err != nil {
			return err
		}
		if err := r.rewriteOriginRemote(ctx, target, source, slug, identity); err != nil {
			return err
		}
		return markGitSourceMaterialized(target, chownID(user.UID), chownID(user.GID))
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if gitSourceAwaitsPush(source) {
		// The client delivers the commits and, via updateInstead, the working
		// tree. Nothing to clone or check out here.
		return r.initGitSource(ctx, source, target)
	}
	args := []string{"clone"}
	if checkout, ok := source.Checkout.Get(); ok {
		if refName := strings.TrimSpace(optString(checkout.RefName)); refName != "" {
			args = append(args, "--branch", refName)
		}
	}
	args = append(args, fetchURL, target)
	if err := runGitWithSafeDirectories(ctx, "", chownID(identity.UID), chownID(identity.GID), gitSafeDirectories(fetchURL, r.hostMountPrefix), args...); err != nil {
		return err
	}
	if err := checkoutGitSource(ctx, target, source, chownID(identity.UID), chownID(identity.GID)); err != nil {
		return err
	}
	if err := r.restoreGitWorkspace(ctx, target, source, fetchURL, chownID(identity.UID), chownID(identity.GID)); err != nil {
		return err
	}
	if err := r.rewriteOriginRemote(ctx, target, source, slug, identity); err != nil {
		return err
	}
	return markGitSourceMaterialized(target, chownID(user.UID), chownID(user.GID))
}

// rewriteOriginRemote points a clone-delivered local source's "origin" remote
// at the path the sandbox container will see, /.discobox/origins/<slug>,
// instead of the pool-agent-process-local path materializeGitSource actually
// cloned from — which is meaningless once the sandbox container exists.
// restoreGitWorkspace never depends on "origin"'s configured URL (it fetches
// by an explicitly resolved URL instead), so this stays correct even though a
// source is only ever materialized once (ADR 0026): it runs once, at the end
// of the same materialize call that clones and restores the workspace, before
// that source is marked materialized.
func (r *DockerSandboxRuntime) rewriteOriginRemote(ctx context.Context, target string, source workerapimodel.GitSource, slug string, identity sandboxuser.User) error {
	if gitSourceAwaitsPush(source) || slug == "" {
		return nil
	}
	if strings.TrimSpace(optString(source.LocalDirectory)) == "" {
		return nil
	}
	return runGit(ctx, target, chownID(identity.UID), chownID(identity.GID), "remote", "set-url", "origin", path.Join(sandboxOriginsMount, slug))
}

func (r *DockerSandboxRuntime) restoreGitWorkspace(ctx context.Context, repo string, source workerapimodel.GitSource, fetchURL string, uid, gid int) error {
	workspace, ok := source.Workspace.Get()
	if !ok || workspace.Mode.Or(workerclient.GitSourceWorkspaceModeClean) != workerclient.GitSourceWorkspaceModeDirty {
		return nil
	}
	baseCommit := strings.TrimSpace(optString(workspace.BaseCommit))
	snapshotRef := strings.TrimSpace(optString(workspace.SnapshotRef))
	if baseCommit == "" || snapshotRef == "" {
		return fmt.Errorf("dirty workspace requires baseCommit and snapshotRef")
	}
	if err := runGit(ctx, repo, uid, gid, "check-ref-format", snapshotRef); err != nil {
		return fmt.Errorf("invalid workspace snapshot ref %q: %w", snapshotRef, err)
	}

	// A push-delivered source has no origin to fetch from: the client pushed the
	// snapshot ref in alongside the branch, so the objects are already here.
	// Everything below is local and applies to both delivery modes.
	if !gitSourceAwaitsPush(source) {
		refspec := "+" + snapshotRef + ":" + snapshotRef
		// The fetch reads from fetchURL by explicit URL rather than the
		// "origin" remote name, so it stays correct even after
		// rewriteOriginRemote later repoints "origin" to the in-sandbox path
		// (ADR 0026) — that path is only resolvable from inside the sandbox,
		// not from this process. fetchURL is a possibly arbitrary and
		// differently owned local path (materializeGitSource's clone case), so
		// the fetch always runs under the caller's own identity rather than
		// uid/gid — the same identity that owns repo at this point in the
		// clone-delivered path.
		if err := runGitWithSafeDirectories(ctx, repo, -1, -1, gitSafeDirectories(fetchURL, r.hostMountPrefix), "fetch", fetchURL, refspec); err != nil {
			return fmt.Errorf("fetch workspace snapshot %q: %w", snapshotRef, err)
		}
	} else if err := runGit(ctx, repo, uid, gid, "rev-parse", "--verify", "--quiet", snapshotRef+"^{commit}"); err != nil {
		return fmt.Errorf("workspace snapshot %q was not pushed to the sandbox", snapshotRef)
	}

	parent, err := runGitOutput(ctx, repo, uid, gid, nil, "rev-parse", "--verify", snapshotRef+"^")
	if err != nil {
		return fmt.Errorf("resolve workspace snapshot parent: %w", err)
	}
	resolvedBase, err := runGitOutput(ctx, repo, uid, gid, nil, "rev-parse", "--verify", baseCommit+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve workspace base commit: %w", err)
	}
	if strings.TrimSpace(string(parent)) != strings.TrimSpace(string(resolvedBase)) {
		return fmt.Errorf("workspace snapshot %q is not based on %s", snapshotRef, baseCommit)
	}

	// Keep the branch selected by checkoutGitSource, but move it to the exact
	// base commit before applying the snapshot tree diff to the worktree only.
	// This deliberately leaves both originally staged and unstaged changes
	// unstaged in the sandbox.
	if err := runGit(ctx, repo, uid, gid, "reset", "--hard", baseCommit); err != nil {
		return err
	}
	patch, err := runGitOutput(ctx, repo, uid, gid, nil, "diff", "--binary", "--full-index", baseCommit, snapshotRef, "--")
	if err != nil {
		return fmt.Errorf("create workspace snapshot patch: %w", err)
	}
	if len(patch) == 0 {
		return nil
	}
	if _, err := runGitOutput(ctx, repo, uid, gid, patch, "apply", "--binary", "--whitespace=nowarn", "-"); err != nil {
		return fmt.Errorf("apply workspace snapshot: %w", err)
	}
	return nil
}

func gitSourceCloneURL(source workerapimodel.GitSource, hostMountPrefix string) (string, error) {
	if local := strings.TrimSpace(optString(source.LocalDirectory)); local != "" {
		return hostMountedLocalDirectory(local, hostMountPrefix), nil
	}
	if sourceURL, ok := source.URL.Get(); ok {
		return sourceURL.String(), nil
	}
	return "", fmt.Errorf("source URL or localDirectory is required")
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

// gitMaterializedMarkerName records, inside a source's .git directory, that
// materializeGitSource has already finished checking it out and restoring its
// workspace once, so no later create touches the workspace again. It lives
// under .git rather than the worktree so it never appears as an untracked file
// the sandbox user sees.
const gitMaterializedMarkerName = "discobox-materialized"

func gitMaterializedMarkerPath(target string) string {
	return filepath.Join(target, ".git", gitMaterializedMarkerName)
}

// gitSourceMaterialized reports whether a source has already been finalized
// once, so a repeat create knows to leave its workspace alone.
func gitSourceMaterialized(target string) bool {
	_, err := os.Stat(gitMaterializedMarkerPath(target))
	return err == nil
}

// markGitSourceMaterialized records that a source has been finalized, owned by
// the same sandbox user as the rest of the repository.
func markGitSourceMaterialized(target string, uid, gid int) error {
	path := gitMaterializedMarkerPath(target)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

// initGitSource creates the empty repository a client pushes its source into.
//
// The repository must exist before git http-backend will serve it, so a source
// with nothing to clone from is initialized rather than rejected.
//
// The initial branch matters: receive.denyCurrentBranch=updateInstead only
// updates the working tree when the pushed branch is the checked-out one, so
// initializing on the branch the client will push is what makes the pushed
// commit appear in the sandbox rather than sitting in the object store.
func (r *DockerSandboxRuntime) initGitSource(ctx context.Context, source workerapimodel.GitSource, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	args := []string{"init"}
	if branch := gitSourceInitialBranch(source); branch != "" {
		args = append(args, "-b", branch)
	}
	args = append(args, target)
	// target does not exist yet, so this always runs under the caller's own
	// identity; prepareSandboxVolumes/materializePushedSources chown it to the
	// sandbox user immediately after this returns.
	if err := runGit(ctx, "", -1, -1, args...); err != nil {
		return fmt.Errorf("initialize source repository for push: %w", err)
	}
	return nil
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

// sourceUpstreamRef is the remote-tracking ref the source would fetch upstream
// into, derived from the branch it was cloned at — the same derivation
// `disco diff` uses. Only a clone at a branch names one; anything else falls
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

func gitSafeDirectories(cloneURL, hostMountPrefix string) []string {
	if strings.Contains(cloneURL, "://") || !filepath.IsAbs(cloneURL) {
		return nil
	}
	cloneURL = cleanAbsPath(cloneURL)
	if cloneURL == "" {
		return nil
	}
	hostMountPrefix = cleanAbsPath(hostMountPrefix)
	if hostMountPrefix != "" && (cloneURL == hostMountPrefix || strings.HasPrefix(cloneURL, hostMountPrefix+string(filepath.Separator))) {
		return []string{hostMountPrefix, filepath.Join(hostMountPrefix, "*")}
	}
	dirs := []string{cloneURL}
	if filepath.Base(cloneURL) != ".git" {
		dirs = append(dirs, filepath.Join(cloneURL, ".git"))
	}
	return dirs
}

func checkoutGitSource(ctx context.Context, repo string, source workerapimodel.GitSource, uid, gid int) error {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return nil
	}
	refName := strings.TrimSpace(optString(checkout.RefName))
	refType := strings.ToLower(strings.TrimSpace(optString(checkout.RefType)))
	if commit := strings.TrimSpace(optString(checkout.Commit)); commit != "" {
		if refName != "" && refType == "branch" {
			return runGit(ctx, repo, uid, gid, "checkout", "-B", refName, commit)
		}
		return runGit(ctx, repo, uid, gid, "checkout", "--detach", commit)
	}
	if refName != "" {
		return runGit(ctx, repo, uid, gid, "checkout", refName)
	}
	return nil
}

// runGit and its variants run git against dir as uid/gid, or as the calling
// process's own identity when uid is negative. A repository's dubious-ownership
// check trips whenever the running process's identity doesn't match the
// directory's owner, so callers must pass whichever identity actually owns dir
// (see materializeGitSource for how that identity is chosen per operation).
func runGit(ctx context.Context, dir string, uid, gid int, args ...string) error {
	return runGitWithEnv(ctx, dir, uid, gid, nil, args...)
}

func runGitWithSafeDirectories(ctx context.Context, dir string, uid, gid int, safeDirectories []string, args ...string) error {
	if len(safeDirectories) == 0 {
		return runGit(ctx, dir, uid, gid, args...)
	}
	config, err := os.CreateTemp("", "discobox-gitconfig-*")
	if err != nil {
		return err
	}
	defer os.Remove(config.Name())
	for _, safeDirectory := range safeDirectories {
		if strings.ContainsAny(safeDirectory, "\x00\r\n") {
			_ = config.Close()
			return fmt.Errorf("invalid git safe.directory path %q", safeDirectory)
		}
		if _, err := fmt.Fprintf(config, "[safe]\n\tdirectory = %s\n", safeDirectory); err != nil {
			_ = config.Close()
			return err
		}
	}
	if err := config.Close(); err != nil {
		return err
	}
	return runGitWithEnv(ctx, dir, uid, gid, []string{"GIT_CONFIG_GLOBAL=" + config.Name()}, args...)
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
	out, err := cmd.CombinedOutput()
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
	out, err := cmd.CombinedOutput()
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
// resolve. The pool agent runs what it is told and invents nothing: it used to
// substitute a plain alpine image for a missing one, which cannot host a
// sandbox agent at all, so the result was a container that could never answer
// instead of a failure naming the request that was wrong. Every sandbox carries
// a harness config (ADR 0025), so a request without one is a control plane that
// failed to resolve it, not a sandbox asking for a bare shell.
func validateCreateRequest(sandboxID string, req *workerapimodel.PoolSandboxCreateRequest) error {
	if strings.TrimSpace(optString(req.Config.Image)) == "" {
		return fmt.Errorf("sandbox %s: create request has no image", sandboxID)
	}
	if _, ok := req.ResolvedHarnessConfig.Get(); !ok {
		return fmt.Errorf("sandbox %s: create request has no resolved harness config", sandboxID)
	}
	return nil
}
