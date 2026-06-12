// Package docker implements a VM driver backed by Docker containers.
//
// It is intended for local development and end-to-end tests of VM-backed warm
// worker pools. Containers are launched with VM-style boot metadata and can run
// systemd as PID 1 when the selected image supports it.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/internal/sandbox"
	"github.com/obot-platform/discobox/internal/sandbox/vm"
)

const (
	ProviderType      = "docker"
	defaultImage      = "ghcr.io/obot-platform/discobox-systemd:latest"
	defaultAgentPort  = 3002
	labelManaged      = "discobox.vm.managed"
	labelInstanceID   = "discobox.vm.instance_id"
	labelProjectID    = "discobox.project_id"
	labelTenantID     = "discobox.tenant_id"
	labelSandboxID    = "discobox.sandbox_id"
	labelProviderType = "discobox.provider_type"
)

// Config configures a Docker-backed VM driver.
type Config struct {
	Host         string
	Image        string
	Network      string
	AgentPort    int
	Systemd      bool
	Privileged   *bool
	CgroupNSMode string
	Command      []string
	Labels       map[string]string
	HTTPClient   *http.Client
}

// ProviderInstanceConfig is the persisted provider instance configuration.
type ProviderInstanceConfig struct {
	ControlPlaneURL string   `json:"controlPlaneUrl,omitempty"`
	Host            string   `json:"host,omitempty"`
	Image           string   `json:"image,omitempty"`
	Network         string   `json:"network,omitempty"`
	AgentPort       int      `json:"agentPort,omitempty"`
	Systemd         *bool    `json:"systemd,omitempty"`
	Privileged      *bool    `json:"privileged,omitempty"`
	CgroupNSMode    string   `json:"cgroupNsMode,omitempty"`
	Command         []string `json:"command,omitempty"`
	PoolSize        int      `json:"poolSize,omitempty"`
	MinWorkers      int      `json:"minWorkers,omitempty"`
	MaxWorkers      int      `json:"maxWorkers,omitempty"`
	MinHealthy      int      `json:"minHealthyWorkers,omitempty"`
}

// Definition describes the Docker provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "Docker",
		Icon:        "docker",
		Description: "Runs VM-style warm workers as Docker containers, optionally with systemd as PID 1.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Required: true, Placeholder: "http://host.docker.internal:8080"},
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
	labels       map[string]string
}

// NewDriver creates a Docker-backed VM driver and verifies Docker API connectivity.
func NewDriver(ctx context.Context, cfg Config) (*Driver, error) {
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
func NewDriverWithClient(cli *client.Client, cfg Config) *Driver {
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
		command = []string{"/sbin/init"}
	}
	labels := make(map[string]string, len(cfg.Labels)+2)
	for key, value := range cfg.Labels {
		labels[key] = value
	}
	labels[labelProviderType] = ProviderType
	labels[labelManaged] = "true"
	return &Driver{
		client:       cli,
		image:        image,
		network:      cfg.Network,
		agentPort:    agentPort,
		systemd:      systemd,
		privileged:   privileged,
		cgroupNSMode: strings.TrimSpace(cfg.CgroupNSMode),
		command:      command,
		labels:       labels,
	}
}

// NewProvider creates a generic VM provider backed by Docker containers.
func NewProvider(ctx context.Context, cfg Config, providerCfg vm.Config) (*vm.Provider, error) {
	driver, err := NewDriver(ctx, cfg)
	if err != nil {
		return nil, err
	}
	providerCfg.Driver = driver
	if providerCfg.Name == "" {
		providerCfg.Name = "Docker"
	}
	if providerCfg.Description == "" {
		providerCfg.Description = "Runs VM-style warm workers as Docker containers."
	}
	if providerCfg.DefaultImage == "" {
		providerCfg.DefaultImage = driver.image
	}
	if providerCfg.AgentPort == 0 {
		providerCfg.AgentPort = driver.agentPort
	}
	return vm.New(providerCfg)
}

func (d *Driver) CreateVM(ctx context.Context, spec vm.InstanceSpec) (*vm.Instance, error) {
	if d == nil || d.client == nil {
		return nil, errors.New("docker client is required")
	}
	name := containerName(spec.Name)
	if existing, err := d.client.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil {
		image := strings.TrimSpace(spec.Image)
		if image == "" {
			image = d.image
		}
		if existing.Container.Config != nil && existing.Container.Config.Image != image {
			if _, err := d.client.ContainerRemove(ctx, existing.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
				return nil, err
			}
		} else {
			return d.instanceFromInspect(existing.Container), nil
		}
	} else if !cerrdefs.IsNotFound(err) {
		return nil, err
	}

	if existing, err := d.client.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); err == nil {
		return d.instanceFromInspect(existing.Container), nil
	} else if !cerrdefs.IsNotFound(err) {
		return nil, err
	}

	image := strings.TrimSpace(spec.Image)
	if image == "" {
		image = d.image
	}
	exposedPort, ok := network.PortFrom(uint16(d.agentPort), network.TCP)
	if !ok {
		return nil, fmt.Errorf("invalid agent port %d", d.agentPort)
	}
	labels := d.containerLabels(spec)
	config := &container.Config{
		Image:        image,
		Labels:       labels,
		Env:          envList(spec.Boot.Env),
		Cmd:          d.command,
		ExposedPorts: network.PortSet{exposedPort: struct{}{}},
	}
	hostConfig := &container.HostConfig{
		Privileged: d.privileged,
		PortBindings: network.PortMap{
			exposedPort: []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}},
		},
	}
	if d.cgroupNSMode != "" {
		hostConfig.CgroupnsMode = container.CgroupnsMode(d.cgroupNSMode)
	}
	if d.systemd {
		hostConfig.Tmpfs = map[string]string{"/run": "rw,noexec,nosuid,size=64m", "/run/lock": "rw,noexec,nosuid,size=64m", "/tmp": "rw,size=64m"}
		hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/sys/fs/cgroup", Target: "/sys/fs/cgroup", ReadOnly: false})
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
	created, err := d.client.ContainerCreate(ctx, client.ContainerCreateOptions{Config: config, HostConfig: hostConfig, NetworkingConfig: networkConfig, Name: name})
	if err != nil {
		return nil, err
	}
	if _, err := d.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	inspect, err := d.client.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return d.instanceFromInspect(inspect.Container), nil
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

func (d *Driver) AcquireHTTPClient(context.Context, *vm.Instance) (*sandbox.HTTPClientLease, error) {
	return vm.NewDirectHTTPClientLease(), nil
}

func (d *Driver) containerLabels(spec vm.InstanceSpec) map[string]string {
	labels := make(map[string]string, len(d.labels)+len(spec.Metadata)+4)
	for key, value := range d.labels {
		labels[key] = value
	}
	for key, value := range spec.Metadata {
		labels[key] = value
	}
	labels[labelInstanceID] = spec.Name
	labels[labelTenantID] = spec.Ref.TenantID
	labels[labelProjectID] = spec.Ref.ProjectID
	labels[labelSandboxID] = spec.Ref.SandboxID
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

func assignedAgentEndpoint(ports network.PortMap, agentPort int) (string, int) {
	port, ok := network.PortFrom(uint16(agentPort), network.TCP)
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

func containerName(name string) string {
	name = invalidContainerName.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "vm"
	}
	return "discobox-vm-" + name
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
