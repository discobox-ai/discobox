// Package docker implements a VM driver backed by Docker containers.
//
// It is intended for local development and end-to-end tests of VM-backed worker
// pools. Containers are launched with VM-style boot metadata and can run
// systemd as PID 1 when the selected image supports it.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/controlplane"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
	workeragent "github.com/obot-platform/discobox/worker-agent"
	"github.com/obot-platform/discobox/worker-agent/proxyagent"
)

const (
	ProviderType          = "docker"
	defaultImage          = "ghcr.io/obot-platform/discobox-systemd:latest"
	defaultAgentPort      = 3002
	noHealthWaitTimeout   = 30 * time.Second
	healthPollDelay       = 500 * time.Millisecond
	dockerHostGateway     = "host.docker.internal"
	dockerSocketPath      = "/var/run/docker.sock"
	hostMountTargetRoot   = "/host"
	workerHostSandboxRoot = "/var/lib/discobox/projects"
	// workerHostProxyRoot mirrors proxyagent.Root. The worker writes per-sandbox
	// proxy material here through the host-mount prefix; it must reach the host so
	// the daemon can bind-mount that material into sandbox containers.
	workerHostProxyRoot     = "/var/lib/discobox/proxy"
	labelManaged            = "discobox.vm.managed"
	labelInstanceID         = "discobox.vm.instance_id"
	labelProjectID          = "discobox.project_id"
	labelSandboxID          = "discobox.sandbox_id"
	labelWorkerAgent        = "discobox.worker_agent"
	labelWorkerID           = "discobox.worker_id"
	labelWorkerConfig       = "discobox.worker_agent.config_revision"
	labelProviderInstanceID = "discobox.provider_instance_id"
	labelProviderType       = "discobox.provider_type"
)

// DefaultImage returns the default Docker worker image.
func DefaultImage() string { return defaultImage }

// DefaultAgentPort returns the default worker-agent port exposed by Docker workers.
func DefaultAgentPort() int { return defaultAgentPort }

// DriverConfig configures a Docker-backed VM driver.
type DriverConfig struct {
	Host         string
	Image        string
	Network      string
	AgentPort    int
	Systemd      bool
	Privileged   *bool
	CgroupNSMode string
	Command      []string
	DockerSocket string
	HostMounts   []HostMount
	Labels       map[string]string
	HTTPClient   *http.Client
}

// HostMount describes a host path mounted into Docker worker-agent containers.
type HostMount struct {
	Source   string `json:"source,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

func (m HostMount) MarshalJSON() ([]byte, error) {
	mode := "rw"
	if m.ReadOnly {
		mode = "ro"
	}
	return json.Marshal(cleanAbsPath(m.Source) + ":" + mode)
}

func (m *HostMount) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*m = parseHostMount(value)
		return nil
	}
	var object struct {
		Source   string `json:"source"`
		ReadOnly bool   `json:"readOnly"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	*m = HostMount{Source: object.Source, ReadOnly: object.ReadOnly}
	return nil
}

func parseHostMount(value string) HostMount {
	value = strings.TrimSpace(value)
	readOnly := false
	for _, suffix := range []string{":ro", ":rw"} {
		if strings.HasSuffix(value, suffix) {
			readOnly = suffix == ":ro"
			value = strings.TrimSuffix(value, suffix)
			break
		}
	}
	return HostMount{Source: value, ReadOnly: readOnly}
}

// ProviderInstanceConfig is the persisted provider instance configuration.
type ProviderInstanceConfig struct {
	ControlPlaneURL string      `json:"controlPlaneUrl,omitempty"`
	Host            string      `json:"host,omitempty"`
	Image           string      `json:"image,omitempty"`
	Network         string      `json:"network,omitempty"`
	AgentPort       int         `json:"agentPort,omitempty"`
	Systemd         *bool       `json:"systemd,omitempty"`
	Privileged      *bool       `json:"privileged,omitempty"`
	CgroupNSMode    string      `json:"cgroupNsMode,omitempty"`
	Command         []string    `json:"command,omitempty"`
	DockerSocket    string      `json:"bindDockerSocket,omitempty"`
	HostMounts      []HostMount `json:"hostMounts,omitempty"`
	PoolSize        int         `json:"poolSize,omitempty"`
	MinWorkers      int         `json:"minWorkers,omitempty"`
	MaxWorkers      int         `json:"maxWorkers,omitempty"`
	MinHealthy      int         `json:"minHealthyWorkers,omitempty"`
}

