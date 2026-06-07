package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/obot-platform/disco2/internal/sandbox"
)

const (
	defaultImage     = "ghcr.io/obot-platform/disco2-sandbox:latest"
	defaultAgentPort = 3002

	labelManaged    = "disco2.managed"
	labelProjectID  = "disco2.project_id"
	labelSandboxID  = "disco2.sandbox_id"
	labelVolumeKind = "disco2.volume_kind"

	volumeKindData  = "data"
	volumeKindCache = "cache"

	defaultDataMountPath      = "/.data"
	defaultCacheMountPath     = "/.data/cache"
	defaultWorkspaceMountPath = "/.workspace"
)

// Config configures a Docker-backed sandbox provider.
type Config struct {
	Host         string
	DefaultImage string
	AgentPort    int
	Network      string
	Labels       map[string]string

	DataMountPath       string
	CacheMountPath      string
	WorkspaceMountPath  string
	DisableProjectCache bool
}

// Provider runs one Docker container per sandbox.
type Provider struct {
	client       *client.Client
	defaultImage string
	agentPort    int
	network      string
	labels       map[string]string

	dataMountPath       string
	cacheMountPath      string
	workspaceMountPath  string
	disableProjectCache bool
}

// New creates a Docker provider and verifies Docker API connectivity.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	opts := []client.Opt{client.FromEnv}
	if cfg.Host != "" {
		opts = append(opts, client.WithHost(cfg.Host))
	}
	cli, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return NewWithClient(cli, cfg), nil
}

// NewWithClient creates a Docker provider from an existing Docker API client.
func NewWithClient(cli *client.Client, cfg Config) *Provider {
	imageName := strings.TrimSpace(cfg.DefaultImage)
	if imageName == "" {
		imageName = defaultImage
	}
	agentPort := cfg.AgentPort
	if agentPort == 0 {
		agentPort = defaultAgentPort
	}
	labels := make(map[string]string, len(cfg.Labels))
	for key, value := range cfg.Labels {
		labels[key] = value
	}
	return &Provider{
		client:              cli,
		defaultImage:        imageName,
		agentPort:           agentPort,
		network:             cfg.Network,
		labels:              labels,
		dataMountPath:       defaultString(cfg.DataMountPath, defaultDataMountPath),
		cacheMountPath:      defaultString(cfg.CacheMountPath, defaultCacheMountPath),
		workspaceMountPath:  defaultString(cfg.WorkspaceMountPath, defaultWorkspaceMountPath),
		disableProjectCache: cfg.DisableProjectCache,
	}
}

func (p *Provider) DefaultImage(context.Context) (sandbox.ImageRef, error) {
	return sandbox.ImageRef{Name: p.defaultImage}, nil
}

func (p *Provider) ImageExists(ctx context.Context, ref sandbox.ImageRef) (bool, error) {
	_, err := p.client.ImageInspect(ctx, ref.Name)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (p *Provider) GetImage(ctx context.Context, ref sandbox.ImageRef) (*sandbox.ImageInfo, error) {
	inspect, err := p.client.ImageInspect(ctx, ref.Name)
	if err == nil {
		return &sandbox.ImageInfo{
			Ref:       ref,
			ID:        inspect.ID,
			Status:    sandbox.ImageStatusAvailable,
			UpdatedAt: time.Now().UTC(),
		}, nil
	}
	if cerrdefs.IsNotFound(err) {
		return &sandbox.ImageInfo{Ref: ref, Status: sandbox.ImageStatusMissing, UpdatedAt: time.Now().UTC()}, nil
	}
	return nil, err
}

func (p *Provider) PullImage(ctx context.Context, ref sandbox.ImageRef) (<-chan sandbox.ImageEvent, error) {
	reader, err := p.client.ImagePull(ctx, ref.Name, client.ImagePullOptions{})
	if err != nil {
		return nil, err
	}
	events := make(chan sandbox.ImageEvent, 16)
	go func() {
		defer close(events)
		defer reader.Close()
		parsePullEvents(ctx, ref, reader, events)
	}()
	return events, nil
}

func (p *Provider) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	name := containerName(ref.SandboxID)
	existing, err := p.client.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err == nil {
		sb := p.sandboxFromInspect(existing.Container)
		return sb, state, sandbox.ErrAlreadyExists
	}
	if !cerrdefs.IsNotFound(err) {
		return nil, state, err
	}

	imageRef := opts.Image
	if imageRef.Name == "" {
		imageRef = sandbox.ImageRef{Name: p.defaultImage}
	}
	labels := p.containerLabels(ref, opts)
	env := envList(opts.Env)
	exposedPort, ok := network.PortFrom(uint16(p.agentPort), network.TCP)
	if !ok {
		return nil, state, fmt.Errorf("invalid agent port %d", p.agentPort)
	}
	mounts, err := p.createMounts(ctx, ref, opts)
	if err != nil {
		return nil, state, err
	}

	config := &container.Config{
		Image:        imageRef.Name,
		Labels:       labels,
		Env:          env,
		WorkingDir:   opts.WorkingDirectory,
		ExposedPorts: network.PortSet{exposedPort: struct{}{}},
	}
	hostConfig := &container.HostConfig{
		PortBindings: network.PortMap{
			exposedPort: []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}},
		},
		Mounts: mounts,
	}
	if opts.Resources.MemoryMB > 0 {
		hostConfig.Memory = int64(opts.Resources.MemoryMB) * 1024 * 1024
	}
	if opts.Resources.CPUCores > 0 {
		hostConfig.NanoCPUs = int64(opts.Resources.CPUCores * 1_000_000_000)
	}
	networkConfig := &network.NetworkingConfig{}
	if p.network != "" {
		networkConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			p.network: {},
		}
	}

	if _, err := p.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           config,
		HostConfig:       hostConfig,
		NetworkingConfig: networkConfig,
		Name:             name,
	}); err != nil {
		return nil, state, err
	}
	created, err := p.Get(ctx, ref, state)
	return created, state, err
}

