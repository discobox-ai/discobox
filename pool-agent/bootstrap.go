// Package poolagent implements the pool agent: its boot contract, its identity, and its channel to the control plane.
package poolagent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/controlplane"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/pkg/wire"
)

const (
	EnvControlPlaneURL = "DISCOBOX_CONTROL_PLANE_URL"
	EnvProjectID       = "DISCOBOX_PROJECT_ID"
	EnvPoolID          = "DISCOBOX_POOL_ID"
	EnvControlPlaneKey = "DISCOBOX_CONTROL_PLANE_PUBLIC_KEY"
	// EnvAgentListenURL is the transport URL the pool-agent HTTP server binds.
	// The scheme selects the transport, so a backend that terminates the agent
	// API on VSOCK or a Unix socket needs no separate port variable.
	EnvAgentListenURL  = "DISCOBOX_AGENT_LISTEN_URL"
	EnvHostMountPrefix = "DISCOBOX_POOL_HOST_MOUNT_PREFIX"
	// EnvHostStateRoot is where the Docker daemon sees layout.ContainerRoot. It
	// is empty when the daemon sees the same path the container does; a backend
	// sets it when it must place state elsewhere, as wslc does to reach the only
	// disk it persists.
	EnvHostStateRoot = "DISCOBOX_POOL_HOST_STATE_ROOT"
	// EnvRuntime and EnvDriver name the pool's sandbox runtime and, for the
	// machine runtime, the disco-vm driver its sandboxes' machines run on
	// (ADR 26-10-23-551 §3).
	EnvRuntime = "DISCOBOX_POOL_RUNTIME"
	EnvDriver  = "DISCOBOX_POOL_DRIVER"
	// EnvBootstrapFile names a file holding the bootstrap as JSON, which a
	// pool's maker places beside the pool bundle rather than rendering it into
	// an environment (ADR 26-10-24-953 §2). It is root's alone: it says which
	// control plane the agent trusts.
	EnvBootstrapFile = "DISCOBOX_POOL_BOOTSTRAP_FILE"
)

// Runtime is the kind of sandbox runtime a pool agent composes.
type Runtime string

const (
	// RuntimeDocker is sandboxes as containers on the Docker daemon beside the
	// agent: the pool-agent image's runtime, and a bootstrap's that names
	// none.
	RuntimeDocker Runtime = "docker"
	// RuntimeMachine is sandboxes as disco-vm machines on the pool's own
	// engine (ADR 26-10-23-551 §3): the runtime of a pool machine booted from
	// the pool-agent bundle, which names its driver.
	RuntimeMachine Runtime = "machine"
)

// Bootstrap is the VM boot contract used by the control plane and pool agent.
//
// Both directions are addressed by a single URL each, and the scheme alone
// decides the transport (see the root wire package). A backend is therefore
// expressed entirely in the URLs it renders — http:// for a pool that shares the
// host network, vsock://2:3001 for a libkrun microVM, unix:///... for a guest
// whose helper terminates the socket — with no transport-specific field here.
type Bootstrap struct {
	// ControlPlaneURL is where the agent reaches the control plane.
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	ProjectID       string `json:"projectId,omitempty"`
	PoolID          string `json:"poolId,omitempty"`
	ControlPlaneKey string `json:"controlPlanePublicKey,omitempty"`
	// AgentListenURL is where the agent's own HTTP server binds.
	AgentListenURL  string `json:"agentListenUrl,omitempty"`
	HostMountPrefix string `json:"hostMountPrefix,omitempty"`
	// HostStateRoot is where the pool's Docker daemon sees layout.ContainerRoot.
	// Empty means no relocation. Only paths handed to the daemon are translated;
	// everything the agent reads and writes stays in container terms.
	HostStateRoot string `json:"hostStateRoot,omitempty"`
	// Runtime is the pool's sandbox runtime; empty is RuntimeDocker.
	Runtime Runtime `json:"runtime,omitempty"`
	// Driver is the disco-vm driver a RuntimeMachine pool's sandboxes run on,
	// such as boxd. Only that runtime has one.
	Driver string `json:"driver,omitempty"`
}

// EffectiveRuntime is the runtime the bootstrap names, RuntimeDocker when it
// names none.
func (b Bootstrap) EffectiveRuntime() Runtime {
	if b.Runtime == "" {
		return RuntimeDocker
	}
	return b.Runtime
}