// Definition describes the Docker provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "Docker",
		Icon:        "docker",
		Description: "Runs VM-style workers as Docker containers, optionally with systemd as PID 1.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Placeholder: controlplane.DefaultURL(dockerHostGateway, controlplane.DefaultPort), Advanced: true},
			{Key: "host", Label: "Docker Host", Type: "string", Advanced: true},
			{Key: "image", Label: "Image", Type: "string", Placeholder: defaultImage},
			{Key: "network", Label: "Docker Network", Type: "string", Advanced: true},
			{Key: "minWorkers", Label: "Minimum Workers", Type: "number", Placeholder: "1", Description: "Minimum active VM workers to keep in the pool."},
			{Key: "maxWorkers", Label: "Maximum Workers", Type: "number", Placeholder: "2", Description: "Maximum active VM workers allowed in the pool."},
			{Key: "minHealthyWorkers", Label: "Minimum Healthy Workers", Type: "number", Placeholder: "1", Description: "Minimum ready, schedulable, non-degraded workers before launching replacements."},
			{Key: "poolSize", Label: "Pool Size", Type: "number", Placeholder: "1", Description: "Deprecated alias for minimum workers.", Advanced: true},
			{Key: "systemd", Label: "Run systemd", Type: "boolean", Advanced: true},
			{Key: "privileged", Label: "Privileged", Type: "boolean", Advanced: true},
			{Key: "cgroupNsMode", Label: "Cgroup Namespace", Type: "string", Advanced: true},
			{Key: "command", Label: "Command", Type: "string", Advanced: true},
			{Key: "bindDockerSocket", Label: "Bind Docker Socket", Type: "string", Placeholder: dockerSocketPath, Advanced: true},
			{Key: "agentPort", Label: "Agent Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		},
	}
}

// Driver manages VM-style Docker containers.
type Driver struct {
	client       *client.Client
	ownsClient   bool
	image        string
	network      string
	agentPort    int
	systemd      bool
	privileged   bool
	cgroupNSMode string
	command      []string
	dockerSocket string
	hostMounts   []HostMount
	labels       map[string]string

	watcherMu     sync.Mutex
	watcherCancel context.CancelFunc
}

// NewDriver creates a Docker-backed VM driver and verifies Docker API connectivity.
func NewDriver(ctx context.Context, cfg DriverConfig) (*Driver, error) {
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
	d := NewDriverWithClient(cli, cfg)
	d.ownsClient = true
	return d, nil
}

// NewDriverWithClient creates a Docker-backed VM driver from an existing Docker client.
func NewDriverWithClient(cli *client.Client, cfg DriverConfig) *Driver {
	agentPort := cfg.AgentPort
	if agentPort == 0 {
		agentPort = defaultAgentPort
	}
	image := strings.TrimSpace(cfg.Image)
	if image == "" {
		image = defaultImage
	}
	systemd := cfg.Systemd
	privileged := systemd
	if cfg.Privileged != nil {
		privileged = *cfg.Privileged
	}
	command := append([]string(nil), cfg.Command...)
	if len(command) == 0 && systemd {
		command = []string{"/usr/local/bin/discobox-worker-agent"}
	}
	labels := make(map[string]string, len(cfg.Labels)+2)
	for key, value := range cfg.Labels {
		labels[key] = value
	}
	labels[labelProviderType] = ProviderType
	labels[labelManaged] = "true"
	dockerSocket := cleanAbsPath(cfg.DockerSocket)
	if dockerSocket == "" {
		dockerSocket = dockerSocketPath
	}
	hostMounts := normalizeHostMounts(cfg.HostMounts)
	return &Driver{
		client:       cli,
		image:        image,
		network:      cfg.Network,
		agentPort:    agentPort,
		systemd:      systemd,
		privileged:   privileged,
		cgroupNSMode: strings.TrimSpace(cfg.CgroupNSMode),
		command:      command,
		dockerSocket: dockerSocket,
		hostMounts:   hostMounts,
		labels:       labels,
	}
}