func (p *Provider) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	current, err := p.client.ContainerInspect(ctx, containerName(ref.SandboxID), client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, state, sandbox.ErrNotFound
		}
		return nil, state, err
	}
	if current.Container.State != nil && current.Container.State.Running {
		return p.sandboxFromInspect(current.Container), state, sandbox.ErrAlreadyRunning
	}
	if _, err := p.client.ContainerStart(ctx, current.Container.ID, client.ContainerStartOptions{}); err != nil {
		return nil, state, err
	}
	started, err := p.Get(ctx, ref, state)
	return started, state, err
}

func (p *Provider) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, timeout time.Duration) (*sandbox.Sandbox, []byte, error) {
	current, err := p.client.ContainerInspect(ctx, containerName(ref.SandboxID), client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, state, sandbox.ErrNotFound
		}
		return nil, state, err
	}
	if current.Container.State == nil || !current.Container.State.Running {
		return p.sandboxFromInspect(current.Container), state, sandbox.ErrNotRunning
	}
	seconds := int(timeout.Seconds())
	if seconds < 0 {
		seconds = 0
	}
	if _, err := p.client.ContainerStop(ctx, current.Container.ID, client.ContainerStopOptions{Timeout: &seconds}); err != nil {
		return nil, state, err
	}
	stopped, err := p.Get(ctx, ref, state)
	return stopped, state, err
}

func (p *Provider) Remove(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts ...sandbox.RemoveOption) ([]byte, error) {
	cfg := sandbox.ParseRemoveOptions(opts)
	var removeErr error
	_, err := p.client.ContainerRemove(ctx, containerName(ref.SandboxID), client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: cfg.RemoveVolumes,
	})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			removeErr = sandbox.ErrNotFound
		} else {
			return state, err
		}
	}
	if cfg.RemoveVolumes {
		if err := p.removeVolume(ctx, dataVolumeName(ref.SandboxID)); err != nil && !cerrdefs.IsNotFound(err) {
			return state, err
		}
	}
	if removeErr != nil {
		return state, removeErr
	}
	return nil, nil
}

func (p *Provider) Get(ctx context.Context, ref sandbox.SandboxRef, _ []byte) (*sandbox.Sandbox, error) {
	inspect, err := p.client.ContainerInspect(ctx, containerName(ref.SandboxID), client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, sandbox.ErrNotFound
		}
		return nil, err
	}
	return p.sandboxFromInspect(inspect.Container), nil
}