// Validate checks the required pool bootstrap fields.
func (b Bootstrap) Validate() error {
	if strings.TrimSpace(b.ControlPlaneURL) == "" {
		return errors.New("control plane URL is required")
	}
	if strings.TrimSpace(b.PoolID) == "" {
		return errors.New("pool ID is required")
	}
	// Validating here means an unreachable transport is rejected at boot with a
	// clear message, rather than at the first request.
	if _, err := wire.Parse(b.ControlPlaneURL); err != nil {
		return fmt.Errorf("control plane URL: %w", err)
	}
	if listen := strings.TrimSpace(b.AgentListenURL); listen != "" {
		if _, err := wire.Parse(listen); err != nil {
			return fmt.Errorf("agent listen URL: %w", err)
		}
	}
	switch b.EffectiveRuntime() {
	case RuntimeDocker:
		if b.Driver != "" {
			return fmt.Errorf("driver %q is for the %s runtime; this pool's is %s", b.Driver, RuntimeMachine, RuntimeDocker)
		}
	case RuntimeMachine:
		if strings.TrimSpace(b.Driver) == "" {
			return fmt.Errorf("the %s runtime needs a driver", RuntimeMachine)
		}
	default:
		return fmt.Errorf("unknown pool runtime %q", b.Runtime)
	}
	return nil
}

type StatusClient interface {
	UpdatePoolStatus(ctx context.Context, req StatusRequest) error
}

// SandboxStateClient is the pool's channel to the control plane about which
// sandboxes exist. Reporting observed states is what makes power state knowable
// at all: operations answer with acceptance only, and the transitions that
// matter most — a container dying, a host rebooting — have no request to answer
// (ADR 0017 §10). Listing the held sandboxes is the other direction: the
// control plane, not this pool, decides which sandboxes exist, and the volume
// reaper collects only the trees it does not hold (ADR 26-10-01-876).
type SandboxStateClient interface {
	ReportSandboxStates(ctx context.Context, req SandboxStateRequest) error
	ListHeldSandboxes(ctx context.Context, req HeldSandboxesRequest) ([]string, error)
}

// SandboxAgentStatusClient mints scoped sandbox-agent tokens and pushes
// polled sandbox-agent status batches (ADR 0030) and inventories
// (ADR 26-10-11-657) to the control plane.
type SandboxAgentStatusClient interface {
	MintSandboxAgentStatusTokens(ctx context.Context, req MintSandboxAgentStatusTokensRequest) (*MintSandboxAgentStatusTokensResponse, error)
	ReportSandboxAgentStatus(ctx context.Context, req SandboxAgentStatusReportRequest) error
	ReportSandboxInventories(ctx context.Context, req SandboxInventoryReportRequest) (*SandboxInventoryReportResponse, error)
}

// hostedPlatform is the one platform this pool hosts, which its agent declares
// at registration and on every status report (ADR 0145 §1). A pool's sandboxes
// are containers on the agent's own kernel and architecture, so it is the
// agent's own platform.
func hostedPlatform() platform.Platform {
	return platform.Current()
}

// hostedImageKind is the kind of image this pool runs, declared beside its
// platform (ADR 26-10-09-106 §4). This agent's sandboxes are containers, so
// it runs OCI images.
func hostedImageKind() platform.ImageKind {
	return platform.OCI
}

// StatusRequest updates pool scheduling status using a signed pool assertion.
type StatusRequest struct {
	ControlPlaneURL string             `json:"-"`
	ProjectID       string             `json:"-"`
	PoolID          string             `json:"-"`
	PrivateKey      ed25519.PrivateKey `json:"-"`
	// Platform is declared on every report, not only at registration: an
	// agent whose key survives a restart does not register again, and the
	// control plane learns what a pool from before platforms hosts from this.
	Platform              platform.Platform  `json:"platform"`
	ImageKind             platform.ImageKind `json:"imageKind"`
	Ready                 bool               `json:"ready"`
	Schedulable           bool               `json:"schedulable"`
	Degraded              bool               `json:"degraded"`
	AvailableCPUVCPUs     float64            `json:"availableCpuVcpus"`
	AvailableMemoryBytes  int64              `json:"availableMemoryBytes"`
	AvailableStorageBytes int64              `json:"availableStorageBytes"`
	Conditions            any                `json:"conditions,omitempty"`
}

// SandboxState is one observation about one sandbox.
type SandboxState struct {
	SandboxID string `json:"sandboxId"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

// SandboxProgress is provisioning progress on one sandbox: work underway that
// has no state transition to announce it, an image pull above all (ADR 0039).
//
// It rides the state channel but is deliberately not a SandboxState. A state
// observation always carries an observed state and progress has none, and a
// complete sync's "every sandbox I host" claim is about states only.
type SandboxProgress struct {
	SandboxID string `json:"sandboxId"`
	// Phase is what is being done, and is always set. Pull refines the one
	// phase that can say how far in it is (ADR 0060).
	Phase string                `json:"phase"`
	Pull  *SandboxPullProgress  `json:"pull,omitempty"`
	Clone *SandboxCloneProgress `json:"clone,omitempty"`
}

// SandboxCloneProgress is the sandbox cloning a source, refining the
// materializing_source phase: what the source is, and how far its clone has
// got when the sandbox has said. Each stage counts toward a total git knows
// before the stage starts.
type SandboxCloneProgress struct {
	Source       string `json:"source"`
	Stage        string `json:"stage,omitempty"`
	Objects      int64  `json:"objects,omitempty"`
	ObjectsTotal int64  `json:"objectsTotal,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
}

