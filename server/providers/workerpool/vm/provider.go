// Package vm provides a driver-neutral sandbox provider for VM-backed runtimes.
package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	workeragent "github.com/obot-platform/discobox/worker-agent"
)

const defaultAgentPort = 3002

// Driver launches and manages VM instances for sandbox workers.
//
// Implementations can target local or remote backends such as KVM, HCS, Apple
// Virtualization, AWS, Azure, or GCP. The generic Provider owns Disco-specific
// boot metadata and sandbox.Provider adaptation; drivers own VM mechanics.
type Driver interface {
	InitializeWorkerProvider(ctx context.Context, provider *model.SandboxProviderInstance, manager any) error
	Close() error
	CreateVM(ctx context.Context, spec InstanceSpec) (*Instance, error)
	StartVM(ctx context.Context, id string) (*Instance, error)
	StopVM(ctx context.Context, id string, timeout time.Duration) (*Instance, error)
	DeleteVM(ctx context.Context, id string, removeVolumes bool) error
	InspectVM(ctx context.Context, id string) (*Instance, error)
}

// HTTPClientDriver can provide a transport to a VM's sandbox agent.
type HTTPClientDriver interface {
	AcquireHTTPClient(ctx context.Context, instance *Instance) (*transport.HTTPClientLease, error)
}

// WorkerHTTPClientDriver can provide a transport to a warm worker by worker ID.
type WorkerHTTPClientDriver interface {
	AcquireWorkerHTTPClient(ctx context.Context, workerID string) (*transport.HTTPClientLease, error)
}

// BootstrapProvider creates the worker identity/bootstrap tuple passed into a
// VM. The control plane implementation should persist Worker and
// WorkerBootstrapToken rows before returning this data.
type BootstrapProvider interface {
	CreateWorkerBootstrap(ctx context.Context, ref sandbox.SandboxRef, opts sandbox.CreateOptions) (WorkerBootstrap, error)
}

// BootstrapProviderFunc adapts a function into BootstrapProvider.
type BootstrapProviderFunc func(context.Context, sandbox.SandboxRef, sandbox.CreateOptions) (WorkerBootstrap, error)

func (f BootstrapProviderFunc) CreateWorkerBootstrap(ctx context.Context, ref sandbox.SandboxRef, opts sandbox.CreateOptions) (WorkerBootstrap, error) {
	return f(ctx, ref, opts)
}

// Config configures a VM-backed sandbox provider.
type Config struct {
	Driver Driver

	Name         string
	Description  string
	DefaultImage string
	AgentPort    int

	ControlPlaneURL string
	Bootstrap       BootstrapProvider
	Metadata        map[string]string
}

// Provider is a generic VM-backed sandbox provider.
type Provider struct {
	driver          Driver
	name            string
	description     string
	defaultImage    string
	agentPort       int
	controlPlaneURL string
	bootstrap       BootstrapProvider
	metadata        map[string]string
}

// New creates a generic VM sandbox provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Driver == nil {
		return nil, errors.New("vm driver is required")
	}
	agentPort := cfg.AgentPort
	if agentPort == 0 {
		agentPort = defaultAgentPort
	}
	metadata := make(map[string]string, len(cfg.Metadata))
	for key, value := range cfg.Metadata {
		metadata[key] = value
	}
	return &Provider{
		driver:          cfg.Driver,
		name:            defaultString(cfg.Name, "Virtual Machine"),
		description:     defaultString(cfg.Description, "Runs sandboxes as worker agents inside virtual machines."),
		defaultImage:    cfg.DefaultImage,
		agentPort:       agentPort,
		controlPlaneURL: cfg.ControlPlaneURL,
		bootstrap:       cfg.Bootstrap,
		metadata:        metadata,
	}, nil
}

func (p *Provider) Initialize(context.Context, *model.SandboxProviderInstance) error {
	return nil
}

func (p *Provider) Close() error {
	if p == nil {
		return nil
	}
	return p.driver.Close()
}