func (p *Provider) AcquireHTTPClient(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.HTTPClientLease, error) {
	runtimeSandbox, err := p.Get(ctx, ref, state)
	if err != nil {
		return nil, err
	}
	for _, port := range runtimeSandbox.Ports {
		if port.ContainerPort != p.agentPort {
			continue
		}
		address := net.JoinHostPort(port.HostIP, strconv.Itoa(port.HostPort))
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("default transport is %T, want *http.Transport", http.DefaultTransport)
		}
		base := defaultTransport.Clone()
		base.Proxy = nil
		base.DialContext = (&net.Dialer{}).DialContext
		transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			next := req.Clone(req.Context())
			next.URL.Scheme = "http"
			next.URL.Host = address
			return base.RoundTrip(next)
		})
		return sandbox.NewHTTPClientLease(&http.Client{Transport: transport}, nil), nil
	}
	return nil, sandbox.ErrNotRunning
}

func (p *Provider) List(ctx context.Context) ([]*sandbox.Sandbox, error) {
	args := make(client.Filters).Add("label", labelManaged+"=true")
	containers, err := p.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: args})
	if err != nil {
		return nil, err
	}
	sandboxes := make([]*sandbox.Sandbox, 0, len(containers.Items))
	for _, summary := range containers.Items {
		inspect, err := p.client.ContainerInspect(ctx, summary.ID, client.ContainerInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		sandboxes = append(sandboxes, p.sandboxFromInspect(inspect.Container))
	}
	return sandboxes, nil
}

func (p *Provider) Watch(ctx context.Context) (<-chan sandbox.StateEvent, error) {
	args := make(client.Filters).
		Add("type", string(events.ContainerEventType)).
		Add("label", labelManaged+"=true")
	result := p.client.Events(ctx, client.EventsListOptions{Filters: args})
	out := make(chan sandbox.StateEvent, 32)
	go func() {
		defer close(out)
		for {
			select {
			case msg, ok := <-result.Messages:
				if !ok {
					return
				}
				event, ok := stateEventFromDockerEvent(msg)
				if !ok {
					continue
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			case err, ok := <-result.Err:
				if !ok || err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
					return
				}
				select {
				case out <- sandbox.StateEvent{Status: sandbox.StatusFailed, Timestamp: time.Now().UTC(), Error: err.Error()}:
				case <-ctx.Done():
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (p *Provider) Reconcile(context.Context) error {
	return nil
}

func (p *Provider) RemoveProject(ctx context.Context, projectID string) error {
	args := make(client.Filters).
		Add("label", labelManaged+"=true").
		Add("label", labelProjectID+"="+projectID)
	containers, err := p.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: args})
	if err != nil {
		return err
	}
	for _, summary := range containers.Items {
		if _, err := p.client.ContainerRemove(ctx, summary.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	volumes, err := p.projectVolumes(ctx, projectID)
	if err != nil {
		return err
	}
	for _, name := range volumes {
		if err := p.removeVolume(ctx, name); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (p *Provider) ClearCache(ctx context.Context, projectID string) error {
	args := make(client.Filters).
		Add("label", labelManaged+"=true").
		Add("label", labelProjectID+"="+projectID)
	containers, err := p.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: args})
	if err != nil {
		return err
	}
	for _, summary := range containers.Items {
		if _, err := p.client.ContainerRemove(ctx, summary.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: false}); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	if err := p.removeVolume(ctx, cacheVolumeName(projectID)); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (p *Provider) CurrentImageID(ctx context.Context, ref sandbox.ImageRef) (string, error) {
	info, err := p.GetImage(ctx, ref)
	if err != nil {
		return "", err
	}
	if info.Status != sandbox.ImageStatusAvailable {
		return "", sandbox.ErrNotFound
	}
	return info.ID, nil
}

func (p *Provider) Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "Docker",
		Description: "Runs sandboxes as local Docker containers.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "host", Label: "Docker Host", Type: "string", Advanced: true},
			{Key: "defaultImage", Label: "Default Image", Type: "string"},
			{Key: "network", Label: "Network", Type: "string", Advanced: true},
		},
	}
}

func (p *Provider) Status() sandbox.ProviderStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status := sandbox.ProviderStatus{
		Available:          true,
		State:              "ready",
		SupportsImages:     true,
		SupportsClearCache: !p.disableProjectCache,
		SupportsInspection: false,
		SupportsResources:  false,
	}
	if _, err := p.client.Ping(ctx, client.PingOptions{}); err != nil {
		status.Available = false
		status.State = "unavailable"
		status.Message = err.Error()
	}
	return status
}

func (p *Provider) Close() error {
	return p.client.Close()
}

func (p *Provider) containerLabels(ref sandbox.SandboxRef, opts sandbox.CreateOptions) map[string]string {
	labels := make(map[string]string, len(p.labels)+len(opts.Labels)+4)
	for key, value := range p.labels {
		labels[key] = value
	}
	for key, value := range opts.Labels {
		labels[key] = value
	}
	labels[labelManaged] = "true"
	labels[labelProjectID] = ref.ProjectID
	labels[labelSandboxID] = ref.SandboxID
	return labels
}

func (p *Provider) createMounts(ctx context.Context, ref sandbox.SandboxRef, opts sandbox.CreateOptions) ([]mount.Mount, error) {
	labels := volumeLabels(ref.ProjectID, volumeKindData)
	if _, err := p.client.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:   dataVolumeName(ref.SandboxID),
		Labels: labels,
	}); err != nil {
		return nil, err
	}
	mounts := []mount.Mount{
		{
			Type:   mount.TypeVolume,
			Source: dataVolumeName(ref.SandboxID),
			Target: p.dataMountPath,
		},
	}
	if !p.disableProjectCache {
		if _, err := p.client.VolumeCreate(ctx, client.VolumeCreateOptions{
			Name:   cacheVolumeName(ref.ProjectID),
			Labels: volumeLabels(ref.ProjectID, volumeKindCache),
		}); err != nil {
			return nil, err
		}
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: cacheVolumeName(ref.ProjectID),
			Target: p.cacheMountPath,
		})
	}
	workspacePath := strings.TrimSpace(opts.WorkspacePath)
	if workspacePath == "" && filepath.IsAbs(opts.WorkspaceSource) {
		workspacePath = opts.WorkspaceSource
	}
	if workspacePath != "" {
		source, err := resolveWorkspaceMountSource(workspacePath)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   source,
			Target:   p.workspaceMountPath,
			ReadOnly: true,
		})
	}
	return mounts, nil
}