// SandboxPullProgress is an image pull as a status line wants it: bytes against
// bytes and layers against layers. Both totals grow while the manifest is
// walked, so the pair is a ratio at a moment rather than progress toward a
// fixed target.
type SandboxPullProgress struct {
	Image          string `json:"image"`
	Layers         int    `json:"layers"`
	LayersComplete int    `json:"layersComplete"`
	Current        int64  `json:"current"`
	Total          int64  `json:"total"`
	Done           bool   `json:"done,omitempty"`
}

// SandboxStateRequest is one delivery on the state channel.
type SandboxStateRequest struct {
	ControlPlaneURL string             `json:"-"`
	ProjectID       string             `json:"-"`
	PoolID          string             `json:"-"`
	PrivateKey      ed25519.PrivateKey `json:"-"`

	// BootID identifies this run of the agent and Sequence counts batches
	// within it. Together they let the control plane drop a delayed delta that
	// would otherwise overwrite a newer full sync; across boots the sequence
	// restarts and means nothing, so ReportedAt decides.
	BootID     string    `json:"bootId"`
	Sequence   int64     `json:"sequence"`
	ReportedAt time.Time `json:"reportedAt"`
	// Complete marks the periodic full sync: States is every sandbox this agent
	// hosts, so a sandbox the control plane thinks is here and that States omits
	// no longer has a container.
	Complete bool           `json:"complete"`
	States   []SandboxState `json:"states"`
	// Progress is always a delta and is unaffected by Complete, which describes
	// States only.
	Progress []SandboxProgress `json:"progress,omitempty"`
}

// HeldSandboxesRequest asks the control plane for every sandbox it holds on
// this pool, in any state.
type HeldSandboxesRequest struct {
	ControlPlaneURL string
	ProjectID       string
	PoolID          string
	PrivateKey      ed25519.PrivateKey
}

// MintSandboxAgentStatusTokensRequest asks the control plane for short-lived
// sandbox-agent tokens for the sandboxes this pool hosts, good for reading
// their status and their inventory and nothing else. It carries no scope: the
// control plane always mints those two, regardless of what is requested.
type MintSandboxAgentStatusTokensRequest struct {
	ControlPlaneURL string             `json:"-"`
	ProjectID       string             `json:"-"`
	PoolID          string             `json:"-"`
	PrivateKey      ed25519.PrivateKey `json:"-"`
	SandboxIDs      []string           `json:"sandboxIds"`
}

