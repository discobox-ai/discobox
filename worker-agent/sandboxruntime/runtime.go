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
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	apigen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"

	"github.com/obot-platform/discobox/worker-agent/proxyagent"

	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
)

const (
	defaultSandboxImage      = "alpine:3.20"
	sandboxAgentPort         = 3003
	sandboxAgentReadyTimeout = 30 * time.Second
	sandboxAgentPollInterval = 100 * time.Millisecond
	sandboxDataRoot          = "/var/lib/discobox/projects"
	sandboxLabelManaged      = "discobox.sandbox.managed"
	sandboxLabelProject      = "discobox.project_id"
	sandboxLabelWorker       = "discobox.worker_id"
	sandboxLabelSandbox      = "discobox.sandbox_id"
	sandboxManifestPublicKey = "controlPlane"
)

var (
	ErrNotFound      = errors.New("sandbox not found")
	ErrAlreadyExists = errors.New("sandbox already exists")
)

// Sandbox is the worker-local runtime view of a sandbox instance.
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

// AssignedPort describes a runtime-assigned port mapping.
type AssignedPort struct {
	ContainerPort int
	HostPort      int
	HostIP        string
	Protocol      string
}

// Status is the worker-local runtime status.
type Status string

const (
	StatusCreated Status = "created"
	StatusRunning Status = "running"
	StatusStopped Status = "stopped"
	StatusFailed  Status = "failed"
	StatusRemoved Status = "removed"
)