func (p *Provider) projectVolumes(ctx context.Context, projectID string) ([]string, error) {
	args := make(client.Filters).
		Add("label", labelManaged+"=true").
		Add("label", labelProjectID+"="+projectID)
	volumes, err := p.client.VolumeList(ctx, client.VolumeListOptions{Filters: args})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(volumes.Items))
	for _, item := range volumes.Items {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (p *Provider) removeVolume(ctx context.Context, name string) error {
	_, err := p.client.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	return err
}

func (p *Provider) sandboxFromInspect(inspect container.InspectResponse) *sandbox.Sandbox {
	sb := &sandbox.Sandbox{
		ID:        inspect.ID,
		SandboxID: inspect.Config.Labels[labelSandboxID],
		Image:     inspect.Config.Image,
		Metadata:  copyStringMap(inspect.Config.Labels),
		Env:       envMap(inspect.Config.Env),
		Ports:     assignedPorts(inspect.NetworkSettings.Ports),
	}
	if inspect.Created != "" {
		if created, err := time.Parse(time.RFC3339Nano, inspect.Created); err == nil {
			sb.CreatedAt = created
		}
	}
	if inspect.State == nil {
		sb.Status = sandbox.StatusCreated
		return sb
	}
	sb.Error = inspect.State.Error
	if inspect.State.StartedAt != "" {
		if started, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt); err == nil && !started.IsZero() {
			sb.StartedAt = &started
		}
	}
	if inspect.State.FinishedAt != "" {
		if stopped, err := time.Parse(time.RFC3339Nano, inspect.State.FinishedAt); err == nil && !stopped.IsZero() {
			sb.StoppedAt = &stopped
		}
	}
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
	return sb
}

func stateEventFromDockerEvent(msg events.Message) (sandbox.StateEvent, bool) {
	sandboxID := msg.Actor.Attributes[labelSandboxID]
	if sandboxID == "" {
		return sandbox.StateEvent{}, false
	}
	status, ok := statusFromDockerAction(msg.Action)
	if !ok {
		return sandbox.StateEvent{}, false
	}
	timestamp := time.Now().UTC()
	if msg.TimeNano > 0 {
		timestamp = time.Unix(0, msg.TimeNano).UTC()
	} else if msg.Time > 0 {
		timestamp = time.Unix(msg.Time, 0).UTC()
	}
	return sandbox.StateEvent{
		SandboxID: sandboxID,
		Status:    status,
		Timestamp: timestamp,
	}, true
}

