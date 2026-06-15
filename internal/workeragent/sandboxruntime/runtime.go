package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/internal/sandbox"
	workerclient "github.com/obot-platform/discobox/internal/workeragent/client/gen"
)

const (
	defaultSandboxImage = "alpine:3.20"
	sandboxLabelManaged = "discobox.sandbox.managed"
	sandboxLabelProject = "discobox.project_id"
	sandboxLabelWorker  = "discobox.worker_id"
	sandboxLabelSandbox = "discobox.sandbox_id"
)

// Runtime performs local sandbox operations for one worker agent.
type Runtime interface {
	ListSandboxes(ctx context.Context) ([]*sandbox.Sandbox, error)
	GetSandbox(ctx context.Context, sandboxID string) (*sandbox.Sandbox, error)
	CreateSandbox(ctx context.Context, req *workerclient.WorkerSandboxCreateRequest) (*sandbox.Sandbox, error)
	UpdateSandbox(ctx context.Context, sandboxID string, req *workerclient.WorkerSandboxUpdateRequest) (*sandbox.Sandbox, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	StartSandbox(ctx context.Context, sandboxID string, req *workerclient.WorkerSandboxOperationRequest) (*sandbox.Sandbox, error)
	StopSandbox(ctx context.Context, sandboxID string, req *workerclient.WorkerSandboxOperationRequest) (*sandbox.Sandbox, error)
}

// DockerSandboxRuntime launches sandboxes as Docker containers inside a worker.
type DockerSandboxRuntime struct {
	client    *client.Client
	projectID string
	workerID  string
}

func NewDockerSandboxRuntime(projectID, workerID string) (*DockerSandboxRuntime, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &DockerSandboxRuntime{client: cli, projectID: projectID, workerID: workerID}, nil
}

func (r *DockerSandboxRuntime) ListSandboxes(ctx context.Context) ([]*sandbox.Sandbox, error) {
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.filters("")})
	if err != nil {
		return nil, err
	}
	out := make([]*sandbox.Sandbox, 0, len(containers.Items))
	for _, ctr := range containers.Items {
		inspect, err := r.client.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, err
		}
		out = append(out, r.sandboxFromInspect(inspect.Container))
	}
	return out, nil
}

func (r *DockerSandboxRuntime) CreateSandbox(ctx context.Context, req *workerclient.WorkerSandboxCreateRequest) (*sandbox.Sandbox, error) {
	sandboxID := ""
	if req != nil {
		sandboxID = strings.TrimSpace(req.SandboxId)
	}
	if sandboxID == "" {
		return nil, fmt.Errorf("sandbox ID is required")
	}
	if existing, err := r.GetSandbox(ctx, sandboxID); err == nil {
		return existing, nil
	} else if !errors.Is(err, sandbox.ErrNotFound) {
		return nil, err
	}
	imageName := strings.TrimSpace(optString(req.Image))
	if imageName == "" {
		imageName = defaultSandboxImage
	}
	pull, err := r.client.ImagePull(ctx, imageName, client.ImagePullOptions{})
	if err != nil {
		return nil, err
	}
	if err := pull.Wait(ctx); err != nil {
		_ = pull.Close()
		return nil, err
	}
	_ = pull.Close()
	name := sandboxContainerName(r.workerID, sandboxID)
	cfg := &container.Config{
		Image:      imageName,
		Labels:     r.labels(sandboxID),
		Env:        envList(map[string]string(optCreateEnv(req.Env))),
		WorkingDir: optString(req.WorkingDirectory),
		Cmd:        []string{"sleep", "infinity"},
	}
	hostCfg := &container.HostConfig{}
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

func (r *DockerSandboxRuntime) GetSandbox(ctx context.Context, sandboxID string) (*sandbox.Sandbox, error) {
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.filters(sandboxID)})
	if err != nil {
		return nil, err
	}
	if len(containers.Items) == 0 {
		return nil, sandbox.ErrNotFound
	}
	inspect, err := r.client.ContainerInspect(ctx, containers.Items[0].ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return r.sandboxFromInspect(inspect.Container), nil
}