// Runtime performs local sandbox operations for one worker agent.
type Runtime interface {
	ListSandboxes(ctx context.Context) ([]*Sandbox, error)
	GetSandbox(ctx context.Context, sandboxID string) (*Sandbox, error)
	CreateSandbox(ctx context.Context, req *workerapimodel.WorkerSandboxCreateRequest) (*Sandbox, error)
	UpdateSandbox(ctx context.Context, sandboxID string, req *workerapimodel.WorkerSandboxUpdateRequest) (*Sandbox, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	StartSandbox(ctx context.Context, sandboxID string, req *workerapimodel.WorkerSandboxOperationRequest) (*Sandbox, error)
	StopSandbox(ctx context.Context, sandboxID string, req *workerapimodel.WorkerSandboxOperationRequest) (*Sandbox, error)
	GitRepositoryPath(ctx context.Context, sandboxID, repositoryID string) (string, error)
	HTTPBaseURL(ctx context.Context, sandboxID string, port int) (*url.URL, error)
}

// DockerSandboxRuntime launches sandboxes as Docker containers inside a worker.
type DockerSandboxRuntime struct {
	client                *client.Client
	projectID             string
	workerID              string
	controlPlanePublicKey string
	hostMountPrefix       string
}

type DockerSandboxRuntimeConfig struct {
	ProjectID             string
	WorkerID              string
	ControlPlanePublicKey string
	HostMountPrefix       string
}

func NewDockerSandboxRuntime(cfg DockerSandboxRuntimeConfig) (*DockerSandboxRuntime, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &DockerSandboxRuntime{
		client:                cli,
		projectID:             cfg.ProjectID,
		workerID:              cfg.WorkerID,
		controlPlanePublicKey: cfg.ControlPlanePublicKey,
		hostMountPrefix:       cleanAbsPath(cfg.HostMountPrefix),
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

func (r *DockerSandboxRuntime) CreateSandbox(ctx context.Context, req *workerapimodel.WorkerSandboxCreateRequest) (*Sandbox, error) {
	sandboxID := ""
	if req != nil {
		sandboxID = strings.TrimSpace(req.SandboxId)
	}
	if sandboxID == "" {
		return nil, fmt.Errorf("sandbox ID is required")
	}
	if existing, err := r.GetSandbox(ctx, sandboxID); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	normalizeSandboxConfig(&req.Config)
	config := req.Config
	imageName := strings.TrimSpace(optString(config.Image))
	if imageName == "" {
		imageName = defaultSandboxImage
	}
	if err := r.ensureImageAvailable(ctx, imageName); err != nil {
		return nil, err
	}
	user := resolveSandboxUser(req)
	mounts, err := r.prepareSandboxVolumes(ctx, sandboxID, req, user)
	if err != nil {
		return nil, err
	}
	proxyMaterial, err := proxyagent.EnsureSandboxMaterial(sandboxID, r.workerHostPath)
	if err != nil {
		return nil, err
	}
	sentinels, _ := req.Sentinels.Get()
	if err := proxyagent.UpsertSandboxSentinels(r.workerHostPath, sandboxID, sentinels); err != nil {
		return nil, err
	}
	mounts = append(mounts, mount.Mount{
		Type:     mount.TypeBind,
		Source:   proxyMaterial.MountSource,
		Target:   proxyagent.SandboxProxyMount,
		ReadOnly: true,
	})
	if err := r.writeSandboxHarnessConfig(ctx, sandboxID, req, proxyMaterial.Env); err != nil {
		return nil, err
	}
	baseEnv := mergeEnv(map[string]string(optSandboxConfigEnv(config.Env)), proxyMaterial.Env)
	name := sandboxContainerName(r.workerID, sandboxID)
	cfg := &container.Config{
		Image:        imageName,
		Labels:       r.labels(sandboxID),
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
	if memoryBytes := optInt64(config.MemoryBytes); memoryBytes > 0 {
		hostCfg.Memory = memoryBytes
	} else if resources, ok := req.Resources.Get(); ok && resources.MemoryMb > 0 {
		hostCfg.Memory = resources.MemoryMb * 1024 * 1024
	}
	if cpuVCPUs := optFloat64(config.CpuVcpus); cpuVCPUs > 0 {
		hostCfg.NanoCPUs = int64(cpuVCPUs * 1_000_000_000)
	} else if resources, ok := req.Resources.Get(); ok && resources.CpuCores > 0 {
		hostCfg.NanoCPUs = int64(resources.CpuCores * 1_000_000_000)
	}
	// Attach the sandbox to the per-worker internal network only: it reaches the
	// worker proxy (resolved as discobox-worker-proxy via Docker embedded DNS)
	// and DNS, but has no route off-box, so all egress is forced through the proxy.
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			proxyagent.SandboxNetworkName(r.workerID): {},
		},
	}
	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg, NetworkingConfig: netCfg, Name: name})
	if err != nil {
		return nil, err
	}
	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	if err := r.waitForSandboxAgent(ctx, sandboxID); err != nil {
		return nil, err
	}
	return r.GetSandbox(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) ensureImageAvailable(ctx context.Context, imageName string) error {
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
	if err := pull.Wait(ctx); err != nil {
		return fmt.Errorf("pull image %q: %w", imageName, err)
	}
	return nil
}

func (r *DockerSandboxRuntime) prepareSandboxVolumes(ctx context.Context, sandboxID string, req *workerapimodel.WorkerSandboxCreateRequest, user sandboxUserIdentity) ([]mount.Mount, error) {
	sources := sandboxSources(req)
	mounts := make([]mount.Mount, 0, len(sources)+1)
	homeHostPath := filepath.Join(sandboxVolumesRoot(r.projectID, sandboxID), "home")
	homeWorkerPath := r.workerHostPath(homeHostPath)
	if err := prepareOwnedDirectory(ctx, homeWorkerPath, user.uid, user.gid); err != nil {
		return nil, fmt.Errorf("set home ownership: %w", err)
	}
	mounts = append(mounts, mount.Mount{
		Type:   mount.TypeBind,
		Source: homeHostPath,
		Target: user.homeDirectory,
	})
	configHostPath := sandboxConfigRoot(r.projectID, sandboxID)
	configWorkerPath := r.workerHostPath(configHostPath)
	if err := prepareOwnedDirectory(ctx, configWorkerPath, 0, 0); err != nil {
		return nil, fmt.Errorf("prepare sandbox config directory: %w", err)
	}
	mounts = append(mounts, mount.Mount{
		Type:     mount.TypeBind,
		Source:   configHostPath,
		Target:   "/etc/discobox",
		ReadOnly: true,
	})
	for _, source := range sources {
		sourceHostPath := filepath.Join(sandboxVolumesRoot(r.projectID, sandboxID), "source", source.slug)
		sourceWorkerPath := r.workerHostPath(sourceHostPath)
		if err := r.materializeGitSource(ctx, source.git, sourceWorkerPath); err != nil {
			return nil, fmt.Errorf("materialize source %q: %w", source.slug, err)
		}
		if err := prepareOwnedDirectory(ctx, sourceWorkerPath, user.uid, user.gid); err != nil {
			return nil, fmt.Errorf("set source ownership %q: %w", source.slug, err)
		}
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: sourceHostPath,
			Target: source.target,
		})
	}
	return mounts, nil
}

