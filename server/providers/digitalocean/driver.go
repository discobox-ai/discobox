package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/dockerworker/sshdocker"
)

// dockerInstallUserData brings up Docker on a fresh droplet. The worker-agent
// container is launched by the control plane over the droplet's Docker daemon,
// so cloud-init only needs Docker itself.
const dockerInstallUserData = `#cloud-config
package_update: true
packages:
  - docker.io
runcmd:
  - [ systemctl, enable, --now, docker ]
`

const (
	dropletStatusPollInterval = time.Second
	gracefulShutdownTimeout   = 30 * time.Second
	dropletPowerActionTimeout = 2 * time.Minute
)

var errDropletStatusTimeout = errors.New("timed out waiting for droplet status")

// DriverConfig configures a DigitalOcean Droplet driver.
type DriverConfig struct {
	Token string

	APIBaseURL string
	Region     string
	Size       string
	Image      string
	SSHKeys    []string
	VPCUUID    string
	Tags       []string

	SSHUser       string
	SSHPrivateKey string

	Backups    bool
	IPv6       bool
	Monitoring bool
	AgentPort  int

	HTTPClient *http.Client
}

// Driver manages one Docker-enabled DigitalOcean Droplet per sandbox worker.
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
	ssh        *sshdocker.Dialer
}

// NewDriver creates a DigitalOcean VM driver.
func NewDriver(cfg DriverConfig) (*Driver, error) {
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
	ssh, err := sshdocker.New(cfg.SSHUser, cfg.SSHPrivateKey)
	if err != nil {
		return nil, err
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
		ssh:        ssh,
	}, nil
}

func (d *Driver) Close() error {
	return nil
}

func (d *Driver) EnsureVM(ctx context.Context, poolID string, spec dockerworker.VMSpec) (*dockerworker.VMInfo, error) {
	existing, err := d.findPoolDroplet(ctx, poolID)
	if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		if vmInfoFromDroplet(*existing).Status == sandbox.StatusStopped {
			if err := d.doDropletAction(ctx, existing.ID, "power_on"); err != nil {
				return nil, err
			}
			existing, err = d.waitForDropletStatus(ctx, poolID, sandbox.StatusRunning, dropletPowerActionTimeout)
			if err != nil {
				return nil, err
			}
		}
		return vmInfoFromDroplet(*existing), nil
	}
	req := createDropletRequest{
		Name:       spec.Name,
		Region:     d.region,
		Size:       d.size,
		Image:      d.image,
		SSHKeys:    d.sshKeys,
		Backups:    d.backups,
		IPv6:       d.ipv6,
		Monitoring: d.monitoring,
		Tags:       dropletTags(d.tags, poolID),
		UserData:   dockerInstallUserData,
		VPCUUID:    d.vpcUUID,
	}
	var out dropletResponse
	if err := d.do(ctx, http.MethodPost, "/v2/droplets", req, &out); err != nil {
		return nil, err
	}
	return vmInfoFromDroplet(out.Droplet), nil
}

// StopVM powers off the Droplet while preserving its root disk. It first asks
// the guest to shut down cleanly, then uses a hard power-off if the guest does
// not stop within the grace period.
func (d *Driver) StopVM(ctx context.Context, poolID string) error {
	droplet, err := d.findPoolDroplet(ctx, poolID)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return nil
		}
		return err
	}
	if vmInfoFromDroplet(*droplet).Status == sandbox.StatusStopped {
		return nil
	}
	if err := d.doDropletAction(ctx, droplet.ID, "shutdown"); err != nil {
		return err
	}
	if _, err := d.waitForDropletStatus(ctx, poolID, sandbox.StatusStopped, gracefulShutdownTimeout); err == nil {
		return nil
	} else if !errors.Is(err, errDropletStatusTimeout) {
		return err
	}
	if err := d.doDropletAction(ctx, droplet.ID, "power_off"); err != nil {
		return err
	}
	_, err = d.waitForDropletStatus(ctx, poolID, sandbox.StatusStopped, dropletPowerActionTimeout)
	return err
}

func (d *Driver) DeleteVM(ctx context.Context, poolID string) error {
	droplet, err := d.findPoolDroplet(ctx, poolID)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return nil
		}
		return err
	}
	err = d.do(ctx, http.MethodDelete, "/v2/droplets/"+url.PathEscape(strconv.FormatInt(droplet.ID, 10)), nil, nil)
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil
	}
	return err
}

