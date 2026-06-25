package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
)

const (
	defaultSandboxImage = "alpine:3.20"
	sandboxAgentPort    = 3003
	sandboxDataRoot     = "/var/lib/discobox/projects"
	sandboxLabelManaged = "discobox.sandbox.managed"
	sandboxLabelProject = "discobox.project_id"
	sandboxLabelWorker  = "discobox.worker_id"
	sandboxLabelSandbox = "discobox.sandbox_id"
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
}

func NewDockerSandboxRuntime(projectID, workerID, controlPlanePublicKey string) (*DockerSandboxRuntime, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &DockerSandboxRuntime{client: cli, projectID: projectID, workerID: workerID, controlPlanePublicKey: controlPlanePublicKey}, nil
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
		out = append(out, r.sandboxFromInspect(inspect.Container))
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
	imageName := strings.TrimSpace(optString(req.Image))
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
	if err := r.writeSandboxAgentConfig(ctx, sandboxID, req); err != nil {
		return nil, err
	}
	name := sandboxContainerName(r.workerID, sandboxID)
	cfg := &container.Config{
		Image:      imageName,
		Labels:     r.labels(sandboxID),
		Env:        envList(envWithSandboxUser(map[string]string(optCreateEnv(req.Env)), user)),
		WorkingDir: sourceWorkingDirectory(req),
	}
	hostCfg := &container.HostConfig{Mounts: mounts, Privileged: true}
	if memoryBytes := optInt64(req.MemoryBytes); memoryBytes > 0 {
		hostCfg.Memory = memoryBytes
	} else if resources, ok := req.Resources.Get(); ok && resources.MemoryMB > 0 {
		hostCfg.Memory = resources.MemoryMB * 1024 * 1024
	}
	if cpuVCPUs := optFloat64(req.CpuVcpus); cpuVCPUs > 0 {
		hostCfg.NanoCPUs = int64(cpuVCPUs * 1_000_000_000)
	} else if resources, ok := req.Resources.Get(); ok && resources.CPUCores > 0 {
		hostCfg.NanoCPUs = int64(resources.CPUCores * 1_000_000_000)
	}
	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: cfg, HostConfig: hostCfg, Name: name})
	if err != nil {
		return nil, err
	}
	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
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
	homePath := filepath.Join(sandboxVolumesRoot(r.projectID, sandboxID), "home")
	if err := prepareOwnedDirectory(ctx, homePath, user.uid, user.gid); err != nil {
		return nil, fmt.Errorf("set home ownership: %w", err)
	}
	mounts = append(mounts, mount.Mount{
		Type:   mount.TypeBind,
		Source: homePath,
		Target: user.homeDirectory,
	})
	configPath := sandboxConfigRoot(r.projectID, sandboxID)
	if err := prepareOwnedDirectory(ctx, configPath, 0, 0); err != nil {
		return nil, fmt.Errorf("prepare sandbox config directory: %w", err)
	}
	mounts = append(mounts, mount.Mount{
		Type:     mount.TypeBind,
		Source:   configPath,
		Target:   "/etc/discobox",
		ReadOnly: true,
	})
	for _, source := range sources {
		hostPath := filepath.Join(sandboxVolumesRoot(r.projectID, sandboxID), "source", source.slug)
		if err := materializeGitSource(ctx, source.git, hostPath); err != nil {
			return nil, fmt.Errorf("materialize source %q: %w", source.slug, err)
		}
		if err := prepareOwnedDirectory(ctx, hostPath, user.uid, user.gid); err != nil {
			return nil, fmt.Errorf("set source ownership %q: %w", source.slug, err)
		}
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: hostPath,
			Target: source.target,
		})
	}
	return mounts, nil
}