func (r *DockerSandboxRuntime) writeSandboxHarnessConfig(ctx context.Context, sandboxID string, req *workerapimodel.WorkerSandboxCreateRequest, proxyEnv map[string]string) error {
	configDir := r.workerHostPath(sandboxConfigRoot(r.projectID, sandboxID))
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	// The proxy material is bind-mounted at /etc/discobox/proxy, nested under the
	// read-only /etc/discobox config mount. Pre-create the mountpoint here so the
	// container runtime does not have to create it inside the read-only parent.
	if err := os.MkdirAll(filepath.Join(configDir, "proxy"), 0o755); err != nil {
		return err
	}
	cfg := buildSandboxManifest(r.projectID, sandboxID, r.workerID, r.controlPlanePublicKey, req, proxyEnv)
	data, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(configDir, "sandbox.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return chownRecursive(ctx, configDir, 0, 0)
}

func manifestHarnessConfigFiles(files []workerapimodel.HarnessConfigFile) []apimodel.HarnessConfigFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]apimodel.HarnessConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, apimodel.HarnessConfigFile{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: publicOptBool(file.CreateOnly),
			Template:   publicOptBool(file.Template),
		})
	}
	return out
}

func buildSandboxManifest(projectID, sandboxID, workerID, controlPlanePublicKey string, req *workerapimodel.WorkerSandboxCreateRequest, proxyEnv map[string]string) apimodel.SandboxManifest {
	manifest := apimodel.SandboxManifest{
		APIVersion: apimodel.SandboxManifestAPIVersion,
		SandboxID:  sandboxID,
		Provider: &apimodel.SandboxManifestProvider{
			Kind:      "discobox-worker",
			ProjectID: projectID,
			PublicKeys: map[string]string{
				sandboxManifestPublicKey: controlPlanePublicKey,
			},
			WorkerID: workerID,
		},
		AgentRuntime: &apimodel.SandboxManifestAgentRuntime{
			ListenAddress: fmt.Sprintf(":%d", sandboxAgentPort),
			WorkingRoot:   "/workspace",
			RuntimeDir:    "/run/discobox/agent-terminals",
			DatabasePath:  "/var/lib/discobox/sandbox-agent.db",
			ResourceCollection: &apimodel.SandboxManifestResourceCollection{
				SampleInterval: time.Second.String(),
				RetentionCount: 300,
			},
		},
	}
	if req != nil {
		manifest.Config = publicSandboxConfig(req.Config)
		// The worker owns the effective sandbox user used for the home mount and
		// container environment. Publish that fully resolved identity even when
		// the request omitted or partially specified config.user, so the
		// sandbox-agent installs harness files and launches commands against the
		// same home directory.
		user := resolveSandboxUser(req)
		manifest.Config.User = apigen.NewOptSandboxUser(apigen.SandboxUser{
			Name:          apigen.NewOptString(user.name),
			UID:           apigen.NewOptInt64(int64(user.uid)),
			Gid:           apigen.NewOptInt64(int64(user.gid)),
			HomeDirectory: apigen.NewOptString(user.homeDirectory),
		})
		if resources, ok := req.Resources.Get(); ok {
			manifest.Resources = &apimodel.SandboxResources{
				CPUCores:       resources.CpuCores,
				DiskMB:         resources.DiskMb,
				MemoryMB:       resources.MemoryMb,
				TimeoutSeconds: resources.TimeoutSeconds,
			}
		}
		if resolved, ok := req.ResolvedHarnessConfig.Get(); ok {
			resolvedConfig := apimodel.SandboxManifestResolvedHarnessConfig{
				ID: resolved.ID, Name: resolved.Name,
			}
			if files, ok := resolved.Files.Get(); ok {
				resolvedConfig.Files = manifestHarnessConfigFiles(files)
			}
			manifest.ResolvedHarnessConfig = &resolvedConfig
		}
	}
	// Inject the worker-proxy environment so sandbox-agent-spawned terminals and
	// execs route outbound traffic through the local forwarder and trust the
	// MITM CA.
	if len(proxyEnv) > 0 {
		env := map[string]string{}
		if existing, ok := manifest.Config.Env.Get(); ok {
			for key, value := range existing {
				env[key] = value
			}
		}
		for key, value := range proxyEnv {
			env[key] = value
		}
		manifest.Config.Env = apigen.NewOptSandboxConfigEnv(apigen.SandboxConfigEnv(env))
	}
	return manifest
}