func (d *Driver) InspectVM(ctx context.Context, poolID string) (*dockerworker.VMInfo, error) {
	droplet, err := d.findPoolDroplet(ctx, poolID)
	if err != nil {
		return nil, err
	}
	return vmInfoFromDroplet(*droplet), nil
}

// AcquireDockerClient reaches the droplet's Docker daemon by dialing its Unix
// socket over SSH. The engine owns readiness waiting, so failures while the
// droplet boots or installs Docker are expected and retried by the caller.
func (d *Driver) AcquireDockerClient(ctx context.Context, poolID string) (*dockerworker.DockerClientLease, error) {
	droplet, err := d.findPoolDroplet(ctx, poolID)
	if err != nil {
		return nil, err
	}
	host := publicIPv4(droplet.Networks)
	if host == "" {
		return nil, fmt.Errorf("droplet for worker %s has no public IPv4 address yet", poolID)
	}
	return d.ssh.AcquireDockerClient(ctx, sshdocker.Target{Host: host})
}

func (d *Driver) AcquirePoolAgentClient(ctx context.Context, poolID string) (*transport.HTTPClientLease, error) {
	droplet, err := d.findPoolDroplet(ctx, poolID)
	if err != nil {
		return nil, err
	}
	host := publicIPv4(droplet.Networks)
	if host == "" {
		return nil, fmt.Errorf("droplet for worker %s has no public IPv4 address", poolID)
	}
	baseURL := "http://" + net.JoinHostPort(host, strconv.Itoa(d.agentPort))
	return transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, baseURL, nil), nil
}

func (d *Driver) findPoolDroplet(ctx context.Context, poolID string) (*droplet, error) {
	if strings.TrimSpace(poolID) == "" {
		return nil, sandbox.ErrNotFound
	}
	var out dropletsResponse
	if err := d.do(ctx, http.MethodGet, "/v2/droplets?tag_name="+url.QueryEscape(workerTag(poolID)), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Droplets) == 0 {
		return nil, sandbox.ErrNotFound
	}
	return &out.Droplets[0], nil
}

func (d *Driver) doDropletAction(ctx context.Context, dropletID int64, actionType string) error {
	path := "/v2/droplets/" + url.PathEscape(strconv.FormatInt(dropletID, 10)) + "/actions"
	return d.do(ctx, http.MethodPost, path, dropletActionRequest{Type: actionType}, nil)
}

func (d *Driver) waitForDropletStatus(ctx context.Context, poolID string, status sandbox.Status, timeout time.Duration) (*droplet, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		droplet, err := d.findPoolDroplet(ctx, poolID)
		if err != nil {
			return nil, err
		}
		if vmInfoFromDroplet(*droplet).Status == status {
			return droplet, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("%w: pool %s to become %s", errDropletStatusTimeout, poolID, status)
		case <-time.After(dropletStatusPollInterval):
		}
	}
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
	if resp.StatusCode == http.StatusNotFound {
		return sandbox.ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("digitalocean %s %s failed: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func vmInfoFromDroplet(d droplet) *dockerworker.VMInfo {
	var status sandbox.Status
	switch d.Status {
	case "active":
		status = sandbox.StatusRunning
	case "off", "archive":
		status = sandbox.StatusStopped
	case "new":
		status = sandbox.StatusCreated
	default:
		status = sandbox.StatusFailed
	}
	return &dockerworker.VMInfo{
		ID:      strconv.FormatInt(d.ID, 10),
		Status:  status,
		Address: publicIPv4(d.Networks),
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

func dropletTags(defaultTags []string, poolID string) []string {
	tags := append([]string(nil), defaultTags...)
	for _, tag := range []string{"discobox", workerTag(poolID)} {
		if tag == "" || strings.HasSuffix(tag, "-") {
			continue
		}
		if !contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return tags
}

func workerTag(poolID string) string {
	return "discobox-pool-" + safeTag(poolID)
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

type dropletActionRequest struct {
	Type string `json:"type"`
}

type dropletResponse struct {
	Droplet droplet `json:"droplet"`
}

type dropletsResponse struct {
	Droplets []droplet `json:"droplets"`
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