// NewProvider creates a generic VM provider backed by Docker containers.
func NewProvider(ctx context.Context, cfg DriverConfig, providerCfg vm.Config) (*vm.Provider, error) {
	driver, err := NewDriver(ctx, cfg)
	if err != nil {
		return nil, err
	}
	providerCfg.Driver = driver
	if providerCfg.Name == "" {
		providerCfg.Name = "Docker"
	}
	if providerCfg.Description == "" {
		providerCfg.Description = "Runs VM-style workers as Docker containers."
	}
	if providerCfg.DefaultImage == "" {
		providerCfg.DefaultImage = driver.image
	}
	if providerCfg.AgentPort == 0 {
		providerCfg.AgentPort = driver.agentPort
	}
	provider, err := vm.New(providerCfg)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return provider, nil
}

func (d *Driver) Close() error {
	if d == nil {
		return nil
	}
	d.watcherMu.Lock()
	cancel := d.watcherCancel
	d.watcherCancel = nil
	d.watcherMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if d.client == nil || !d.ownsClient {
		return nil
	}
	return d.client.Close()
}

func (d *Driver) CreateVM(ctx context.Context, spec vm.InstanceSpec) (*vm.Instance, error) {
	if d == nil || d.client == nil {
		return nil, errors.New("docker client is required")
	}
	derivedControlPlaneURL := controlPlaneURLDefaulted(spec.Boot)
	boot := d.containerBootConfig(spec.Boot)
	spec.Boot = boot
	workerID := strings.TrimSpace(boot.Env[workeragent.EnvWorkerID])
	projectID := strings.TrimSpace(spec.Ref.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(boot.Env[workeragent.EnvProjectID])
	}
	name := containerName(workerID, spec.Name)
	image := strings.TrimSpace(spec.Image)
	if image == "" {
		image = d.image
	}
	labels := d.containerLabels(spec)
	workerAgent := labels[labelWorkerAgent] == "true"
	if workerAgent && d.usesHostMountPrefix() {
		env := make(map[string]string, len(boot.Env)+1)
		for key, value := range boot.Env {
			env[key] = value
		}
		env[workeragent.EnvHostMountPrefix] = hostMountTargetRoot
		boot.Env = env
	}
	if existing, err := d.client.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil {
		if shouldRemoveExistingContainer(existing.Container, image, labels, workerAgent) {
			if _, err := d.client.ContainerRemove(ctx, existing.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				return nil, err
			}
		} else {
			return d.instanceFromHealthyInspect(ctx, existing.Container.ID, false)
		}
	} else if !cerrdefs.IsNotFound(err) {
		return nil, err
	}

	if existing, err := d.client.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil {
		return d.instanceFromInspect(existing.Container), nil
	} else if !cerrdefs.IsNotFound(err) {
		return nil, err
	}

	exposedPort, ok := agentNetworkPort(d.agentPort)
	if !ok {
		return nil, fmt.Errorf("invalid agent port %d", d.agentPort)
	}
	config := &container.Config{
		Image:        image,
		Labels:       labels,
		Env:          envList(boot.Env),
		Cmd:          d.command,
		ExposedPorts: network.PortSet{exposedPort: struct{}{}},
	}
	hostConfig := &container.HostConfig{
		Privileged: d.privileged,
		PortBindings: network.PortMap{
			exposedPort: []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}},
		},
	}
	hostConfig.ExtraHosts = append(hostConfig.ExtraHosts, controlPlaneExtraHosts(derivedControlPlaneURL, boot.Env[workeragent.EnvControlPlaneURL])...)
	if d.cgroupNSMode != "" {
		hostConfig.CgroupnsMode = container.CgroupnsMode(d.cgroupNSMode)
	} else if d.systemd {
		// systemd (PID 1 in the worker) must create its own cgroup subtree. A
		// private cgroup namespace makes Docker mount a writable cgroup2 hierarchy
		// delegated to the container; bind-mounting the host /sys/fs/cgroup instead
		// drops the container onto the read-only host cgroup root and systemd exits
		// 255 before it can even log.
		hostConfig.CgroupnsMode = container.CgroupnsMode("private")
	}
	hostConfig.Mounts = d.containerMounts(workerAgent, workerID, projectID)
	if d.systemd {
		hostConfig.Tmpfs = map[string]string{"/run": "rw,noexec,nosuid,size=64m", "/run/lock": "rw,noexec,nosuid,size=64m", "/tmp": "rw,size=64m"}
	}
	if spec.Resources.MemoryMB > 0 {
		hostConfig.Memory = int64(spec.Resources.MemoryMB) * 1024 * 1024
	}
	if spec.Resources.CPUCores > 0 {
		hostConfig.NanoCPUs = int64(spec.Resources.CPUCores * 1_000_000_000)
	}
	networkConfig := &network.NetworkingConfig{}
	if d.network != "" {
		networkConfig.EndpointsConfig = map[string]*network.EndpointSettings{d.network: {}}
	}
	// Worker-agent VMs run the shared proxy that their sandboxes route through.
	// Create the per-worker internal network so the worker can be aliased as the
	// proxy server name on it and sandboxes can reach only the proxy.
	if workerAgent && d.dockerSocket != "" {
		if err := d.ensureSandboxNetwork(ctx, workerID); err != nil {
			return nil, err
		}
	}
	created, err := d.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: config, HostConfig: hostConfig, NetworkingConfig: networkConfig, Name: name})
	if err != nil {
		return nil, err
	}
	if workerAgent && d.dockerSocket != "" {
		if _, err := d.client.NetworkConnect(ctx, proxyagent.SandboxNetworkName(workerID), client.NetworkConnectOptions{
			Container:      created.ID,
			EndpointConfig: &network.EndpointSettings{Aliases: []string{proxyagent.ServerName}},
		}); err != nil {
			return nil, fmt.Errorf("connect worker to sandbox network: %w", err)
		}
	}
	if _, err := d.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	return d.instanceFromHealthyInspect(ctx, created.ID, true)
}