func publicSandboxConfig(config workerapimodel.SandboxConfig) apimodel.SandboxConfig {
	out := apimodel.SandboxConfig{
		HarnessConfigId:     publicOptString(config.HarnessConfigId),
		Model:               publicOptString(config.Model),
		ModelReasoningLevel: publicOptString(config.ModelReasoningLevel),
		ModelServiceTier:    publicOptString(config.ModelServiceTier),
		Description:         publicOptString(config.Description),
		Image:               optString(config.Image),
		Name:                optString(config.Name),
		Prompt:              config.Prompt,
		CpuVcpus:            optFloat64(config.CpuVcpus),
		MemoryBytes:         optInt64(config.MemoryBytes),
		StorageBytes:        optInt64(config.StorageBytes),
	}
	if mode, ok := config.HarnessMode.Get(); ok {
		out.HarnessMode = apigen.NewOptSandboxConfigHarnessMode(apigen.SandboxConfigHarnessMode(mode))
	}
	if env, ok := config.Env.Get(); ok {
		out.Env = apigen.NewOptSandboxConfigEnv(apigen.SandboxConfigEnv(env))
	}
	if source, ok := config.Source.Get(); ok {
		out.Source = apigen.NewOptGitSource(publicGitSource(source))
	}
	if refs, ok := config.SourceCodeReferences.Get(); ok {
		outRefs := make(apigen.SandboxConfigSourceCodeReferences, len(refs))
		for key, ref := range refs {
			outRefs[key] = publicGitSource(ref)
		}
		out.SourceCodeReferences = apigen.NewOptSandboxConfigSourceCodeReferences(outRefs)
	}
	if user, ok := config.User.Get(); ok {
		out.User = apigen.NewOptSandboxUser(publicSandboxUser(user))
	}
	return out
}

func publicGitSource(source workerapimodel.GitSource) apigen.GitSource {
	out := apigen.GitSource{
		Kind: apigen.GitSourceKind(source.Kind),
	}
	if checkout, ok := source.Checkout.Get(); ok {
		out.Checkout = apigen.NewOptGitSourceCheckout(apigen.GitSourceCheckout{
			Commit:  publicOptString(checkout.Commit),
			RefName: publicOptString(checkout.RefName),
			RefType: publicOptString(checkout.RefType),
		})
	}
	if destination, ok := source.Destination.Get(); ok {
		out.Destination = apigen.NewOptGitSourceDestination(apigen.GitSourceDestination{
			Directory:        publicOptString(destination.Directory),
			WorkingDirectory: publicOptString(destination.WorkingDirectory),
		})
	}
	out.LocalDirectory = publicOptString(source.LocalDirectory)
	out.Slug = publicOptString(source.Slug)
	out.URL = publicOptURI(source.URL)
	if workspace, ok := source.Workspace.Get(); ok {
		out.Workspace = apigen.NewOptGitSourceWorkspace(apigen.GitSourceWorkspace{
			BaseCommit:  publicOptString(workspace.BaseCommit),
			Mode:        publicOptGitSourceWorkspaceMode(workspace.Mode),
			SnapshotRef: publicOptString(workspace.SnapshotRef),
		})
	}
	return out
}

func publicSandboxUser(user workerapimodel.SandboxUser) apigen.SandboxUser {
	return apigen.SandboxUser{
		Gid:           publicOptInt64(user.Gid),
		HomeDirectory: publicOptString(user.HomeDirectory),
		Name:          publicOptString(user.Name),
		UID:           publicOptInt64(user.UID),
	}
}

func publicOptString(opt workerclient.OptString) apigen.OptString {
	if value, ok := opt.Get(); ok {
		return apigen.NewOptString(value)
	}
	return apigen.OptString{}
}

func publicOptBool(opt workerclient.OptBool) apigen.OptBool {
	if value, ok := opt.Get(); ok {
		return apigen.NewOptBool(value)
	}
	return apigen.OptBool{}
}