func (r *DockerSandboxRuntime) writeSandboxAgentConfig(ctx context.Context, sandboxID string, req *workerapimodel.WorkerSandboxCreateRequest) error {
	configDir := sandboxConfigRoot(r.projectID, sandboxID)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	cfg := sandboxAgentConfig{
		Identity: sandboxAgentIdentity{
			ProjectID: r.projectID,
			SandboxID: sandboxID,
			WorkerID:  r.workerID,
		},
		ControlPlanePublicKey: r.controlPlanePublicKey,
		ListenAddress:         fmt.Sprintf(":%d", sandboxAgentPort),
		WorkingRoot:           "/workspace",
		RuntimeDir:            "/run/discobox/agent-terminals",
		DatabasePath:          "/var/lib/discobox/sandbox-agent.db",
		Resources: sandboxAgentResourceConfig{
			SampleInterval: int64(time.Second),
			RetentionCount: 300,
		},
	}
	if req != nil {
		if resolved, ok := req.ResolvedAgentConfig.Get(); ok {
			cfg.Agents = append(cfg.Agents, sandboxAgentConfigAgent{
				ID:      resolved.ID,
				Name:    resolved.Name,
				Command: []string{"/bin/bash", "-lc", resolved.RunCommand},
			})
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(configDir, "sandbox-agent.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return chownRecursive(ctx, configDir, 0, 0)
}

func prepareOwnedDirectory(ctx context.Context, dir string, uid, gid int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	return r.sandboxFromInspect(inspect.Container), nil
}

func (r *DockerSandboxRuntime) UpdateSandbox(ctx context.Context, sandboxID string, _ *workerapimodel.WorkerSandboxUpdateRequest) (*Sandbox, error) {
	return r.GetSandbox(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = r.client.ContainerRemove(ctx, sb.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
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
	repoPath := filepath.Join(sandboxVolumesRoot(r.projectID, sandboxID), "source", repositoryID)
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

func (r *DockerSandboxRuntime) sandboxFromInspect(inspect container.InspectResponse) *Sandbox {
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
	}
	return sb
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
	sb := &Sandbox{ID: req.SandboxId, SandboxID: req.SandboxId, Status: StatusRunning, Image: optString(req.Image), CreatedAt: now, StartedAt: &now, Env: copyMap(map[string]string(optCreateEnv(req.Env)))}
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
		if image := optString(req.Image); image != "" {
			sb.Image = image
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

type sandboxAgentConfig struct {
	Identity              sandboxAgentIdentity       `json:"identity"`
	ControlPlanePublicKey string                     `json:"controlPlanePublicKey"`
	ListenAddress         string                     `json:"listenAddress"`
	WorkingRoot           string                     `json:"workingRoot"`
	RuntimeDir            string                     `json:"runtimeDir"`
	DatabasePath          string                     `json:"databasePath"`
	Agents                []sandboxAgentConfigAgent  `json:"agents,omitempty"`
	Resources             sandboxAgentResourceConfig `json:"resources"`
}

type sandboxAgentIdentity struct {
	ProjectID string `json:"projectId"`
	SandboxID string `json:"sandboxId"`
	WorkerID  string `json:"workerId"`
}

type sandboxAgentConfigAgent struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Command []string `json:"command"`
}

type sandboxAgentResourceConfig struct {
	SampleInterval int64 `json:"sampleInterval"`
	RetentionCount int   `json:"retentionCount"`
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
		homeDirectory: "/root",
	}
	if req == nil {
		return out
	}
	user, ok := req.User.Get()
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
	source, ok := req.Source.Get()
	if !ok {
		return ""
	}
	destination, ok := source.Destination.Get()
	if !ok {
		return ""
	}
	return optString(destination.WorkingDirectory)
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
	if source, ok := req.Source.Get(); ok {
		out = append(out, sandboxSourceFor("primary", source, "/workspace", used))
	}
	if refs, ok := req.SourceCodeReferences.Get(); ok {
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

func sandboxVolumesRoot(projectID, sandboxID string) string {
	return filepath.Join(sandboxDataRoot, projectID, "sandboxes", sandboxID, "volumes")
}

func materializeGitSource(ctx context.Context, source workerapimodel.GitSource, target string) error {
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		return checkoutGitSource(ctx, target, source)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	cloneURL, err := gitSourceCloneURL(source)
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
	if err := runGit(ctx, "", args...); err != nil {
		return err
	}
	return checkoutGitSource(ctx, target, source)
}

func gitSourceCloneURL(source workerapimodel.GitSource) (string, error) {
	if local := strings.TrimSpace(optString(source.LocalDirectory)); local != "" {
		return local, nil
	}
	if sourceURL, ok := source.URL.Get(); ok {
		return sourceURL.String(), nil
	}
	return "", fmt.Errorf("source URL or localDirectory is required")
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
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
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

func optCreateEnv(opt workerclient.OptWorkerSandboxCreateRequestEnv) workerclient.WorkerSandboxCreateRequestEnv {
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