func shouldRemoveExistingContainer(existing container.InspectResponse, desiredImage string, desiredLabels map[string]string, workerAgent bool) bool {
	if existing.Config != nil && strings.TrimSpace(desiredImage) != "" && existing.Config.Image != desiredImage {
		return true
	}
	if !workerAgent || existing.Config == nil {
		return false
	}
	for key, value := range desiredLabels {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if existing.Config.Labels[key] != value {
			return true
		}
	}
	return false
}

func (d *Driver) instanceFromHealthyInspect(ctx context.Context, id string, wait bool) (*vm.Instance, error) {
	inspect, err := d.inspectHealthy(ctx, id, wait)
	if err != nil {
		return nil, err
	}
	return d.instanceFromInspect(*inspect), nil
}

func (d *Driver) inspectHealthy(ctx context.Context, id string, wait bool) (*container.InspectResponse, error) {
	noHealthDeadline := time.Now().Add(noHealthWaitTimeout)
	for {
		inspect, err := d.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return nil, mapDockerNotFound(err)
		}
		err = containerReadyError(inspect.Container)
		if err == nil {
			if !wait || containerHasHealth(inspect.Container) || time.Now().After(noHealthDeadline) {
				return &inspect.Container, nil
			}
		} else if !wait || !containerHealthStarting(inspect.Container) {
			return &inspect.Container, err
		}

		timer := time.NewTimer(healthPollDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (d *Driver) containerBootConfig(boot vm.BootConfig) vm.BootConfig {
	if strings.TrimSpace(boot.Env[workeragent.EnvControlPlaneURL]) != "" {
		return boot
	}
	env := make(map[string]string, len(boot.Env)+1)
	for key, value := range boot.Env {
		env[key] = value
	}
	env[workeragent.EnvControlPlaneURL] = defaultDockerControlPlaneURL()
	boot.Env = env
	return boot
}

func defaultDockerControlPlaneURL() string {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = strconv.Itoa(controlplane.DefaultPort)
	}
	return "http://" + dockerHostGateway + ":" + port
}

func controlPlaneURLUsesHostGateway(value string) bool {
	return strings.Contains(value, "://"+dockerHostGateway) || strings.HasPrefix(value, dockerHostGateway+":")
}

func controlPlaneURLDefaulted(boot vm.BootConfig) bool {
	return strings.TrimSpace(boot.Env[workeragent.EnvControlPlaneURL]) == ""
}

func controlPlaneExtraHosts(defaulted bool, controlPlaneURL string) []string {
	if defaulted && controlPlaneURLUsesHostGateway(controlPlaneURL) {
		return []string{dockerHostGateway + ":host-gateway"}
	}
	return nil
}

func (d *Driver) containerMounts(workerAgent bool, workerID, projectID string) []mount.Mount {
	var mounts []mount.Mount
	if workerAgent {
		if d.dockerSocket != "" {
			mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: d.dockerSocket, Target: dockerSocketPath})
			if !hasHostMountSource(d.hostMounts, workerHostSandboxRoot) {
				mounts = append(mounts, mount.Mount{
					Type:        mount.TypeBind,
					Source:      workerHostSandboxRoot,
					Target:      hostMountTarget(workerHostSandboxRoot),
					BindOptions: &mount.BindOptions{CreateMountpoint: true},
				})
			}
			if !hasHostMountSource(d.hostMounts, workerHostProxyRoot) {
				mounts = append(mounts, mount.Mount{
					Type:        mount.TypeBind,
					Source:      workerHostProxyRoot,
					Target:      hostMountTarget(workerHostProxyRoot),
					BindOptions: &mount.BindOptions{CreateMountpoint: true},
				})
			}
		}
		for _, hostMount := range d.hostMounts {
			mounts = append(mounts, mount.Mount{
				Type:     mount.TypeBind,
				Source:   hostMount.Source,
				Target:   hostMountTarget(hostMount.Source),
				ReadOnly: hostMount.ReadOnly,
			})
		}
	}
	if d.systemd {
		// Do not bind-mount the host /sys/fs/cgroup here: with a private cgroup
		// namespace Docker mounts a writable cgroup2 hierarchy for the container,
		// which systemd requires. The host bind mount would shadow it with the
		// read-only host cgroup root.
		mounts = append(mounts,
			mount.Mount{Type: mount.TypeVolume, Source: workerScopedVolumeName(workerID, "docker"), Target: "/var/lib/docker"},
			mount.Mount{Type: mount.TypeVolume, Source: projectScopedVolumeName(projectID, "discobox"), Target: "/var/lib/discobox"},
		)
	}
	return mounts
}