func publicOptInt64(opt workerclient.OptInt64) apigen.OptInt64 {
	if value, ok := opt.Get(); ok {
		return apigen.NewOptInt64(value)
	}
	return apigen.OptInt64{}
}

func publicOptURI(opt workerclient.OptURI) apigen.OptURI {
	if value, ok := opt.Get(); ok {
		return apigen.NewOptURI(value)
	}
	return apigen.OptURI{}
}

func publicOptGitSourceWorkspaceMode(opt workerclient.OptGitSourceWorkspaceMode) apigen.OptGitSourceWorkspaceMode {
	if value, ok := opt.Get(); ok {
		return apigen.NewOptGitSourceWorkspaceMode(apigen.GitSourceWorkspaceMode(value))
	}
	return apigen.OptGitSourceWorkspaceMode{}
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

func (r *DockerSandboxRuntime) UpdateSandbox(ctx context.Context, sandboxID string, req *workerapimodel.WorkerSandboxUpdateRequest) (*Sandbox, error) {
	if req != nil {
		if sentinels, ok := req.Sentinels.Get(); ok {
			// Re-register the sandbox's sentinel set with the proxy so newly bound
			// secrets resolve without a restart.
			if err := proxyagent.UpsertSandboxSentinels(r.workerHostPath, sandboxID, sentinels); err != nil {
				return nil, err
			}
		}
	}
	return r.GetSandbox(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
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
	if err := proxyagent.RemoveSandboxSentinels(r.workerHostPath, sandboxID); err != nil {
		return err
	}
	return proxyagent.RemoveSandboxMaterial(sandboxID, r.workerHostPath)
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
)

// ReconcileProxyMaterial prunes proxy material for sandboxes whose containers no
// longer exist. It is the recovery path for containers deleted out of band or
// while the worker was down, which never run through DeleteSandbox.
func (r *DockerSandboxRuntime) ReconcileProxyMaterial(ctx context.Context, minAge time.Duration) error {
	sandboxes, err := r.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	live := make([]string, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if sb.SandboxID != "" {
			live = append(live, sb.SandboxID)
		}
	}
	return proxyagent.PruneOrphanedMaterial(live, r.workerHostPath, minAge)
}

// WatchProxyMaterial reconciles orphaned proxy material after establishing a
// Docker event subscription, and thereafter only when the event stream reports
// a managed sandbox container being destroyed. It does not poll on a timer.
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
	filters = filters.Add("label", sandboxLabelWorker+"="+r.workerID)
	result := r.client.Events(ctx, client.EventsListOptions{
		Since:   fmt.Sprintf("%d.%09d", since.Unix(), since.Nanosecond()),
		Filters: filters,
	})

	// Reconcile once the subscription is established. Destroys from `since`
	// onward are buffered by the daemon and delivered on the stream below, so the
	// reconcile and the replayed events together cover every deletion.
	r.reconcileProxyMaterial(ctx, logger)

	debounce := time.NewTimer(0)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
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
		}
	}
}