// SandboxAgentStatusToken is one minted sandbox-agent token for the poll.
type SandboxAgentStatusToken struct {
	SandboxID string    `json:"sandboxId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// MintSandboxAgentStatusTokensResponse is the control plane's response to a
// token mint request.
type MintSandboxAgentStatusTokensResponse struct {
	Tokens []SandboxAgentStatusToken `json:"tokens"`
}

// SandboxAgentStatusEntry is one sandbox's polled status, relayed to the
// control plane opaquely: Status is forwarded as received, never re-serialized
// from a parsed structure, so a field this agent does not model still reaches
// the control plane. (encoding/json compacts it in transit; nothing else
// changes.)
//
// Resources is the one thing read out of that payload rather than merely
// forwarded, and it is deliberately not part of it. The relay is unaffected;
// the counters are copied aside so the resource reporter can difference them
// across its tick, and they travel to the control plane in that agent-authored
// report instead (ADR 0071 resource accounting §2).
type SandboxAgentStatusEntry struct {
	SandboxID  string          `json:"sandboxId"`
	Status     json.RawMessage `json:"status"`
	ObservedAt time.Time       `json:"observedAt"`

	// Resources never leaves this process on this channel.
	Resources *apimodel.SandboxAgentResourceUsage `json:"-"`
}

// SandboxAgentStatusReportRequest pushes this tick's successfully polled
// sandbox-agent status batch to the control plane. Sandboxes that failed to
// poll this tick are omitted, not reported as stale.
type SandboxAgentStatusReportRequest struct {
	ControlPlaneURL string                    `json:"-"`
	ProjectID       string                    `json:"-"`
	PoolID          string                    `json:"-"`
	PrivateKey      ed25519.PrivateKey        `json:"-"`
	Sandboxes       []SandboxAgentStatusEntry `json:"sandboxes"`
}

// SandboxInventoryEntry is one sandbox's inventory as this tick read it
// (ADR 26-10-11-657 §2). Inventory is the sandbox agent's answer, relayed as
// received like a status, and is omitted when the control plane was already
// given the inventory with this digest: the entry then only says the
// inventory still stood at ObservedAt.
type SandboxInventoryEntry struct {
	SandboxID  string          `json:"sandboxId"`
	Digest     string          `json:"digest"`
	ObservedAt time.Time       `json:"observedAt"`
	Inventory  json.RawMessage `json:"inventory,omitempty"`
}

// SandboxInventoryReportRequest pushes the inventories this tick read.
type SandboxInventoryReportRequest struct {
	ControlPlaneURL string                  `json:"-"`
	ProjectID       string                  `json:"-"`
	PoolID          string                  `json:"-"`
	PrivateKey      ed25519.PrivateKey      `json:"-"`
	Sandboxes       []SandboxInventoryEntry `json:"sandboxes"`
}

// SandboxInventoryReportResponse names the sandboxes reported by digest alone
// whose inventory the control plane does not hold under that digest.
type SandboxInventoryReportResponse struct {
	Wanted []string `json:"wanted"`
}

// Identity is the pool's identity: its key, made on the pool by
// `discobox-provision identity` and recorded on the pool by its maker before
// this agent starts (ADR 26-10-24-953 §4). The private key signs every
// request to the control plane; nothing registers it.
type Identity struct {
	PublicKey  string
	PrivateKey ed25519.PrivateKey
}

// LoadIdentity reads the pool's identity key at path, on the pool's durable
// storage (layout.Root.PoolIdentityKey, ADR 0063). None is a pool its maker
// has not enrolled, which this agent cannot be, and never makes one: a key
// made here would be one the control plane has never heard of.
func LoadIdentity(path string) (*Identity, error) {
	key, err := poolauth.LoadKey(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("pool identity key %s is not there: this pool's maker enrolls it before the agent starts", path)
	}
	if err != nil {
		return nil, err
	}
	public, err := poolauth.PublicKeyOf(key)
	if err != nil {
		return nil, err
	}
	return &Identity{PublicKey: public, PrivateKey: key}, nil
}

// Load reads this agent's bootstrap: the file EnvBootstrapFile names, when
// the environment names one, with each field the environment sets taking the
// environment's value. A pool's maker places the file in its box
// (bundle.BootstrapFile) and the agent's unit names it (ADR 26-10-24-953 §2).
// A control plane URL neither gives is the local default.
func Load() (Bootstrap, error) {
	var bootstrap Bootstrap
	if path := strings.TrimSpace(os.Getenv(EnvBootstrapFile)); path != "" {
		file, err := readBootstrapFile(path)
		if err != nil {
			return Bootstrap{}, err
		}
		bootstrap = file
	}
	for _, field := range []struct {
		env   string
		value *string
	}{
		{EnvControlPlaneURL, &bootstrap.ControlPlaneURL},
		{EnvProjectID, &bootstrap.ProjectID},
		{EnvPoolID, &bootstrap.PoolID},
		{EnvControlPlaneKey, &bootstrap.ControlPlaneKey},
		{EnvAgentListenURL, &bootstrap.AgentListenURL},
		{EnvHostMountPrefix, &bootstrap.HostMountPrefix},
		{EnvHostStateRoot, &bootstrap.HostStateRoot},
		{EnvDriver, &bootstrap.Driver},
	} {
		if value := strings.TrimSpace(os.Getenv(field.env)); value != "" {
			*field.value = value
		}
	}
	if value := strings.TrimSpace(os.Getenv(EnvRuntime)); value != "" {
		bootstrap.Runtime = Runtime(value)
	}
	if strings.TrimSpace(bootstrap.ControlPlaneURL) == "" {
		bootstrap.ControlPlaneURL = controlplane.DefaultURL("localhost", controlplane.DefaultPort)
	}
	return bootstrap, nil
}

// readBootstrapFile reads the bootstrap file at path, refusing one another
// user owns or may read or write: a file another user could rewrite would
// choose the control plane, and the key, this agent trusts.
func readBootstrapFile(path string) (Bootstrap, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the path is the bootstrap file the agent's unit names.
	if err != nil {
		return Bootstrap{}, fmt.Errorf("read pool bootstrap: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Bootstrap{}, fmt.Errorf("read pool bootstrap: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Bootstrap{}, fmt.Errorf("pool bootstrap %s is not a regular file", path)
	}
	if err := checkBootstrapAccess(path, info); err != nil {
		return Bootstrap{}, err
	}
	var bootstrap Bootstrap
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bootstrap); err != nil {
		return Bootstrap{}, fmt.Errorf("decode pool bootstrap %s: %w", path, err)
	}
	return bootstrap, nil
}
