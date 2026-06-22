// Package digitalocean implements a DigitalOcean Droplet VM driver.
package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/sandbox/vm"
)

const (
	ProviderType      = "digitalocean"
	defaultAPIBaseURL = "https://api.digitalocean.com"
	defaultRegion     = "nyc3"
	defaultSize       = "s-1vcpu-1gb"
	defaultImage      = "ubuntu-24-04-x64"
	defaultAgentPort  = 3002
)

// ProviderInstanceConfig is the persisted provider instance configuration.
type ProviderInstanceConfig struct {
	Token           string   `json:"token,omitempty"`
	TokenEnv        string   `json:"tokenEnv,omitempty"`
	ControlPlaneURL string   `json:"controlPlaneUrl,omitempty"`
	APIBaseURL      string   `json:"apiBaseUrl,omitempty"`
	Region          string   `json:"region,omitempty"`
	Size            string   `json:"size,omitempty"`
	Image           string   `json:"image,omitempty"`
	SSHKeys         []string `json:"sshKeys,omitempty"`
	VPCUUID         string   `json:"vpcUuid,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Backups         bool     `json:"backups,omitempty"`
	IPv6            bool     `json:"ipv6,omitempty"`
	Monitoring      bool     `json:"monitoring,omitempty"`
	AgentPort       int      `json:"agentPort,omitempty"`
	PoolSize        int      `json:"poolSize,omitempty"`
	MinWorkers      int      `json:"minWorkers,omitempty"`
	MaxWorkers      int      `json:"maxWorkers,omitempty"`
	MinHealthy      int      `json:"minHealthyWorkers,omitempty"`
}

// Config configures a DigitalOcean Droplet driver.
type Config struct {
	Token string

	APIBaseURL string
	Region     string
	Size       string
	Image      string
	SSHKeys    []string
	VPCUUID    string
	Tags       []string

	Backups    bool
	IPv6       bool
	Monitoring bool
	AgentPort  int

	HTTPClient *http.Client
}

// Definition describes the DigitalOcean provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "DigitalOcean",
		Icon:        "digitalocean",
		Description: "Runs sandboxes as DigitalOcean Droplets.",
		ConfigFields: []sandbox.ProviderConfigField{
			{Key: "token", Label: "API Token", Type: "password", CredentialProvider: "digitalocean", CredentialAuthType: "token"},
			{Key: "tokenEnv", Label: "API Token Environment Variable", Type: "string", Placeholder: "DIGITALOCEAN_ACCESS_TOKEN", Description: "Environment variable containing the API token; use instead of token for local CLI workflows."},
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Required: true, Placeholder: "https://discobot.example.com"},
			{Key: "minWorkers", Label: "Minimum Workers", Type: "number", Placeholder: "1", Description: "Minimum active VM workers to keep in the pool."},
			{Key: "maxWorkers", Label: "Maximum Workers", Type: "number", Placeholder: "2", Description: "Maximum active VM workers allowed in the pool."},
			{Key: "minHealthyWorkers", Label: "Minimum Healthy Workers", Type: "number", Placeholder: "1", Description: "Minimum ready, schedulable, non-degraded workers before launching replacements."},
			{Key: "poolSize", Label: "Pool Size", Type: "number", Placeholder: "1", Description: "Deprecated alias for minimum workers.", Advanced: true},
			{Key: "region", Label: "Region", Type: "string", Placeholder: defaultRegion},
			{Key: "size", Label: "Droplet Size", Type: "string", Placeholder: defaultSize},
			{Key: "image", Label: "Image", Type: "string", Placeholder: defaultImage},
			{Key: "sshKeys", Label: "SSH Keys", Type: "string", Description: "Optional SSH key IDs or fingerprints.", Advanced: true},
			{Key: "vpcUuid", Label: "VPC UUID", Type: "string", Advanced: true},
			{Key: "tags", Label: "Tags", Type: "string", Advanced: true},
			{Key: "backups", Label: "Backups", Type: "boolean", Advanced: true},
			{Key: "ipv6", Label: "IPv6", Type: "boolean", Advanced: true},
			{Key: "monitoring", Label: "Monitoring", Type: "boolean", Advanced: true},
			{Key: "agentPort", Label: "Agent Port", Type: "number", Placeholder: strconv.Itoa(defaultAgentPort), Advanced: true},
		},
	}
}

// Driver manages one DigitalOcean Droplet per sandbox worker.
type Driver struct {
	token      string
	baseURL    string
	region     string
	size       string
	image      string
	sshKeys    []string
	vpcUUID    string
	tags       []string
	backups    bool
	ipv6       bool
	monitoring bool
	agentPort  int
	client     *http.Client
}

// NewDriver creates a DigitalOcean VM driver.
func NewDriver(cfg Config) (*Driver, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("digitalocean token is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	agentPort := cfg.AgentPort
	if agentPort == 0 {
		agentPort = defaultAgentPort
	}
	return &Driver{
		token:      strings.TrimSpace(cfg.Token),
		baseURL:    strings.TrimRight(defaultString(cfg.APIBaseURL, defaultAPIBaseURL), "/"),
		region:     defaultString(cfg.Region, defaultRegion),
		size:       defaultString(cfg.Size, defaultSize),
		image:      defaultString(cfg.Image, defaultImage),
		sshKeys:    append([]string(nil), cfg.SSHKeys...),
		vpcUUID:    cfg.VPCUUID,
		tags:       append([]string(nil), cfg.Tags...),
		backups:    cfg.Backups,
		ipv6:       cfg.IPv6,
		monitoring: cfg.Monitoring,
		agentPort:  agentPort,
		client:     client,
	}, nil
}

// NewProvider creates a generic VM provider backed by DigitalOcean Droplets.
func NewProvider(cfg Config, providerCfg vm.Config) (*vm.Provider, error) {
	driver, err := NewDriver(cfg)
	if err != nil {
		return nil, err
	}
	providerCfg.Driver = driver
	if providerCfg.Name == "" {
		providerCfg.Name = "DigitalOcean"
	}
	if providerCfg.Description == "" {
		providerCfg.Description = "Runs sandboxes as DigitalOcean Droplets."
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
	image := spec.Image
	if image == "" {
		image = d.image
	}
	req := createDropletRequest{
		Name:       spec.Name,
		Region:     d.region,
		Size:       effectiveSize(d.size, spec.Resources),
		Image:      image,
		SSHKeys:    d.sshKeys,
		Backups:    d.backups,
		IPv6:       d.ipv6,
		Monitoring: d.monitoring,
		Tags:       dropletTags(d.tags, spec),
		UserData:   spec.Boot.CloudInitUserData,
		VPCUUID:    d.vpcUUID,
	}
	var out dropletResponse
	if err := d.do(ctx, http.MethodPost, "/v2/droplets", req, &out); err != nil {
		return nil, err
	}
	return instanceFromDroplet(out.Droplet, d.agentPort), nil
}

func (d *Driver) StartVM(ctx context.Context, id string) (*vm.Instance, error) {
	if err := d.action(ctx, id, "power_on"); err != nil {
		return nil, err
	}
	return d.InspectVM(ctx, id)
}

func (d *Driver) StopVM(ctx context.Context, id string, timeout time.Duration) (*vm.Instance, error) {
	actionType := "shutdown"
	if timeout == 0 {
		actionType = "power_off"
	}
	if err := d.action(ctx, id, actionType); err != nil {
		return nil, err
	}
	return d.InspectVM(ctx, id)
}

func (d *Driver) DeleteVM(ctx context.Context, id string, _ bool) error {
	return d.do(ctx, http.MethodDelete, "/v2/droplets/"+url.PathEscape(id), nil, nil)
}

func (d *Driver) InspectVM(ctx context.Context, id string) (*vm.Instance, error) {
	var out dropletResponse
	if err := d.do(ctx, http.MethodGet, "/v2/droplets/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return instanceFromDroplet(out.Droplet, d.agentPort), nil
}

func (d *Driver) AcquireHTTPClient(context.Context, *vm.Instance) (*transport.HTTPClientLease, error) {
	return vm.NewDirectHTTPClientLease(), nil
}

func (d *Driver) action(ctx context.Context, id, actionType string) error {
	return d.do(ctx, http.MethodPost, "/v2/droplets/"+url.PathEscape(id)+"/actions", map[string]string{"type": actionType}, nil)
}

func (d *Driver) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("digitalocean %s %s failed: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func instanceFromDroplet(d droplet, agentPort int) *vm.Instance {
	createdAt, _ := time.Parse(time.RFC3339, d.CreatedAt)
	var status sandbox.Status
	var startedAt *time.Time
	var stoppedAt *time.Time
	switch d.Status {
	case "active":
		status = sandbox.StatusRunning
		if !createdAt.IsZero() {
			startedAt = &createdAt
		}
	case "off", "archive":
		status = sandbox.StatusStopped
		now := time.Now().UTC()
		stoppedAt = &now
	case "new":
		status = sandbox.StatusCreated
	default:
		status = sandbox.StatusFailed
	}
	host := publicIPv4(d.Networks)
	agentURL := ""
	if host != "" {
		agentURL = "http://" + host + ":" + strconv.Itoa(agentPort)
	}
	metadata := map[string]string{
		"digitalocean.region": d.Region.Slug,
		"digitalocean.size":   d.SizeSlug,
		"digitalocean.status": d.Status,
	}
	return &vm.Instance{
		ID:        strconv.FormatInt(d.ID, 10),
		Name:      d.Name,
		Image:     d.Image.Slug,
		Status:    status,
		AgentURL:  agentURL,
		AgentHost: host,
		Metadata:  metadata,
		CreatedAt: createdAt,
		StartedAt: startedAt,
		StoppedAt: stoppedAt,
	}
}

func publicIPv4(networks dropletNetworks) string {
	for _, network := range networks.V4 {
		if network.Type == "public" && network.IPAddress != "" {
			return network.IPAddress
		}
	}
	return ""
}

func effectiveSize(defaultSize string, resources sandbox.ResourceConfig) string {
	return defaultSize
}

func dropletTags(defaultTags []string, spec vm.InstanceSpec) []string {
	tags := append([]string(nil), defaultTags...)
	for _, tag := range []string{
		"discobox",
		"discobox-project-" + safeTag(spec.Ref.ProjectID),
		"discobox-sandbox-" + safeTag(spec.Ref.SandboxID),
	} {
		if strings.TrimRight(tag, "-") != tag || strings.HasSuffix(tag, "-") {
			continue
		}
		if !contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return tags
}

func safeTag(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", "-", ":", "-", "/", "-", " ", "-").Replace(value)
	return strings.Trim(value, "-")
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

type createDropletRequest struct {
	Name       string   `json:"name"`
	Region     string   `json:"region"`
	Size       string   `json:"size"`
	Image      string   `json:"image"`
	SSHKeys    []string `json:"ssh_keys,omitempty"`
	Backups    bool     `json:"backups,omitempty"`
	IPv6       bool     `json:"ipv6,omitempty"`
	Monitoring bool     `json:"monitoring,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	UserData   string   `json:"user_data,omitempty"`
	VPCUUID    string   `json:"vpc_uuid,omitempty"`
}

type dropletResponse struct {
	Droplet droplet `json:"droplet"`
}

type droplet struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	SizeSlug  string          `json:"size_slug"`
	Region    dropletRegion   `json:"region"`
	Image     dropletImage    `json:"image"`
	Networks  dropletNetworks `json:"networks"`
}

type dropletRegion struct {
	Slug string `json:"slug"`
}

type dropletImage struct {
	Slug string `json:"slug"`
}

type dropletNetworks struct {
	V4 []dropletNetwork `json:"v4"`
}

type dropletNetwork struct {
	IPAddress string `json:"ip_address"`
	Type      string `json:"type"`
}