func (r *DockerSandboxRuntime) StartSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.WorkerSandboxOperationRequest) (*Sandbox, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.Status == StatusRunning {
		return sb, nil
	}
	if _, err := r.client.ContainerStart(ctx, sb.ID, client.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	if err := r.waitForSandboxAgent(ctx, sandboxID); err != nil {
		return nil, err
	}
	return r.GetSandbox(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) StopSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.WorkerSandboxOperationRequest) (*Sandbox, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	timeout := 10
	if _, err := r.client.ContainerStop(ctx, sb.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		return nil, err
	}
	return r.GetSandbox(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) GitRepositoryPath(ctx context.Context, sandboxID, repositoryID string) (string, error) {
	if _, err := r.GetSandbox(ctx, sandboxID); err != nil {
		return "", err
	}
	repoPath := r.workerHostPath(filepath.Join(sandboxVolumesRoot(r.projectID, sandboxID), "source", repositoryID))
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return repoPath, nil
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
			base, err := r.HTTPBaseURL(ctx, sandboxID, sandboxAgentPort)
			if err == nil {
				healthURL := *base
				healthURL.Path = "/healthz"
				req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, healthURL.String(), nil)
				if reqErr != nil {
					return reqErr
				}
				resp, reqErr := http.DefaultClient.Do(req)
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
	args = args.Add("label", sandboxLabelWorker+"="+r.workerID)
	if strings.TrimSpace(sandboxID) != "" {
		args = args.Add("label", sandboxLabelSandbox+"="+sandboxID)
	}
	return args
}

func (r *DockerSandboxRuntime) labels(sandboxID string) map[string]string {
	return map[string]string{
		sandboxLabelManaged: "true",
		sandboxLabelProject: r.projectID,
		sandboxLabelWorker:  r.workerID,
		sandboxLabelSandbox: sandboxID,
	}
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
			"worker_id": r.workerID,
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
	mu              sync.Mutex
	sandboxes       map[string]*Sandbox
	gitRepositories map[string]map[string]string
}

func NewMemorySandboxRuntime() *MemorySandboxRuntime {
	return &MemorySandboxRuntime{
		sandboxes:       map[string]*Sandbox{},
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

func (r *MemorySandboxRuntime) CreateSandbox(_ context.Context, req *workerapimodel.WorkerSandboxCreateRequest) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req == nil {
		return nil, fmt.Errorf("sandbox create request is required")
	}
	now := time.Now().UTC()
	sb := &Sandbox{ID: req.SandboxId, SandboxID: req.SandboxId, Status: StatusRunning, Image: optString(req.Config.Image), CreatedAt: now, StartedAt: &now, Env: copyMap(map[string]string(optSandboxConfigEnv(req.Config.Env)))}
	r.sandboxes[req.SandboxId] = sb
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

func (r *MemorySandboxRuntime) UpdateSandbox(_ context.Context, sandboxID string, req *workerapimodel.WorkerSandboxUpdateRequest) (*Sandbox, error) {
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

func (r *MemorySandboxRuntime) DeleteSandbox(_ context.Context, sandboxID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sandboxes, sandboxID)
	return nil
}

func (r *MemorySandboxRuntime) StartSandbox(_ context.Context, sandboxID string, _ *workerapimodel.WorkerSandboxOperationRequest) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, ErrNotFound
	}
	if sb.Status == StatusRunning {
		return cloneSandbox(sb), nil
	}
	now := time.Now().UTC()
	sb.Status = StatusRunning
	sb.StartedAt = &now
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) StopSandbox(_ context.Context, sandboxID string, _ *workerapimodel.WorkerSandboxOperationRequest) (*Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, ErrNotFound
	}
	now := time.Now().UTC()
	sb.Status = StatusStopped
	sb.StoppedAt = &now
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) GitRepositoryPath(_ context.Context, sandboxID, repositoryID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sandboxes[sandboxID] == nil {
		return "", ErrNotFound
	}
	repositories := r.gitRepositories[sandboxID]
	if repositories == nil || repositories[repositoryID] == "" {
		return "", ErrNotFound
	}
	return repositories[repositoryID], nil
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

func sandboxContainerName(workerID, sandboxID string) string {
	name := "discobox-sandbox-" + workerID + "-" + sandboxID
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, name)
	return strings.Trim(name, "-_.")
}

func sandboxConfigRoot(projectID, sandboxID string) string {
	return filepath.Join(sandboxVolumesRoot(projectID, sandboxID), "config")
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

func optInt64(opt workerclient.OptInt64) int64 {
	v, _ := opt.Get()
	return v
}

func optFloat64(opt workerclient.OptFloat64) float64 {
	v, _ := opt.Get()
	return v
}

type sandboxUserIdentity struct {
	uid           int
	gid           int
	name          string
	homeDirectory string
}

func resolveSandboxUser(req *workerapimodel.WorkerSandboxCreateRequest) sandboxUserIdentity {
	out := sandboxUserIdentity{
		uid:           0,
		gid:           0,
		name:          "root",
		homeDirectory: "/home/root",
	}
	if req == nil {
		return out
	}
	user, ok := req.Config.User.Get()
	if !ok {
		return out
	}
	if uid, ok := user.UID.Get(); ok {
		out.uid = int(uid)
	}
	if gid, ok := user.Gid.Get(); ok {
		out.gid = int(gid)
	} else if user.UID.Set {
		out.gid = out.uid
	}
	if name := strings.TrimSpace(optString(user.Name)); name != "" {
		out.name = name
	}
	if home := cleanContainerPath(optString(user.HomeDirectory)); home != "" {
		out.homeDirectory = home
	} else if out.name != "" && out.name != "root" {
		out.homeDirectory = path.Join("/home", out.name)
	}
	return out
}

func sourceWorkingDirectory(req *workerapimodel.WorkerSandboxCreateRequest) string {
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

func sandboxSources(req *workerapimodel.WorkerSandboxCreateRequest) []sandboxSource {
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

func (r *DockerSandboxRuntime) workerHostPath(hostPath string) string {
	hostPath = cleanAbsPath(hostPath)
	if hostPath == "" {
		return ""
	}
	if r.hostMountPrefix == "" {
		return hostPath
	}
	if hostPath == r.hostMountPrefix || strings.HasPrefix(hostPath, r.hostMountPrefix+string(filepath.Separator)) {
		return hostPath
	}
	return filepath.Join(r.hostMountPrefix, strings.TrimPrefix(hostPath, string(filepath.Separator)))
}

func sandboxVolumesRoot(projectID, sandboxID string) string {
	return filepath.Join(sandboxDataRoot, projectID, "sandboxes", sandboxID, "volumes")
}

func (r *DockerSandboxRuntime) materializeGitSource(ctx context.Context, source workerapimodel.GitSource, target string) error {
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		return checkoutGitSource(ctx, target, source)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	cloneURL, err := gitSourceCloneURL(source, r.hostMountPrefix)
	if err != nil {
		return err
	}
	args := []string{"clone"}
	if checkout, ok := source.Checkout.Get(); ok {
		if refName := strings.TrimSpace(optString(checkout.RefName)); refName != "" {
			args = append(args, "--branch", refName)
		}
	}
	args = append(args, cloneURL, target)
	if err := runGitWithSafeDirectories(ctx, "", gitSafeDirectories(cloneURL, r.hostMountPrefix), args...); err != nil {
		return err
	}
	return checkoutGitSource(ctx, target, source)
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

func checkoutGitSource(ctx context.Context, repo string, source workerapimodel.GitSource) error {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return nil
	}
	refName := strings.TrimSpace(optString(checkout.RefName))
	refType := strings.ToLower(strings.TrimSpace(optString(checkout.RefType)))
	if commit := strings.TrimSpace(optString(checkout.Commit)); commit != "" {
		if refName != "" && refType == "branch" {
			return runGit(ctx, repo, "checkout", "-B", refName, commit)
		}
		return runGit(ctx, repo, "checkout", "--detach", commit)
	}
	if refName != "" {
		return runGit(ctx, repo, "checkout", refName)
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	return runGitWithEnv(ctx, dir, nil, args...)
}

func runGitWithSafeDirectories(ctx context.Context, dir string, safeDirectories []string, args ...string) error {
	if len(safeDirectories) == 0 {
		return runGit(ctx, dir, args...)
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
	return runGitWithEnv(ctx, dir, []string{"GIT_CONFIG_GLOBAL=" + config.Name()}, args...)
}

func runGitWithEnv(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func chownRecursive(ctx context.Context, root string, uid, gid int) error {
	if err := runChown(ctx, root, uid, gid); err == nil {
		return nil
	}
	return filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		//nolint:gosec // The tree is a worker-owned clone target; Lchown avoids following repository symlinks.
		return os.Lchown(p, uid, gid)
	})
}

func runChown(ctx context.Context, root string, uid, gid int) error {
	//nolint:gosec // root is a worker-owned source volume path and args are passed without a shell.
	cmd := exec.CommandContext(ctx, "chown", "-R", "--no-dereference", fmt.Sprintf("%d:%d", uid, gid), root)
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

func envWithSandboxUser(values map[string]string, user sandboxUserIdentity) map[string]string {
	out := copyMap(values)
	out["DISCOBOX_USER_UID"] = fmt.Sprintf("%d", user.uid)
	out["DISCOBOX_USER_GID"] = fmt.Sprintf("%d", user.gid)
	out["DISCOBOX_USER_NAME"] = user.name
	out["DISCOBOX_USER_HOME"] = user.homeDirectory
	if _, ok := out["HOME"]; !ok && user.homeDirectory != "" {
		out["HOME"] = user.homeDirectory
	}
	if _, ok := out["USER"]; !ok && user.name != "" {
		out["USER"] = user.name
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