func (d *Driver) usesHostMountPrefix() bool {
	return d.dockerSocket != "" || len(d.hostMounts) > 0
}

func hasHostMountSource(hostMounts []HostMount, source string) bool {
	source = cleanAbsPath(source)
	for _, hostMount := range hostMounts {
		if hostMount.Source == source {
			return true
		}
	}
	return false
}

func normalizeHostMounts(hostMounts []HostMount) []HostMount {
	out := make([]HostMount, 0, len(hostMounts))
	seen := map[string]struct{}{}
	for _, hostMount := range hostMounts {
		source := cleanAbsPath(hostMount.Source)
		if source == "" {
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, HostMount{Source: source, ReadOnly: hostMount.ReadOnly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func cleanAbsPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasPrefix(path, "/") {
		return ""
	}
	parts := make([]string, 0, strings.Count(path, "/")+1)
	for _, part := range strings.Split(path, "/") {
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
		return "/"
	}
	return "/" + strings.Join(parts, "/")
}

func hostMountTarget(source string) string {
	source = cleanAbsPath(source)
	source = strings.TrimPrefix(source, "/")
	if source == "" {
		return hostMountTargetRoot
	}
	return hostMountTargetRoot + "/" + source
}

func (d *Driver) StartVM(ctx context.Context, id string) (*vm.Instance, error) {
	inspect, err := d.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, mapDockerNotFound(err)
	}
	if inspect.Container.State == nil || !inspect.Container.State.Running {
		if _, err := d.client.ContainerStart(ctx, inspect.Container.ID, client.ContainerStartOptions{}); err != nil {
			return nil, err
		}
	}
	inspect, err = d.client.ContainerInspect(ctx, inspect.Container.ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return d.instanceFromInspect(inspect.Container), nil
}

func (d *Driver) StopVM(ctx context.Context, id string, timeout time.Duration) (*vm.Instance, error) {
	inspect, err := d.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, mapDockerNotFound(err)
	}
	seconds := int(timeout.Seconds())
	if seconds < 0 {
		seconds = 0
	}
	if inspect.Container.State != nil && inspect.Container.State.Running {
		if _, err := d.client.ContainerStop(ctx, inspect.Container.ID, client.ContainerStopOptions{Timeout: &seconds}); err != nil {
			return nil, err
		}
	}
	inspect, err = d.client.ContainerInspect(ctx, inspect.Container.ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return d.instanceFromInspect(inspect.Container), nil
}

func (d *Driver) DeleteVM(ctx context.Context, id string, removeVolumes bool) error {
	_, err := d.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: removeVolumes})
	return mapDockerNotFound(err)
}

func (d *Driver) InspectVM(ctx context.Context, id string) (*vm.Instance, error) {
	inspect, err := d.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, mapDockerNotFound(err)
	}
	return d.instanceFromInspect(inspect.Container), nil
}