func (p *Provider) Create(ctx context.Context, ref sandbox.SandboxRef, state []byte, opts sandbox.CreateOptions) (*sandbox.Sandbox, []byte, error) {
	if len(state) > 0 {
		data, err := decodeState(state)
		if err != nil {
			return nil, state, err
		}
		inst, err := p.driver.InspectVM(ctx, data.InstanceID)
		if err != nil {
			return nil, state, err
		}
		return sandboxFromInstance(ref, inst, p.agentPort), state, sandbox.ErrAlreadyExists
	}
	image := opts.Image.Name
	if image == "" {
		image = p.defaultImage
	}
	bootstrap, err := p.workerBootstrap(ctx, ref, opts)
	if err != nil {
		return nil, state, err
	}
	boot := BuildBootConfig(BootInput{
		Ref:             ref,
		Options:         opts,
		WorkerBootstrap: bootstrap,
		ControlPlaneURL: p.controlPlaneURL,
		AgentPort:       p.agentPort,
	})
	inst, err := p.driver.CreateVM(ctx, InstanceSpec{
		Ref:       ref,
		Name:      instanceName(ref),
		Image:     image,
		Resources: opts.Resources,
		Boot:      boot,
		Metadata:  mergeStringMaps(p.metadata, opts.Labels),
	})
	if err != nil {
		return nil, state, err
	}
	providerState, err := encodeState(stateData{InstanceID: inst.ID, Worker: bootstrap})
	if err != nil {
		return nil, state, err
	}
	return sandboxFromInstance(ref, inst, p.agentPort), providerState, nil
}

func (p *Provider) Start(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, []byte, error) {
	data, err := decodeState(state)
	if err != nil {
		return nil, state, err
	}
	inst, err := p.driver.StartVM(ctx, data.InstanceID)
	if err != nil {
		return nil, state, err
	}
	return sandboxFromInstance(ref, inst, p.agentPort), state, nil
}

func (p *Provider) Stop(ctx context.Context, ref sandbox.SandboxRef, state []byte, timeout time.Duration) (*sandbox.Sandbox, []byte, error) {
	data, err := decodeState(state)
	if err != nil {
		return nil, state, err
	}
	inst, err := p.driver.StopVM(ctx, data.InstanceID, timeout)
	if err != nil {
		return nil, state, err
	}
	return sandboxFromInstance(ref, inst, p.agentPort), state, nil
}

func (p *Provider) Remove(ctx context.Context, _ sandbox.SandboxRef, state []byte, opts ...sandbox.RemoveOption) ([]byte, error) {
	data, err := decodeState(state)
	if err != nil {
		return state, err
	}
	cfg := sandbox.ParseRemoveOptions(opts)
	if err := p.driver.DeleteVM(ctx, data.InstanceID, cfg.RemoveVolumes); err != nil {
		return state, err
	}
	return nil, nil
}

func (p *Provider) Get(ctx context.Context, ref sandbox.SandboxRef, state []byte) (*sandbox.Sandbox, error) {
	data, err := decodeState(state)
	if err != nil {
		return nil, err
	}
	inst, err := p.driver.InspectVM(ctx, data.InstanceID)
	if err != nil {
		return nil, err
	}
	return sandboxFromInstance(ref, inst, p.agentPort), nil
}

func (p *Provider) List(context.Context) ([]*sandbox.Sandbox, error) {
	return nil, nil
}

func (p *Provider) AcquireHTTPClient(ctx context.Context, _ sandbox.SandboxRef, state []byte) (*transport.HTTPClientLease, error) {
	clientDriver, ok := p.driver.(HTTPClientDriver)
	if !ok {
		return nil, errors.New("vm driver does not provide sandbox agent HTTP access")
	}
	data, err := decodeState(state)
	if err != nil {
		return nil, err
	}
	inst, err := p.driver.InspectVM(ctx, data.InstanceID)
	if err != nil {
		return nil, err
	}
	return clientDriver.AcquireHTTPClient(ctx, inst)
}

func (p *Provider) AcquireWorkerHTTPClientForID(ctx context.Context, workerID string) (*transport.HTTPClientLease, error) {
	workerDriver, ok := p.driver.(WorkerHTTPClientDriver)
	if ok {
		return workerDriver.AcquireWorkerHTTPClient(ctx, workerID)
	}
	return nil, errors.New("vm driver does not provide worker HTTP access")
}

func (p *Provider) DefaultImage(context.Context) (sandbox.ImageRef, error) {
	return sandbox.ImageRef{Name: p.defaultImage}, nil
}