func (r *DockerSandboxRuntime) UpdateSandbox(ctx context.Context, sandboxID string, _ *workerclient.WorkerSandboxUpdateRequest) (*sandbox.Sandbox, error) {
	return r.GetSandbox(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = r.client.ContainerRemove(ctx, sb.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return err
}

func (r *DockerSandboxRuntime) StartSandbox(ctx context.Context, sandboxID string, _ *workerclient.WorkerSandboxOperationRequest) (*sandbox.Sandbox, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if sb.Status == sandbox.StatusRunning {
		return sb, nil
	}
	if _, err := r.client.ContainerStart(ctx, sb.ID, client.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	return r.GetSandbox(ctx, sandboxID)
}

func (r *DockerSandboxRuntime) StopSandbox(ctx context.Context, sandboxID string, _ *workerclient.WorkerSandboxOperationRequest) (*sandbox.Sandbox, error) {
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

func (r *DockerSandboxRuntime) sandboxFromInspect(inspect container.InspectResponse) *sandbox.Sandbox {
	createdAt, _ := time.Parse(time.RFC3339Nano, inspect.Created)
	sandboxID := inspect.Config.Labels[sandboxLabelSandbox]
	sb := &sandbox.Sandbox{
		ID:        inspect.ID,
		SandboxID: sandboxID,
		Status:    sandbox.StatusCreated,
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
			sb.Status = sandbox.StatusRunning
		case inspect.State.Dead || inspect.State.OOMKilled || inspect.State.Error != "":
			sb.Status = sandbox.StatusFailed
		case inspect.State.Status == "created":
			sb.Status = sandbox.StatusCreated
		default:
			sb.Status = sandbox.StatusStopped
		}
	}
	return sb
}

// MemorySandboxRuntime is a lightweight runtime for tests and non-Docker embeds.
type MemorySandboxRuntime struct {
	mu        sync.Mutex
	sandboxes map[string]*sandbox.Sandbox
}

func NewMemorySandboxRuntime() *MemorySandboxRuntime {
	return &MemorySandboxRuntime{sandboxes: map[string]*sandbox.Sandbox{}}
}

func (r *MemorySandboxRuntime) ListSandboxes(context.Context) ([]*sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*sandbox.Sandbox, 0, len(r.sandboxes))
	for _, sb := range r.sandboxes {
		out = append(out, cloneSandbox(sb))
	}
	return out, nil
}

func (r *MemorySandboxRuntime) CreateSandbox(_ context.Context, req *workerclient.WorkerSandboxCreateRequest) (*sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req == nil {
		return nil, fmt.Errorf("sandbox create request is required")
	}
	now := time.Now().UTC()
	sb := &sandbox.Sandbox{ID: req.SandboxId, SandboxID: req.SandboxId, Status: sandbox.StatusRunning, Image: optString(req.Image), CreatedAt: now, StartedAt: &now, Env: copyMap(map[string]string(optCreateEnv(req.Env)))}
	r.sandboxes[req.SandboxId] = sb
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) GetSandbox(_ context.Context, sandboxID string) (*sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, sandbox.ErrNotFound
	}
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) UpdateSandbox(_ context.Context, sandboxID string, req *workerclient.WorkerSandboxUpdateRequest) (*sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, sandbox.ErrNotFound
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

func (r *MemorySandboxRuntime) StartSandbox(_ context.Context, sandboxID string, _ *workerclient.WorkerSandboxOperationRequest) (*sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, sandbox.ErrNotFound
	}
	if sb.Status == sandbox.StatusRunning {
		return cloneSandbox(sb), nil
	}
	now := time.Now().UTC()
	sb.Status = sandbox.StatusRunning
	sb.StartedAt = &now
	return cloneSandbox(sb), nil
}

func (r *MemorySandboxRuntime) StopSandbox(_ context.Context, sandboxID string, _ *workerclient.WorkerSandboxOperationRequest) (*sandbox.Sandbox, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.sandboxes[sandboxID]
	if sb == nil {
		return nil, sandbox.ErrNotFound
	}
	now := time.Now().UTC()
	sb.Status = sandbox.StatusStopped
	sb.StoppedAt = &now
	return cloneSandbox(sb), nil
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

func cloneSandbox(sb *sandbox.Sandbox) *sandbox.Sandbox {
	if sb == nil {
		return nil
	}
	clone := *sb
	clone.Metadata = copyMap(sb.Metadata)
	clone.Env = copyMap(sb.Env)
	clone.Ports = append([]sandbox.AssignedPort(nil), sb.Ports...)
	return &clone
}