func (d *Driver) ListWorkerVMs(ctx context.Context, providerID string) ([]vm.Instance, error) {
	filters := make(client.Filters).
		Add("label", labelManaged+"=true").
		Add("label", labelWorkerAgent+"=true").
		Add("label", labelProviderInstanceID+"="+providerID)
	summaries, err := d.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	instances := make([]vm.Instance, 0, len(summaries.Items))
	for _, summary := range summaries.Items {
		inspect, err := d.client.ContainerInspect(ctx, summary.ID, client.ContainerInspectOptions{})
		if err != nil {
			if errors.Is(mapDockerNotFound(err), sandbox.ErrNotFound) {
				continue
			}
			return nil, err
		}
		instances = append(instances, *d.instanceFromInspect(inspect.Container))
	}
	return instances, nil
}

func (d *Driver) InspectWorkerVM(ctx context.Context, workerID string) (*vm.Instance, error) {
	if strings.TrimSpace(workerID) == "" {
		return nil, sandbox.ErrNotFound
	}
	inspect, err := d.client.ContainerInspect(ctx, containerName(workerID, ""), client.ContainerInspectOptions{})
	if err != nil {
		return nil, mapDockerNotFound(err)
	}
	return d.instanceFromInspect(inspect.Container), nil
}

func (d *Driver) RemoveWorkerVM(ctx context.Context, workerID string, currentInstanceID string, removeVolumes bool) error {
	instanceID := strings.TrimSpace(currentInstanceID)
	if instanceID == "" {
		inst, err := d.InspectWorkerVM(ctx, workerID)
		if err != nil {
			if errors.Is(err, sandbox.ErrNotFound) {
				return nil
			}
			return err
		}
		instanceID = inst.ID
	}
	if instanceID == "" {
		return nil
	}
	if err := d.DeleteVM(ctx, instanceID, removeVolumes); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return err
	}
	// Best-effort remove the per-worker sandbox network once the worker (and its
	// sandboxes) are gone. It fails harmlessly if sandboxes are still attached.
	if d.dockerSocket != "" {
		_, _ = d.client.NetworkRemove(ctx, proxyagent.SandboxNetworkName(workerID), client.NetworkRemoveOptions{})
	}
	return nil
}