func (p *Provider) ImageExists(context.Context, sandbox.ImageRef) (bool, error) {
	return true, nil
}

func (p *Provider) GetImage(_ context.Context, ref sandbox.ImageRef) (*sandbox.ImageInfo, error) {
	return &sandbox.ImageInfo{Ref: ref, Status: sandbox.ImageStatusAvailable, UpdatedAt: time.Now().UTC()}, nil
}

func (p *Provider) PullImage(_ context.Context, ref sandbox.ImageRef) (<-chan sandbox.ImageEvent, error) {
	events := make(chan sandbox.ImageEvent, 1)
	events <- sandbox.ImageEvent{Ref: ref, Status: sandbox.ImageStatusAvailable, Time: time.Now().UTC()}
	close(events)
	return events, nil
}

func (p *Provider) Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{Name: p.name, Description: p.description}
}

func (p *Provider) Status() sandbox.ProviderStatus {
	return sandbox.ProviderStatus{Available: true, State: "ready", SupportsImages: true}
}

func (p *Provider) workerBootstrap(ctx context.Context, ref sandbox.SandboxRef, opts sandbox.CreateOptions) (WorkerBootstrap, error) {
	if p.bootstrap == nil {
		return WorkerBootstrap{}, nil
	}
	bootstrap, err := p.bootstrap.CreateWorkerBootstrap(ctx, ref, opts)
	if err != nil {
		return WorkerBootstrap{}, err
	}
	if bootstrap.ProjectID == "" {
		bootstrap.ProjectID = ref.ProjectID
	}
	if bootstrap.SandboxID == "" {
		bootstrap.SandboxID = ref.SandboxID
	}
	return bootstrap, nil
}

func decodeState(state []byte) (stateData, error) {
	if len(state) == 0 {
		return stateData{}, sandbox.ErrNotFound
	}
	var data stateData
	if err := json.Unmarshal(state, &data); err != nil {
		return stateData{}, err
	}
	if data.InstanceID == "" {
		return stateData{}, sandbox.ErrNotFound
	}
	return data, nil
}

func encodeState(data stateData) ([]byte, error) {
	return json.Marshal(data)
}

func sandboxFromInstance(ref sandbox.SandboxRef, inst *Instance, agentPort int) *sandbox.Sandbox {
	if inst == nil {
		return nil
	}
	metadata := make(map[string]string, len(inst.Metadata)+2)
	for key, value := range inst.Metadata {
		metadata[key] = value
	}
	if inst.AgentURL != "" {
		metadata["agent_url"] = inst.AgentURL
	}
	return &sandbox.Sandbox{
		ID:        inst.ID,
		SandboxID: ref.SandboxID,
		Status:    inst.Status,
		Image:     inst.Image,
		CreatedAt: inst.CreatedAt,
		StartedAt: inst.StartedAt,
		StoppedAt: inst.StoppedAt,
		Error:     inst.Error,
		Metadata:  metadata,
		Ports: []sandbox.AssignedPort{{
			ContainerPort: agentPort,
			HostPort:      agentPort,
			HostIP:        inst.AgentHost,
			Protocol:      "tcp",
		}},
	}
}

func instanceName(ref sandbox.SandboxRef) string {
	parts := []string{"discobox", ref.ProjectID, ref.SandboxID}
	return strings.ReplaceAll(strings.Join(parts, "-"), "_", "-")
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(overlay))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range overlay {
		merged[key] = value
	}
	return merged
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// NewDirectHTTPClientLease returns a direct HTTP client for drivers that expose
// sandbox agents through ordinary network addresses.
func NewDirectHTTPClientLease() *transport.HTTPClientLease {
	return transport.NewHTTPClientLease(http.DefaultClient, nil)
}

// NewDirectHTTPClientLeaseForBaseURL returns a direct HTTP client with a concrete base URL.
func NewDirectHTTPClientLeaseForBaseURL(baseURL string) *transport.HTTPClientLease {
	return transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, baseURL, nil)
}

// WorkerBootstrap aliases the worker agent package payload so VM drivers and
// agent registration share one boot contract.
type WorkerBootstrap = workeragent.Bootstrap