func statusFromDockerAction(action events.Action) (sandbox.Status, bool) {
	switch action {
	case events.ActionCreate:
		return sandbox.StatusCreated, true
	case events.ActionStart, events.ActionRestart, events.ActionUnPause:
		return sandbox.StatusRunning, true
	case events.ActionStop, events.ActionDie, events.ActionPause:
		return sandbox.StatusStopped, true
	case events.ActionOOM, events.ActionKill:
		return sandbox.StatusFailed, true
	case events.ActionDestroy, events.ActionRemove:
		return sandbox.StatusRemoved, true
	default:
		return "", false
	}
}

func parsePullEvents(ctx context.Context, ref sandbox.ImageRef, reader io.Reader, events chan<- sandbox.ImageEvent) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			sendImageEvent(events, sandbox.ImageEvent{Ref: ref, Status: sandbox.ImageStatusFailed, Error: ctx.Err().Error(), Time: time.Now().UTC()})
			return
		default:
		}
		event := decodePullProgress(ref, scanner.Bytes())
		sendImageEvent(events, event)
	}
	if err := scanner.Err(); err != nil {
		sendImageEvent(events, sandbox.ImageEvent{Ref: ref, Status: sandbox.ImageStatusFailed, Error: err.Error(), Time: time.Now().UTC()})
		return
	}
	sendImageEvent(events, sandbox.ImageEvent{Ref: ref, Status: sandbox.ImageStatusAvailable, Time: time.Now().UTC()})
}

type pullProgress struct {
	Status         string `json:"status"`
	Error          string `json:"error"`
	ProgressDetail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
}

func decodePullProgress(ref sandbox.ImageRef, data []byte) sandbox.ImageEvent {
	event := sandbox.ImageEvent{Ref: ref, Status: sandbox.ImageStatusPulling, Time: time.Now().UTC()}
	var progress pullProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		event.Progress = &sandbox.ImageProgress{Message: string(data)}
		return event
	}
	if progress.Error != "" {
		event.Status = sandbox.ImageStatusFailed
		event.Error = progress.Error
		return event
	}
	event.Progress = &sandbox.ImageProgress{
		Message:      progress.Status,
		CurrentBytes: progress.ProgressDetail.Current,
		TotalBytes:   progress.ProgressDetail.Total,
	}
	if progress.ProgressDetail.Total > 0 {
		percent := float64(progress.ProgressDetail.Current) / float64(progress.ProgressDetail.Total) * 100
		event.Progress.Percent = &percent
	}
	return event
}

func sendImageEvent(events chan<- sandbox.ImageEvent, event sandbox.ImageEvent) {
	select {
	case events <- event:
	default:
	}
}

var invalidContainerName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func containerName(sandboxID string) string {
	name := invalidContainerName.ReplaceAllString(sandboxID, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "sandbox"
	}
	return "disco2-sandbox-" + name
}

func envList(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env
}

func envMap(values []string) map[string]string {
	env := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok {
			env[key] = val
		}
	}
	return env
}

func copyStringMap(values map[string]string) map[string]string {
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func volumeLabels(projectID, kind string) map[string]string {
	return map[string]string{
		labelManaged:    "true",
		labelProjectID:  projectID,
		labelVolumeKind: kind,
	}
}

func dataVolumeName(sandboxID string) string {
	return volumeName("data", sandboxID)
}

func cacheVolumeName(projectID string) string {
	return volumeName("cache", projectID)
}

func volumeName(prefix, id string) string {
	name := invalidContainerName.ReplaceAllString(id, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "default"
	}
	return "disco2-" + prefix + "-" + name
}

func resolveWorkspaceMountSource(sourcePath string) (string, error) {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %q is not a directory", abs)
	}
	return abs, nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func assignedPorts(ports network.PortMap) []sandbox.AssignedPort {
	assigned := make([]sandbox.AssignedPort, 0)
	for containerPort, bindings := range ports {
		port, err := strconv.Atoi(containerPort.Port())
		if err != nil {
			continue
		}
		for _, binding := range bindings {
			hostPort, err := strconv.Atoi(binding.HostPort)
			if err != nil {
				continue
			}
			hostIP := binding.HostIP.String()
			if !binding.HostIP.IsValid() || binding.HostIP.IsUnspecified() {
				hostIP = "127.0.0.1"
			}
			assigned = append(assigned, sandbox.AssignedPort{
				ContainerPort: port,
				HostPort:      hostPort,
				HostIP:        hostIP,
				Protocol:      string(containerPort.Proto()),
			})
		}
	}
	return assigned
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