// ensureSandboxNetwork creates the per-worker internal bridge network if absent.
func (d *Driver) ensureSandboxNetwork(ctx context.Context, workerID string) error {
	name := proxyagent.SandboxNetworkName(workerID)
	if _, err := d.client.NetworkInspect(ctx, name, client.NetworkInspectOptions{}); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}
	_, err := d.client.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   map[string]string{labelManaged: "true", labelWorkerID: workerID},
	})
	if err != nil && !cerrdefs.IsConflict(err) && !cerrdefs.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (d *Driver) RepairWorkerVM(ctx context.Context, workerID string, currentInstanceID string, spec vm.InstanceSpec, _ string) (*vm.Instance, error) {
	instanceID := strings.TrimSpace(currentInstanceID)
	if instanceID == "" {
		inst, err := d.InspectWorkerVM(ctx, workerID)
		if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return nil, err
		}
		if inst != nil {
			instanceID = inst.ID
		}
	}
	if instanceID != "" {
		if err := d.DeleteVM(ctx, instanceID, true); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return nil, err
		}
	}
	return d.CreateVM(ctx, spec)
}

func (d *Driver) AcquireHTTPClient(_ context.Context, inst *vm.Instance) (*transport.HTTPClientLease, error) {
	baseURL := ""
	if inst != nil {
		baseURL = inst.AgentURL
	}
	return vm.NewDirectHTTPClientLeaseForBaseURL(baseURL), nil
}

func (d *Driver) AcquireWorkerHTTPClient(ctx context.Context, workerID string) (*transport.HTTPClientLease, error) {
	if strings.TrimSpace(workerID) == "" {
		return nil, fmt.Errorf("worker ID is required")
	}
	inspect, err := d.client.ContainerInspect(ctx, containerName(workerID, ""), client.ContainerInspectOptions{})
	if err != nil {
		return nil, mapDockerNotFound(err)
	}
	inst := d.instanceFromInspect(inspect.Container)
	if strings.TrimSpace(inst.AgentURL) == "" {
		return nil, fmt.Errorf("worker %q does not expose an agent URL", workerID)
	}
	return vm.NewDirectHTTPClientLeaseForBaseURL(inst.AgentURL), nil
}

func (d *Driver) containerLabels(spec vm.InstanceSpec) map[string]string {
	labels := make(map[string]string, len(d.labels)+len(spec.Metadata)+3)
	for key, value := range d.labels {
		labels[key] = value
	}
	for key, value := range spec.Metadata {
		labels[key] = value
	}
	labels[labelInstanceID] = spec.Name
	labels[labelProjectID] = spec.Ref.ProjectID
	labels[labelSandboxID] = spec.Ref.SandboxID
	labels[labelWorkerID] = strings.TrimSpace(spec.Boot.Env[workeragent.EnvWorkerID])
	if labels[labelWorkerAgent] == "true" {
		delete(labels, labelSandboxID)
	}
	return labels
}

func (d *Driver) instanceFromInspect(inspect container.InspectResponse) *vm.Instance {
	createdAt, _ := time.Parse(time.RFC3339Nano, inspect.Created)
	inst := &vm.Instance{
		ID:        inspect.ID,
		Name:      strings.TrimPrefix(inspect.Name, "/"),
		Image:     inspect.Config.Image,
		Status:    sandbox.StatusCreated,
		Metadata:  copyStringMap(inspect.Config.Labels),
		CreatedAt: createdAt,
	}
	if inspect.State != nil {
		inst.Error = inspect.State.Error
		if started, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt); err == nil && !started.IsZero() {
			inst.StartedAt = &started
		}
		if stopped, err := time.Parse(time.RFC3339Nano, inspect.State.FinishedAt); err == nil && !stopped.IsZero() {
			inst.StoppedAt = &stopped
		}
		switch {
		case inspect.State.Running:
			inst.Status = sandbox.StatusRunning
		case inspect.State.Dead || inspect.State.OOMKilled || inspect.State.Error != "":
			inst.Status = sandbox.StatusFailed
		case inspect.State.Status == "created":
			inst.Status = sandbox.StatusCreated
		default:
			inst.Status = sandbox.StatusStopped
		}
	}
	if host, port := assignedAgentEndpoint(inspect.NetworkSettings.Ports, d.agentPort); host != "" && port > 0 {
		inst.AgentHost = host
		inst.AgentURL = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	}
	return inst
}

func containerReadyError(inspect container.InspectResponse) error {
	if inspect.State == nil {
		return fmt.Errorf("container %s has no runtime state", shortContainerID(inspect.ID))
	}
	if !inspect.State.Running {
		message := strings.TrimSpace(inspect.State.Error)
		if message == "" {
			message = fmt.Sprintf("container %s is %s", shortContainerID(inspect.ID), inspect.State.Status)
		}
		return errors.New(message)
	}
	if inspect.State.Health == nil {
		return nil
	}
	switch inspect.State.Health.Status {
	case "healthy":
		return nil
	case "starting":
		return fmt.Errorf("container %s health check is starting", shortContainerID(inspect.ID))
	case "unhealthy":
		return fmt.Errorf("container %s is unhealthy%s", shortContainerID(inspect.ID), healthLogSuffix(inspect.State.Health))
	default:
		return fmt.Errorf("container %s health check status is %q", shortContainerID(inspect.ID), inspect.State.Health.Status)
	}
}

func containerHealthStarting(inspect container.InspectResponse) bool {
	return inspect.State != nil && inspect.State.Running && inspect.State.Health != nil && inspect.State.Health.Status == "starting"
}

func containerHasHealth(inspect container.InspectResponse) bool {
	return inspect.State != nil && inspect.State.Health != nil
}

func healthLogSuffix(health *container.Health) string {
	if health == nil || len(health.Log) == 0 {
		return ""
	}
	output := strings.TrimSpace(health.Log[len(health.Log)-1].Output)
	if output == "" {
		return ""
	}
	if len(output) > 512 {
		output = output[:512] + "..."
	}
	return ": " + output
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func assignedAgentEndpoint(ports network.PortMap, agentPort int) (string, int) {
	port, ok := agentNetworkPort(agentPort)
	if !ok {
		return "", 0
	}
	bindings := ports[port]
	if len(bindings) == 0 {
		return "", 0
	}
	host := bindings[0].HostIP.String()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	hostPort, _ := strconv.Atoi(bindings[0].HostPort)
	return host, hostPort
}

func agentNetworkPort(agentPort int) (network.Port, bool) {
	if agentPort <= 0 || agentPort > 65535 {
		return network.Port{}, false
	}
	return network.PortFrom(uint16(agentPort), network.TCP)
}

func mapDockerNotFound(err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return sandbox.ErrNotFound
	}
	return err
}

var invalidContainerName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func containerName(workerID, name string) string {
	if workerID != "" {
		name = workerID
	}
	name = invalidContainerName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "vm"
	}
	return "discobox-vm-" + name
}

func workerScopedVolumeName(workerID, suffix string) string {
	return scopedVolumeName("worker", workerID, suffix)
}

func projectScopedVolumeName(projectID, suffix string) string {
	return scopedVolumeName("project", projectID, suffix)
}

func scopedVolumeName(scope, id, suffix string) string {
	name := id
	if strings.TrimSpace(name) == "" {
		name = "unknown"
	}
	name = invalidContainerName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "unknown"
	}
	return "discobox-" + scope + "-" + name + "-" + suffix
}

func envList(values map[string]string) []string {
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env
}

func copyStringMap(values map[string]string) map[string]string {
	copied := make(map[string]string, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}
