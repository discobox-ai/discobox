package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/x/id"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
	"github.com/discobox-ai/discobox/pool-agent/gitcache"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// A machine pool's sandboxes are disco-vm machines on the pool's own engine
// (ADR 26-10-23-551 §3), one per sandbox, named by its ID so that a create
// that was lost is found by name. A sandbox's machine boots a driver base, and
// every start places the platform bundle's agent layer, its units, its pins
// and sandbox.json through disco-vm's copy, then runs the agent's
// `discobox-provision apply` through its exec (ADR 26-10-24-953 §2). The
// sandbox agent listens where the driver's service says, and the pool reaches
// it at the instance's one URL.
//
// The machine's disk is the sandbox: nothing of it is on the pool. What the
// pool keeps is its records, in the sandbox's tree as a container's are
// (sandboxCore), and its proxy material.

// MachineSandboxRuntimeConfig is what a machine pool's runtime is made with.
type MachineSandboxRuntimeConfig struct {
	ProjectID             string
	PoolID                string
	ControlPlanePublicKey string
	// AgentTLS has every sandbox serve its agent's API over TLS, the pool's
	// setting, which a machine takes at its next start (ADR 26-10-29-481
	// §3).
	AgentTLS bool
	// Root is where the runtime reads and writes pool state.
	Root layout.Root
	// Engine is the pool's own disco-vm engine, in the pool's project.
	Engine *engine.Engine
	// Base is the ID of the driver's base a sandbox's machine boots; empty is
	// the first base the driver lists.
	Base string
	// Isolate creates every sandbox's machine stripped of what its provider
	// gives a machine by default, on Network alone (ADR 26-10-23-551 §4). A
	// driver that cannot isolate a machine is refused for it.
	Isolate bool
	// Network is the provider's network the pool's machine and its sandboxes'
	// machines share, and no other machine of the provider's.
	Network string
	// Egress fences what every sandbox's machine reaches beyond Network, set
	// and read back by the driver before a create returns (ADR 26-10-29-481
	// §4). Nil leaves the driver's default, which only a test runtime does; a
	// driver that cannot fence is refused for it.
	Egress *machine.Egress
	// SandboxIdleTimeout is how long a sandbox runs idle before it powers
	// itself off (ADR 0108). Zero leaves the sandbox-agent's default.
	SandboxIdleTimeout time.Duration
	// PoolEndpoints is where the pool serves its sandboxes, as their
	// bootstraps name it (proxyagent.PoolEndpoints).
	PoolEndpoints sandboxconfig.PoolEndpoints
	// IdentityKey is the pool's identity key, which signs the runtime-config
	// documents the pool delivers its sandboxes (ADR 26-10-08-127 §3).
	IdentityKey ed25519.PrivateKey
	// Platform is the one platform the pool hosts (ADR 0145 §1).
	Platform platform.Platform
	// AgentLayers is the pool's asset cache, which a sandbox's agent layer is
	// read from.
	AgentLayers AgentLayers
}

// MachineSandboxRuntime runs a pool's sandboxes as disco-vm machines.
type MachineSandboxRuntime struct {
	sandboxCore
	engine  *engine.Engine
	base    string
	isolate bool
	network string
	egress  *machine.Egress
	// observed is each sandbox's power state as the runtime last saw it,
	// which answers what must not wait on the driver (RunningSandboxIDsInMode)
	// and what the driver cannot say for now (an unreachable service).
	observed sync.Map
	// urls is each running sandbox's instance URL, asked of the driver once
	// per start rather than on every dial.
	urls sync.Map
}

var _ Runtime = (*MachineSandboxRuntime)(nil)

// NewMachineSandboxRuntime is a machine pool's runtime over its engine.
func NewMachineSandboxRuntime(cfg MachineSandboxRuntimeConfig) (*MachineSandboxRuntime, error) {
	if cfg.Engine == nil {
		return nil, errors.New("a machine runtime needs its engine")
	}
	if cfg.Isolate {
		if !cfg.Engine.Driver.Capabilities().Isolation {
			return nil, fmt.Errorf("the %s driver cannot isolate a sandbox's machine from the rest of its provider's account (ADR 26-10-23-551 §4)", cfg.Engine.Driver.Name())
		}
		if strings.TrimSpace(cfg.Network) == "" {
			return nil, errors.New("an isolated sandbox machine needs the pool's network, or it reaches nothing")
		}
	}
	if cfg.Egress != nil && !cfg.Engine.Driver.Capabilities().Egress {
		return nil, fmt.Errorf("the %s driver cannot fence a sandbox's machine's egress to its pool (ADR 26-10-29-481 §4)", cfg.Engine.Driver.Name())
	}
	r := &MachineSandboxRuntime{
		sandboxCore: sandboxCore{
			paths:                 sandboxpath.For(cfg.Platform),
			projectID:             cfg.ProjectID,
			poolID:                cfg.PoolID,
			controlPlanePublicKey: cfg.ControlPlanePublicKey,
			agentTLS:              cfg.AgentTLS,
			sandboxIdleTimeout:    cfg.SandboxIdleTimeout,
			poolEndpoints:         cfg.PoolEndpoints,
			identityKey:           cfg.IdentityKey,
			root:                  cfg.Root,
			gitCache:              gitcache.New(cfg.Root.PoolGitCache(cfg.ProjectID, cfg.PoolID)),
			agentLayers:           cfg.AgentLayers,
		},
		engine:  cfg.Engine,
		base:    strings.TrimSpace(cfg.Base),
		isolate: cfg.Isolate,
		network: strings.TrimSpace(cfg.Network),
		egress:  cfg.Egress,
	}
	r.boxes = r
	return r, nil
}

// Capabilities is what the pool's driver can do, which the pool reports as
// its capacity: a machine pool's sandboxes run on its driver, not on the
// pool's own machine.
func (r *MachineSandboxRuntime) Capabilities() machine.Capabilities {
	return r.engine.Driver.Capabilities()
}

// Driver names the pool's disco-vm driver.
func (r *MachineSandboxRuntime) Driver() string { return r.engine.Driver.Name() }

// machineName is the name of a sandbox's machine: its ID, in the lower-case
// letters, digits and single hyphens a disco-vm name is spelled in.
func machineName(sandboxID string) (string, error) {
	name := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(sandboxID)), "_", "-")
	if err := machine.ValidateName(name); err != nil {
		return "", fmt.Errorf("sandbox %s has no machine name: %w", sandboxID, err)
	}
	return name, nil
}

// machineRecordName is the pool's record of a sandbox's machine, in the
// sandbox's tree.
const machineRecordName = "machine.json"

// machineRecord is what the pool made a sandbox's machine with. The bootstrap
// is kept whole, so a machine is made again in place from it when its
// project layer moves with no create request in hand (rebuildForProjectLayer).
type machineRecord struct {
	// Instance is the machine's ID, as its driver keeps it.
	Instance        string `json:"instance"`
	SpecFingerprint string `json:"specFingerprint,omitempty"`
	HarnessMode     string `json:"harnessMode,omitempty"`
	// Env is the sandbox user's environment, which names who owns what the
	// pool serves the sandbox (GitOriginPath).
	Env map[string]string `json:"env,omitempty"`
	// Bootstrap is what sandbox.json is rendered from.
	Bootstrap sandboxconfig.Document `json:"bootstrap"`
	// Started reports a machine started since it was made: one that was not
	// is created, not stopped.
	Started bool `json:"started,omitempty"`
}

func (r *MachineSandboxRuntime) machineRecordPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), machineRecordName)
}

func (r *MachineSandboxRuntime) readMachineRecord(sandboxID string) (machineRecord, bool, error) {
	data, err := os.ReadFile(r.machineRecordPath(sandboxID))
	if errors.Is(err, os.ErrNotExist) {
		return machineRecord{}, false, nil
	}
	if err != nil {
		return machineRecord{}, false, err
	}
	var record machineRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return machineRecord{}, false, fmt.Errorf("decode %s: %w", machineRecordName, err)
	}
	return record, true, nil
}

func (r *MachineSandboxRuntime) writeMachineRecord(sandboxID string, record machineRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := r.machineRecordPath(sandboxID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// instance is the sandbox's machine, by its name alone, and ErrNotFound when
// its driver has none.
func (r *MachineSandboxRuntime) instance(ctx context.Context, sandboxID string) (machine.Instance, error) {
	name, err := machineName(sandboxID)
	if err != nil {
		return machine.Instance{}, err
	}
	inst, err := r.engine.Driver.Instance(ctx, name)
	if errors.Is(err, machine.ErrNotFound) {
		return machine.Instance{}, ErrNotFound
	}
	return inst, err
}

// observe records and returns a sandbox's power state.
func (r *MachineSandboxRuntime) observe(sandboxID string, status Status) Status {
	r.observed.Store(sandboxID, status)
	return status
}

func (r *MachineSandboxRuntime) lastObserved(sandboxID string) (Status, bool) {
	value, ok := r.observed.Load(sandboxID)
	if !ok {
		return "", false
	}
	status, ok := value.(Status)
	return status, ok
}

// status is the machine's power state as its engine reads it. A driver that
// cannot be asked is no stop (ADR 26-10-09-106 §2): the state last seen
// stands, and a machine never seen is an error rather than a guess.
func (r *MachineSandboxRuntime) status(ctx context.Context, sandboxID string, inst machine.Instance, record machineRecord) (Status, error) {
	switch r.engine.State(ctx, inst) {
	case engine.Running:
		return r.observe(sandboxID, StatusRunning), nil
	case engine.Stopped:
		r.urls.Delete(sandboxID)
		if !record.Started {
			return r.observe(sandboxID, StatusCreated), nil
		}
		return r.observe(sandboxID, StatusStopped), nil
	}
	if last, ok := r.lastObserved(sandboxID); ok {
		return last, nil
	}
	return "", fmt.Errorf("sandbox %s's machine: the %s driver cannot be asked for its state", sandboxID, r.engine.Driver.Name())
}

// sandbox is the runtime view of a sandbox's machine.
func (r *MachineSandboxRuntime) sandbox(ctx context.Context, sandboxID string, inst machine.Instance, record machineRecord) (*Sandbox, error) {
	status, err := r.status(ctx, sandboxID, inst, record)
	if err != nil {
		return nil, err
	}
	metadata := map[string]string{"driver": r.engine.Driver.Name(), "instance": inst.ID}
	if record.HarnessMode != "" {
		metadata[sandboxLabelHarnessMode] = record.HarnessMode
	}
	return &Sandbox{
		ID:        inst.ID,
		SandboxID: sandboxID,
		Status:    status,
		Image:     inst.Image,
		CreatedAt: inst.Created,
		Metadata:  metadata,
		Env:       copyMap(record.Env),
	}, nil
}

// GetSandbox is the sandbox's machine. An archived sandbox keeps its machine,
// whose disk is what archiving keeps, but has no box to use until it is
// created again, as a container's has none (powerTarget).
func (r *MachineSandboxRuntime) GetSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	if !r.hostsSandbox(sandboxID) || r.isArchived(sandboxID) {
		return nil, ErrNotFound
	}
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	return r.sandbox(ctx, sandboxID, inst, record)
}

// ListSandboxes is every sandbox of this pool with a machine, archived ones
// aside. A machine of the pool's project that no sandbox's record names is
// an orphan, which the volume reaper removes (WatchSandboxVolumes).
func (r *MachineSandboxRuntime) ListSandboxes(ctx context.Context) ([]*Sandbox, error) {
	ids, err := r.StoredSandboxIDs(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := r.engine.List(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]machine.Instance, len(instances))
	for _, inst := range instances {
		byName[inst.Name] = inst
	}
	out := make([]*Sandbox, 0, len(ids))
	for _, sandboxID := range ids {
		if r.isArchived(sandboxID) {
			continue
		}
		name, err := machineName(sandboxID)
		if err != nil {
			continue
		}
		inst, ok := byName[name]
		if !ok {
			continue
		}
		record, ok, err := r.readMachineRecord(sandboxID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// A machine whose state cannot be read fails the list rather than
		// leaving it out: what reads the list takes absence as no machine.
		sb, err := r.sandbox(ctx, sandboxID, inst, record)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, nil
}

// RunningSandboxIDsInMode reads what the runtime last saw, never the driver:
// it is called every second.
func (r *MachineSandboxRuntime) RunningSandboxIDsInMode(_ context.Context, harnessMode string) ([]string, error) {
	if harnessMode == "" || harnessMode == string(sandboxconfig.HarnessModeRun) {
		return nil, nil
	}
	var out []string
	var err error
	r.observed.Range(func(key, value any) bool {
		sandboxID, _ := key.(string)
		if status, _ := value.(Status); status != StatusRunning {
			return true
		}
		record, ok, readErr := r.readMachineRecord(sandboxID)
		if readErr != nil {
			err = readErr
			return false
		}
		if ok && record.HarnessMode == harnessMode {
			out = append(out, sandboxID)
		}
		return true
	})
	return out, err
}

// CreateSandbox makes the sandbox's machine, or makes it again: a spec that
// moved, or a project layer its primary source declares that it was not made
// with, places a new bootstrap in the same machine and restarts it, keeping
// its disk (rebuildInPlace). A create that finds the machine as it is settles
// it and starts it when asked, as a container's does.
func (r *MachineSandboxRuntime) CreateSandbox(ctx context.Context, req *workerapimodel.PoolSandboxCreateRequest) (result *Sandbox, resultErr error) {
	sandboxID := ""
	if req != nil {
		sandboxID = strings.TrimSpace(req.SandboxId)
	}
	if sandboxID == "" {
		return nil, errors.New("sandbox ID is required")
	}
	if err := validateCreateRequest(sandboxID, req); err != nil {
		return nil, err
	}
	if _, err := machineName(sandboxID); err != nil {
		return nil, err
	}
	// A machine boots a driver base, which knows nothing of discobox: the
	// agent is what the pool places on it, so a sandbox pinned to no agent
	// has nothing to run.
	if !delivery(req).PlacesAgent() {
		return nil, fmt.Errorf("sandbox %s: %w", sandboxID, ErrUndelivered)
	}
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	if err := r.harnessCreateAllowed(sandboxID, req.HarnessChangeId.Or("")); err != nil {
		return nil, err
	}
	defer func() {
		if resultErr == nil {
			resultErr = r.finishHarnessChange(sandboxID, req.HarnessChangeId.Or(""))
		}
	}()
	if err := assetcache.WriteRecord(r.sandboxRoot(sandboxID), delivery(req)); err != nil {
		return nil, fmt.Errorf("record sandbox %s's bundles: %w", sandboxID, err)
	}
	// A create of an archived sandbox is its unarchive: its machine, which
	// archiving kept, is made again from this create as a container would be
	// built, its disk as it stands (ADR 0022 §6).
	unarchive := r.isArchived(sandboxID)
	if err := clearSandboxArchiveMarker(r.sandboxRoot(sandboxID)); err != nil {
		return nil, fmt.Errorf("clear sandbox archive marker: %w", err)
	}
	var existing *Sandbox
	if !unarchive {
		var err error
		if existing, err = r.GetSandbox(ctx, sandboxID); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if existing != nil {
		record, _, err := r.readMachineRecord(sandboxID)
		if err != nil {
			return nil, err
		}
		if record.SpecFingerprint == strings.TrimSpace(optString(req.Config.SpecFingerprint)) {
			sb, done, err := r.resumeCreate(ctx, existing, req)
			if err != nil || done {
				return sb, err
			}
			slog.InfoContext(ctx, "making a sandbox machine again in place", "sandboxId", sandboxID,
				"reason", "the project configuration its delivered source declares")
		} else {
			slog.InfoContext(ctx, "making a sandbox machine again in place", "sandboxId", sandboxID,
				"reason", "a spec change", "running", existing.Status == StatusRunning)
		}
	}
	// A machine is made at most twice per create, as a container is built:
	// once on the project layer recorded so far, and once more when settling
	// reads a different one from the sandbox's primary source.
	for builds := 0; ; builds++ {
		sb, rebuild, err := r.buildMachine(ctx, sandboxID, req, existing)
		if err != nil || !rebuild {
			return sb, err
		}
		if builds > 0 {
			return nil, fmt.Errorf("sandbox %s: its project layer changed again after its machine was made again for it", sandboxID)
		}
		slog.InfoContext(ctx, "making a sandbox machine again in place", "sandboxId", sandboxID,
			"reason", "the project configuration its delivered source declares")
		existing = sb
	}
}

// resumeCreate is a create that finds the sandbox's machine made from its
// spec: it settles the sources a client delivered since, restarts a running
// machine whose pins moved, and starts one a lost create made and never
// started. It reports done unless the machine has to be made again for the
// project layer settling read.
func (r *MachineSandboxRuntime) resumeCreate(ctx context.Context, existing *Sandbox, req *workerapimodel.PoolSandboxCreateRequest) (*Sandbox, bool, error) {
	sandboxID := existing.SandboxID
	r.publishSandboxPhase(ctx, sandboxID, PhaseMaterializingSource)
	rebuild, err := r.settleSources(ctx, existing, req)
	if err != nil || rebuild {
		return nil, false, err
	}
	moved, err := r.pinsMoved(sandboxID, existing.ID)
	if err != nil {
		return nil, false, err
	}
	switch {
	case moved && existing.Status == StatusRunning:
		// Restarted into them, as a container is (ADR 26-10-24-953 §1). A
		// stopped machine boots on them at its next start.
		slog.InfoContext(ctx, "restarting sandbox into its moved bundles", "sandboxId", sandboxID)
		if err := r.stopLocked(ctx, sandboxID); err != nil {
			return nil, false, err
		}
		if err := r.startLocked(ctx, sandboxID); err != nil {
			return nil, false, err
		}
	case createStarts(req) && existing.Status == StatusCreated:
		// A create whose request was lost after its machine was made.
		if err := r.startLocked(ctx, sandboxID); err != nil {
			return nil, false, err
		}
		sb, err := r.GetSandbox(ctx, sandboxID)
		if err != nil {
			return nil, false, err
		}
		r.publishSandboxPhase(ctx, sandboxID, PhaseMaterializingSource)
		if rebuild, err := r.settleSources(ctx, sb, req); err != nil || rebuild {
			return nil, false, err
		}
	}
	sb, err := r.observedSandbox(ctx, sandboxID)
	return sb, true, err
}

// pinsMoved reports whether what the sandbox is delivered differs from what
// the pool last placed in its machine, instance.
func (r *MachineSandboxRuntime) pinsMoved(sandboxID, instance string) (bool, error) {
	pins, ok, err := r.deliveredPins(sandboxID)
	if err != nil || !ok {
		return false, err
	}
	placed, ok, err := r.readPlaced(sandboxID)
	if err != nil {
		return false, err
	}
	return !ok || placed.Container != instance || !samePins(placed.Pins, pins), nil
}

// buildMachine makes the sandbox's machine when it has none, records the
// bootstrap it is placed with, and starts it when the create asks or the
// machine was running. A machine it finds is made again in place: a new
// bootstrap, the same disk, and a restart if it ran (rebuildInPlace). It
// reports a rebuild when the project layer settling read is not the one the
// bootstrap was made with.
func (r *MachineSandboxRuntime) buildMachine(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxCreateRequest, existing *Sandbox) (*Sandbox, bool, error) {
	normalizeSandboxConfig(r.paths, &req.Config)
	project, err := r.readProjectLayerRecord(sandboxID)
	if err != nil {
		return nil, false, err
	}
	primary := ""
	if _, hasPrimary := req.Config.Source.Get(); hasPrimary {
		primary = sandboxSources(r.paths, req)[0].slug
	} else {
		project.Project = nil
	}
	project.Source = &primary
	if err := r.writeProjectLayerRecord(sandboxID, project); err != nil {
		return nil, false, err
	}
	r.publishSandboxPhase(ctx, sandboxID, PhasePreparingVolumes)
	proxyEnv, err := r.stageSandbox(sandboxID, req)
	if err != nil {
		return nil, false, err
	}
	record, _, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return nil, false, err
	}
	r.publishSandboxPhase(ctx, sandboxID, PhaseCreatingContainer)
	inst, err := r.ensureInstance(ctx, sandboxID)
	if err != nil {
		return nil, false, err
	}
	if record.Instance != inst.ID {
		// Made just now, or found by name after a create that was lost: this
		// machine has not been started by this record.
		record = machineRecord{Instance: inst.ID}
	}
	listen, err := r.agentListenAddress(ctx, inst)
	if err != nil {
		return nil, false, err
	}
	doc, err := buildSandboxDocument(r.paths, r.projectID, sandboxID, r.poolID, r.controlPlanePublicKey, r.poolPublicKey(), inst.Image, r.poolEndpoints, req, proxyEnv, project.Project)
	if err != nil {
		return nil, false, err
	}
	doc.Runtime.AgentRuntime.ListenAddress = listen
	doc.Runtime.AgentRuntime.TLS = r.agentTLS
	// Recorded beside the tree, as a container's are, so the credentials
	// broker is told the sandbox's variables without reading sandbox.json
	// back (publishCredentialEnvironment).
	if err := r.recordFileSecrets(sandboxID, doc.Image.Secrets); err != nil {
		return nil, false, err
	}
	secretEnv, _ := req.SecretEnv.Get()
	if err := r.publishCredentialEnvironment(sandboxID, cloneStringMap(secretEnv)); err != nil {
		return nil, false, err
	}
	user := resolveSandboxUser(r.paths, req)
	record.SpecFingerprint = strings.TrimSpace(optString(req.Config.SpecFingerprint))
	record.HarnessMode = harnessModeOf(req.Config)
	record.Env = envWithSandboxUser(nil, user)
	record.Bootstrap = doc
	if err := r.writeMachineRecord(sandboxID, record); err != nil {
		return nil, false, err
	}
	running := existing != nil && existing.Status == StatusRunning
	if running {
		if err := r.rebuildInPlace(ctx, sandboxID); err != nil {
			return nil, false, err
		}
	} else if existing != nil || !req.Config.Start.Or(true) {
		// A machine that was down stays down and boots on its new bootstrap
		// at its next start; a create not asked to start leaves it made.
		if !createStarts(req) {
			sb, err := r.observedSandbox(ctx, sandboxID)
			return sb, false, err
		}
		if err := r.startLocked(ctx, sandboxID); err != nil {
			return nil, false, err
		}
	} else if err := r.startLocked(ctx, sandboxID); err != nil {
		return nil, false, err
	}
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, false, err
	}
	r.publishSandboxPhase(ctx, sandboxID, PhaseMaterializingSource)
	rebuild, err := r.settleSources(ctx, sb, req)
	if err != nil || rebuild {
		return sb, rebuild, err
	}
	sb, err = r.observedSandbox(ctx, sandboxID)
	return sb, false, err
}

// keepsDataOnVolume reports whether a sandbox's machine keeps its data on a
// volume of its own, apart from its disk, so that making it again can destroy
// the machine and make a new one on the same volume. No disco-vm driver gives
// an instance a volume yet (discobox-ai/vm#23), so every machine is made again
// in place: its disk is all the sandbox has.
func (r *MachineSandboxRuntime) keepsDataOnVolume() bool { return false }

// rebuildInPlace makes a running sandbox's machine again on the bootstrap
// now recorded for it. A machine that keeps its data on a volume is destroyed
// and made anew on it; one that does not, which is every machine today
// (keepsDataOnVolume), keeps its disk, is placed again — sandbox.json, the
// agent, its units and pins — and restarted, so its agent boots on what was
// placed. The caller holds the sandbox's power lock.
func (r *MachineSandboxRuntime) rebuildInPlace(ctx context.Context, sandboxID string) error {
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return err
	}
	if r.keepsDataOnVolume() {
		inst, err := r.instance(ctx, sandboxID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			if err := r.engine.Remove(ctx, inst, true); err != nil {
				return fmt.Errorf("remove sandbox %s's machine to make it again: %w", sandboxID, err)
			}
		}
		record, _, err := r.readMachineRecord(sandboxID)
		if err != nil {
			return err
		}
		if inst, err = r.ensureInstance(ctx, sandboxID); err != nil {
			return err
		}
		record.Instance, record.Started = inst.ID, false
		if err := r.writeMachineRecord(sandboxID, record); err != nil {
			return err
		}
	}
	return r.startLocked(ctx, sandboxID)
}

// rebuildForProjectLayer makes a running sandbox's machine again on the
// project layer now recorded for it, with no create request in hand: the
// bootstrap the pool kept is placed with that layer in it.
func (r *MachineSandboxRuntime) rebuildForProjectLayer(ctx context.Context, sb *Sandbox) error {
	record, ok, err := r.readMachineRecord(sb.SandboxID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	layer, err := r.readProjectLayerRecord(sb.SandboxID)
	if err != nil {
		return err
	}
	record.Bootstrap.Project = layer.Project
	if err := r.writeMachineRecord(sb.SandboxID, record); err != nil {
		return err
	}
	slog.InfoContext(ctx, "making a sandbox machine again in place", "sandboxId", sb.SandboxID,
		"reason", "the project configuration its delivered source declares")
	return r.rebuildInPlace(ctx, sb.SandboxID)
}

// builtProjectLayer is the project layer the machine's recorded bootstrap
// carries.
func (r *MachineSandboxRuntime) builtProjectLayer(_ context.Context, sb *Sandbox) (string, error) {
	record, ok, err := r.readMachineRecord(sb.SandboxID)
	if err != nil || !ok {
		return "", err
	}
	return projectLayerDigest(record.Bootstrap.Project), nil
}

// takesRuntimeConfig is true of every machine: its bootstrap is always made
// naming this pool's key.
func (r *MachineSandboxRuntime) takesRuntimeConfig(_ context.Context, sandboxID string) (bool, error) {
	if !r.hostsSandbox(sandboxID) {
		return false, ErrNotFound
	}
	return true, nil
}

// ensureInstance is the sandbox's machine, made from the driver's base when
// it has none. A create whose answer was lost made one already, which is
// found by its name.
func (r *MachineSandboxRuntime) ensureInstance(ctx context.Context, sandboxID string) (machine.Instance, error) {
	inst, err := r.instance(ctx, sandboxID)
	if err == nil {
		return inst, r.fenced(inst)
	}
	if !errors.Is(err, ErrNotFound) {
		return inst, err
	}
	name, err := machineName(sandboxID)
	if err != nil {
		return machine.Instance{}, err
	}
	base, err := r.baseRef(ctx)
	if err != nil {
		return machine.Instance{}, err
	}
	opts := engine.CreateOptions{Name: name, Egress: r.egress}
	if r.isolate {
		opts.Isolated, opts.Networks = true, []string{r.network}
	}
	inst, err = r.engine.Create(ctx, base, opts)
	if errors.Is(err, machine.ErrExists) {
		if inst, err = r.instance(ctx, sandboxID); err != nil {
			return inst, err
		}
		return inst, r.fenced(inst)
	}
	if err != nil {
		// A create past the provider's quota fails here, and says so (ADR
		// 26-10-09-106 §4).
		return machine.Instance{}, fmt.Errorf("create sandbox %s's machine from %s: %w", sandboxID, base, err)
	}
	return inst, nil
}

// fenced refuses a machine found by name whose egress its driver does not
// report as the runtime's fence: one made before the fence, or by hand,
// would let its sandbox reach around the pool's proxy (ADR 26-10-29-481 §4).
// A machine the runtime makes is fenced by its create, which reads the fence
// back before it returns.
func (r *MachineSandboxRuntime) fenced(inst machine.Instance) error {
	if r.egress == nil || sameEgress(inst.Egress, r.egress) {
		return nil
	}
	return fmt.Errorf("sandbox machine %s is not fenced to its pool as this pool fences its sandboxes; delete the sandbox's machine so it is made again", inst.Name)
}

// sameEgress reports whether a machine's fence, as its driver reports it, is
// want: nil is no fence, and the allowed hosts compare as a set.
func sameEgress(got, want *machine.Egress) bool {
	if got == nil || want == nil {
		return got == want
	}
	allowed := make(map[string]bool, len(got.Allow))
	for _, host := range got.Allow {
		allowed[strings.ToLower(host)] = true
	}
	for _, host := range want.Allow {
		if !allowed[strings.ToLower(host)] {
			return false
		}
	}
	return len(allowed) == len(want.Allow)
}

// baseRef is the ID of the driver's base a sandbox's machine boots: the
// configured one, matched by ID alone, or the first the driver lists.
func (r *MachineSandboxRuntime) baseRef(ctx context.Context) (string, error) {
	driver := r.engine.Driver.Name()
	bases, err := r.engine.Driver.Bases(ctx)
	if err != nil {
		return "", fmt.Errorf("list the %s driver's bases: %w", driver, err)
	}
	if r.base == "" {
		if len(bases) == 0 {
			return "", fmt.Errorf("the %s driver offers no base to boot a sandbox's machine from", driver)
		}
		return bases[0].ID, nil
	}
	for _, base := range bases {
		if base.ID == r.base {
			return base.ID, nil
		}
	}
	return "", fmt.Errorf("the %s driver has no base with ID %q", driver, r.base)
}

// agentListenAddress is where the sandbox agent listens in the machine: the
// address the driver gives the instance's one service, which its URL reaches.
// The driver picks it, the same for every boot of the instance, so it is
// placed in the bootstrap once (disco-vm ADR 0005).
func (r *MachineSandboxRuntime) agentListenAddress(ctx context.Context, inst machine.Instance) (string, error) {
	service, err := r.engine.Driver.Service(ctx, inst)
	if err != nil && !errors.Is(err, machine.ErrNotRunning) {
		return "", fmt.Errorf("the service address of sandbox machine %s: %w", inst.Name, err)
	}
	address, ok := strings.CutPrefix(service.Listen, "tcp:")
	if !ok || address == "" {
		return "", fmt.Errorf("the %s driver gives machine %s's service %q, and the sandbox agent listens on TCP alone", r.engine.Driver.Name(), inst.Name, service.Listen)
	}
	return address, nil
}

// place puts in the running machine what its agent boots on — sandbox.json,
// and the agent layer when it is not there yet, its units and its pins — and
// runs `discobox-provision apply`, which starts the agent. It reports moved
// when the machine was placed before with something else, which the agent
// started on at boot: then the machine is restarted so it boots on this.
func (r *MachineSandboxRuntime) place(ctx context.Context, sandboxID string, inst machine.Instance, record machineRecord) (bool, error) {
	pins, ok, err := r.deliveredPins(sandboxID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("sandbox %s: %w", sandboxID, ErrUndelivered)
	}
	agent, ok, err := r.sandboxAgent(ctx, sandboxID)
	if err != nil {
		return false, fmt.Errorf("the agent of sandbox %s's pins %v: %w", sandboxID, pins, err)
	}
	if !ok {
		return false, fmt.Errorf("sandbox %s is pinned to %v, and this pool has no asset cache to place its agent from", sandboxID, pins)
	}
	// A machine takes the pool's setting at every start, since its bootstrap
	// is placed again: a bootstrap that moved with it restarts the machine
	// onto it below (ADR 26-10-29-481 §3).
	if record.Bootstrap.Runtime.AgentRuntime.TLS != r.agentTLS {
		record.Bootstrap.Runtime.AgentRuntime.TLS = r.agentTLS
		if err := r.writeMachineRecord(sandboxID, record); err != nil {
			return false, err
		}
	}
	bootstrap, bootstrapDigest, err := renderBootstrap(record)
	if err != nil {
		return false, err
	}
	// The keypair the agent serves its API over TLS with goes beside it, the
	// one the pool holds now, so a start places one due for renewal anew. It
	// is no part of the bootstrap's digest: a renewed keypair moves nothing
	// the machine must restart for.
	var keypair *sandboxconfig.AgentTLS
	if r.agentTLS {
		material, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID)
		if err != nil {
			return false, err
		}
		keypair = &material.Agent
	}
	placed, had, err := r.readPlaced(sandboxID)
	if err != nil {
		return false, err
	}
	had = had && placed.Container == inst.ID
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		return false, err
	}
	if err := guest.CopyTo(ctx, "/", bootstrapTree(bootstrap, keypair)); err != nil {
		return false, fmt.Errorf("place sandbox %s's bootstrap: %w", sandboxID, err)
	}
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(agentlayer.Archive(writer, agent, had && placed.Layer == agent.Digest, pins, agentlayer.Units))
	}()
	defer reader.Close()
	result, err := provisioner.Provision(ctx, guest, provisioner.Plan{
		Dir:     "/",
		Tree:    reader,
		Command: machine.ExecRequest{Argv: []string{agentlayer.AgentPath, "provision", "apply"}},
	})
	if err != nil {
		if tail := strings.TrimSpace(string(result.Stderr)); tail != "" {
			err = fmt.Errorf("%w: %s", err, tail)
		}
		return false, fmt.Errorf("provision sandbox %s's machine: %w", sandboxID, err)
	}
	moved := had && (placed.Layer != agent.Digest || !samePins(placed.Pins, pins) || placed.Bootstrap != bootstrapDigest)
	if err := r.writePlaced(sandboxID, placedRecord{Container: inst.ID, Layer: agent.Digest, Pins: pins, Bootstrap: bootstrapDigest}); err != nil {
		return false, err
	}
	// How the pool reaches the agent is what the bootstrap just placed says.
	return moved, r.recordAgentTLS(sandboxID, record.Bootstrap.Runtime.AgentRuntime.TLS)
}

// renderBootstrap is sandbox.json as record's bootstrap renders it, and its
// digest, which the placement record keeps.
func renderBootstrap(record machineRecord) ([]byte, string, error) {
	data, err := marshalSandboxDocument(record.Bootstrap)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// placementCurrent reports whether the pool's record says the machine
// instance holds what the sandbox boots on now: its agent layer, its pins and
// its bootstrap. It reads the pool's own records alone, and asks the machine
// nothing.
func (r *MachineSandboxRuntime) placementCurrent(ctx context.Context, sandboxID, instance string) (bool, error) {
	placed, ok, err := r.readPlaced(sandboxID)
	if err != nil || !ok || placed.Container != instance {
		return false, err
	}
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil || !ok {
		return false, err
	}
	_, digest, err := renderBootstrap(record)
	if err != nil {
		return false, err
	}
	pins, ok, err := r.deliveredPins(sandboxID)
	if err != nil || !ok || r.agentLayers == nil {
		return false, err
	}
	layer, ok, err := r.agentLayers.AgentLayer(ctx, sandboxID)
	if err != nil || !ok {
		return false, err
	}
	return placed.Layer == layer && samePins(placed.Pins, pins) && placed.Bootstrap == digest, nil
}

// bootstrapTree is sandbox.json where the agent reads it, as a tar placed at
// the machine's root: public, since the sandbox user reads it, and holding no
// secret (its runtime-config document is what carries them). Beside it is the
// keypair the agent serves its API over TLS with, where it does, its key
// root's alone.
func bootstrapTree(data []byte, keypair *sandboxconfig.AgentTLS) io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0)
	for _, dir := range []string{"etc/", "etc/discobox/"} {
		_ = tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch})
	}
	_ = tw.WriteHeader(&tar.Header{Name: "etc/discobox/" + sandboxDocumentName, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)), ModTime: epoch})
	_, _ = tw.Write(data)
	if keypair != nil {
		agentTLSDir := "etc/discobox/" + sandboxconfig.AgentTLSDirName + "/"
		_ = tw.WriteHeader(&tar.Header{Name: agentTLSDir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch})
		for _, file := range agentTLSFiles(*keypair) {
			_ = tw.WriteHeader(&tar.Header{Name: agentTLSDir + file.name, Typeflag: tar.TypeReg, Mode: int64(file.mode), Size: int64(len(file.data)), ModTime: epoch})
			_, _ = tw.Write(file.data)
		}
	}
	_ = tw.Close()
	return &buf
}

// SandboxDialer reaches the sandbox agent at its machine's private URL, where
// its pool's machine reaches it (ADR 26-10-29-481 §1), over TLS where its
// bootstrap told it to serve TLS (§3). A machine has one service, which is
// the agent's, so no other port in it is reached this way.
func (r *MachineSandboxRuntime) SandboxDialer(ctx context.Context, sandboxID string, port int) (Dialer, error) {
	if port != SandboxAgentPort {
		return nil, fmt.Errorf("%w: a machine sandbox is reached at its agent alone, not at port %d", ErrUnsupported, port)
	}
	target, err := r.agentURL(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	dial, err := urlDialer(target)
	if err != nil {
		return nil, err
	}
	if dial, err = r.agentDialer(sandboxID, dial); err != nil {
		return nil, err
	}
	return func(ctx context.Context) (net.Conn, error) {
		conn, err := dial(ctx)
		if err != nil {
			// The URL may be a start old: ask the driver again next time.
			r.urls.CompareAndDelete(sandboxID, target)
		}
		return conn, err
	}, nil
}

// agentURL is the private URL of the sandbox's machine while it runs: a
// direct path on the pool's network, or its public URL where the driver
// gives its machines no other.
func (r *MachineSandboxRuntime) agentURL(ctx context.Context, sandboxID string) (string, error) {
	if value, ok := r.urls.Load(sandboxID); ok {
		if target, ok := value.(string); ok {
			return target, nil
		}
	}
	if !r.hostsSandbox(sandboxID) {
		return "", ErrNotFound
	}
	inst, err := r.instance(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	target, err := r.engine.PrivateURL(ctx, inst)
	if err != nil {
		return "", err
	}
	r.urls.Store(sandboxID, target)
	return target, nil
}

// urlDialer opens a connection to the server at an http or https URL, which
// carries plain HTTP for the Dialer's transport: TLS, where the URL has it,
// is this side's to speak.
func urlDialer(target string) (Dialer, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port := u.Port()
	switch u.Scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	default:
		return nil, fmt.Errorf("a machine's URL %q is neither http nor https", target)
	}
	address := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if u.Scheme == "http" {
		return func(ctx context.Context) (net.Conn, error) { return dialer.DialContext(ctx, "tcp", address) }, nil
	}
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
	return func(ctx context.Context) (net.Conn, error) { return tlsDialer.DialContext(ctx, "tcp", address) }, nil
}

// machineAgentReadyTimeout bounds the wait for a machine's agent once the
// machine runs: `discobox-provision apply` has started it, and a VM's agent
// provisions the sandbox user before it listens (ADR 26-10-09-143 §1).
const machineAgentReadyTimeout = 5 * time.Minute

// machineAgentPollInterval paces that wait: every pass is a request over the
// network to the machine's URL.
const machineAgentPollInterval = 250 * time.Millisecond

// waitForSandboxAgent waits for the agent of a machine just started to answer
// at its URL, or for the machine to stop.
func (r *MachineSandboxRuntime) waitForSandboxAgent(ctx context.Context, sandboxID string) error {
	ctx, cancel := context.WithTimeout(ctx, machineAgentReadyTimeout)
	defer cancel()
	var lastErr error
	for {
		sb, err := r.GetSandbox(ctx, sandboxID)
		if err != nil {
			lastErr = err
		} else if err := sandboxAgentTerminalStateError(sb); err != nil {
			return err
		} else if dial, err := r.SandboxDialer(ctx, sandboxID, SandboxAgentPort); err != nil {
			lastErr = err
		} else if lastErr = sandboxAgentHealthy(ctx, dial); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("wait for sandbox-agent: %w", lastErr)
			}
			return ctx.Err()
		case <-time.After(machineAgentPollInterval):
		}
	}
}

// stopRefusedSandbox stops a machine whose agent refused the pool's
// runtime-config delivery outright.
func (r *MachineSandboxRuntime) stopRefusedSandbox(ctx context.Context, sandboxID string, refusal error) {
	slog.WarnContext(ctx, "stopping a sandbox that refuses the pool's runtime config", "sandboxId", sandboxID, "error", refusal)
	if err := r.stopMachine(ctx, sandboxID); err != nil {
		slog.WarnContext(ctx, "stop a sandbox that refuses the pool's runtime config", "sandboxId", sandboxID, "error", err)
	}
}

// observedSandbox reads the sandbox back and reports what it sees.
func (r *MachineSandboxRuntime) observedSandbox(ctx context.Context, sandboxID string) (*Sandbox, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	r.publishSandboxState(ctx, sandboxID, stateFromStatus(sb.Status))
	return sb, nil
}

// ArchiveSandbox stops the sandbox's machine and keeps it: its disk is the
// durable tree archiving keeps (ADR 0022 §6). What goes is the proxy
// material, which the unarchiving create stages again.
func (r *MachineSandboxRuntime) ArchiveSandbox(ctx context.Context, sandboxID string) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	root := r.sandboxRoot(sandboxID)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("archive sandbox %s: %w", sandboxID, ErrNotFound)
		}
		return fmt.Errorf("archive sandbox %s: %w", sandboxID, err)
	}
	// Marked before the machine stops, so nothing starts it in between.
	if err := writeSandboxArchiveMarker(root, time.Now()); err != nil {
		return fmt.Errorf("archive sandbox %s: %w", sandboxID, err)
	}
	if err := os.Remove(r.harnessFencePath(sandboxID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	r.publishSandboxState(ctx, sandboxID, StateStopping)
	if err := r.stopMachine(ctx, sandboxID); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := proxyagent.RemoveSandboxPolicy(r.root, r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	return proxyagent.RemoveSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID)
}

// DeleteSandbox removes the sandbox's machine, its proxy material and its
// tree, and returns only once the machine is gone: the control plane's delete
// is confirmed rather than accepted (ADR 0022 §3).
func (r *MachineSandboxRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	lock := r.sandboxLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	if err := r.removeMachine(ctx, sandboxID); err != nil {
		return err
	}
	r.observed.Delete(sandboxID)
	r.urls.Delete(sandboxID)
	if err := proxyagent.RemoveSandboxPolicy(r.root, r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	if err := proxyagent.RemoveSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID); err != nil {
		return err
	}
	// A machine pool's sandboxes build on it like any pool's (ADR
	// 26-10-27-767), so a purge takes their images with them here too.
	if err := r.removePublishedImages(sandboxID); err != nil {
		return err
	}
	if err := os.RemoveAll(r.sandboxRoot(sandboxID)); err != nil {
		return fmt.Errorf("remove sandbox data for %s: %w", sandboxID, err)
	}
	return nil
}

// removeMachine destroys the sandbox's machine and confirms it is gone.
func (r *MachineSandboxRuntime) removeMachine(ctx context.Context, sandboxID string) error {
	inst, err := r.instance(ctx, sandboxID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := r.engine.Remove(ctx, inst, true); err != nil {
		return fmt.Errorf("remove sandbox %s's machine: %w", sandboxID, err)
	}
	if _, err := r.instance(ctx, sandboxID); !errors.Is(err, ErrNotFound) {
		if err == nil {
			err = errors.New("its driver still has it")
		}
		return fmt.Errorf("confirm sandbox %s's machine is removed: %w", sandboxID, err)
	}
	return nil
}

// ExportTree is refused: a machine's tree is its disk, which this pool does
// not read out.
func (r *MachineSandboxRuntime) ExportTree(context.Context, string, assetcache.Delivery) (io.ReadCloser, error) {
	return nil, fmt.Errorf("%w: a machine sandbox's tree is its machine's disk, which a machine pool does not export yet", ErrUnsupported)
}

// ImportTree is refused, as its export is.
func (r *MachineSandboxRuntime) ImportTree(context.Context, string, io.Reader) error {
	return fmt.Errorf("%w: a machine pool does not import a sandbox's tree yet", ErrUnsupported)
}

// SyncKnownPools reaps nothing: a machine pool shares no host with another
// pool.
func (r *MachineSandboxRuntime) SyncKnownPools(context.Context, []string) error { return nil }

// ClearCache empties the caches of this pool: its proxy's response cache,
// and each running sandbox's own, which is on that sandbox's disk and which
// its agent empties in place (sandbox agent's cache/clear). Nothing is shared
// between machines, so no sandbox is stopped; a stopped one keeps its cache.
func (r *MachineSandboxRuntime) ClearCache(ctx context.Context) ([]string, error) {
	var errs []error
	if err := emptyDir(r.root.ProxyCache(r.projectID, r.poolID)); err != nil {
		errs = append(errs, fmt.Errorf("clear proxy response cache: %w", err))
	}
	sandboxes, err := r.ListSandboxes(ctx)
	if err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	for _, sb := range sandboxes {
		if sb.Status != StatusRunning {
			continue
		}
		if err := r.clearSandboxCache(ctx, sb.SandboxID); err != nil {
			errs = append(errs, fmt.Errorf("clear sandbox %s's caches: %w", sb.SandboxID, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	// Stopped none: the answer's list is required, and empty.
	return []string{}, nil
}

// clearSandboxCache asks the sandbox's agent to empty its caches, with a
// token the pool signs for that alone.
func (r *MachineSandboxRuntime) clearSandboxCache(ctx context.Context, sandboxID string) error {
	ctx, cancel := context.WithTimeout(ctx, runtimeConfigCallTimeout)
	defer cancel()
	token, err := r.poolToken(sandboxID, sandboxconfig.CacheClearScope)
	if err != nil {
		return err
	}
	dial, err := r.SandboxDialer(ctx, sandboxID, SandboxAgentPort)
	if err != nil {
		return err
	}
	target := HTTPURL(SandboxAgentPort, fmt.Sprintf("/api/projects/%s/sandboxes/%s/cache/clear", r.projectID, sandboxID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: dial.Transport()}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// WatchSandboxVolumes reaps the trees of sandboxes the control plane no
// longer holds, with their machines, and the pool's Git caches, as a
// container pool does (ADR 26-10-01-876), and removes the machines of the
// pool's project that no sandbox of this pool names: orphans of a pool whose
// records were lost (ADR 26-10-23-551 §1).
func (r *MachineSandboxRuntime) WatchSandboxVolumes(ctx context.Context, logger *slog.Logger, held HeldSandboxes) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(sandboxVolumeReapInterval)
	defer ticker.Stop()
	for {
		reapUnheldSandboxVolumes(ctx, r.sandboxesRoot(), held, sandboxVolumeRetention, time.Now(), logger, func(ctx context.Context, sandboxID string) error {
			lock := r.sandboxLock(sandboxID)
			lock.Lock()
			defer lock.Unlock()
			return r.removeMachine(ctx, sandboxID)
		})
		r.reapOrphanMachines(ctx, logger, held, time.Now())
		r.gitCache.SweepIfDue(ctx, gitCacheSweepInterval, gitcache.Held(held), gitcache.SweepOptions{
			NamespaceRetention: sandboxVolumeRetention,
			CacheRetention:     gitCacheRetention,
		}, time.Now(), logger)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// orphanMarkersName is the directory, in the pool's durable state, that
// holds when each orphan machine was first seen, by name.
const orphanMarkersName = "machine-orphans"

// reapOrphanMachines removes the machines of the pool's project that look
// like a sandbox's and that neither a tree of this pool nor a sandbox the
// control plane holds names, once they have been seen so for
// sandboxVolumeRetention, as an unheld tree is kept. Only a name a sandbox ID
// spells is considered (`sbx-…`): a machine of any other name in the project
// is nothing this runtime made, and is left alone however it got there. One
// the control plane holds is kept: its create finds it by name.
func (r *MachineSandboxRuntime) reapOrphanMachines(ctx context.Context, logger *slog.Logger, held HeldSandboxes, now time.Time) {
	instances, err := r.engine.List(ctx)
	if err != nil {
		logger.Warn("list the pool's machines", "error", err)
		return
	}
	stored, err := r.StoredSandboxIDs(ctx)
	if err != nil {
		logger.Warn("list the pool's sandbox trees", "error", err)
		return
	}
	heldCtx, cancel := context.WithTimeout(ctx, sandboxVolumeHeldTimeout)
	heldIDs, err := held(heldCtx)
	cancel()
	if err != nil {
		logger.Warn("list the sandboxes the control plane holds; removing no orphan machine this pass", "error", err)
		return
	}
	named := map[string]bool{}
	for _, id := range append(stored, heldIDs...) {
		if name, err := machineName(id); err == nil {
			named[name] = true
		}
	}
	markers := filepath.Join(r.root.PoolData(r.projectID, r.poolID), orphanMarkersName)
	orphans := map[string]bool{}
	for _, inst := range instances {
		if named[inst.Name] || !strings.HasPrefix(inst.Name, sandboxMachinePrefix) {
			continue
		}
		orphans[inst.Name] = true
		marker := filepath.Join(markers, inst.Name)
		seen, ok := readSandboxTombstone(marker)
		if !ok {
			logger.Warn("sandbox machine is named by no sandbox of this pool; removing it after retention", "machine", inst.Name, "retention", sandboxVolumeRetention.String())
			if err := os.MkdirAll(markers, 0o700); err != nil {
				logger.Warn("record an orphan sandbox machine", "machine", inst.Name, "error", err)
				continue
			}
			writeSandboxTombstone(marker, now, logger)
			continue
		}
		if now.Sub(seen) < sandboxVolumeRetention {
			continue
		}
		if err := r.engine.Remove(ctx, inst, true); err != nil {
			logger.Warn("remove an orphan sandbox machine", "machine", inst.Name, "error", err)
			continue
		}
		_ = os.Remove(marker)
		logger.Info("removed an orphan sandbox machine", "machine", inst.Name, "orphanedFor", now.Sub(seen).Truncate(time.Minute).String())
	}
	// A machine named again, or gone, starts no clock.
	entries, _ := os.ReadDir(markers)
	for _, entry := range entries {
		if !orphans[entry.Name()] {
			_ = os.Remove(filepath.Join(markers, entry.Name()))
		}
	}
}

// sandboxMachinePrefix begins the name of every sandbox's machine: a sandbox
// ID's prefix, spelled as machineName spells it.
const sandboxMachinePrefix = id.PrefixSandbox + "-"

// WatchProxyMaterial reclaims the proxy material of sandboxes with no
// machine, on an interval: there is no event stream to watch.
func (r *MachineSandboxRuntime) WatchProxyMaterial(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(proxyMaterialBackstopInterval)
	defer ticker.Stop()
	for {
		r.reconcileSandboxMaterial(ctx, logger)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// machineStatePollInterval paces the state channel's look at the pool's
// machines: the driver has no event stream, so a transition nothing here
// made — a machine that powered itself off — is seen by looking.
const machineStatePollInterval = 10 * time.Second

// WatchSandboxStates runs the state channel until ctx ends: each poll
// publishes what changed since the last, and every sandboxStateSyncInterval
// a complete sync. A poll that cannot read every machine publishes nothing,
// since a sync built without one would report it gone.
func (r *MachineSandboxRuntime) WatchSandboxStates(ctx context.Context, logger *slog.Logger, publish func(context.Context, SandboxStateBatch) error) {
	if logger == nil {
		logger = slog.Default()
	}
	if publish == nil {
		return
	}
	r.statePublisher.Store(publish)
	defer r.statePublisher.Store((func(context.Context, SandboxStateBatch) error)(nil))
	ticker := time.NewTicker(machineStatePollInterval)
	defer ticker.Stop()
	last := map[string]string{}
	var synced time.Time
	for {
		sandboxes, err := r.ListSandboxes(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("list sandbox machines for the state channel", "error", err)
			}
		} else {
			now := time.Now()
			complete := now.Sub(synced) >= sandboxStateSyncInterval
			batch := SandboxStateBatch{Complete: complete, ReportedAt: now.UTC()}
			seen := make(map[string]string, len(sandboxes))
			for _, sb := range sandboxes {
				state := stateFromStatus(sb.Status)
				seen[sb.SandboxID] = state
				if complete || last[sb.SandboxID] != state {
					batch.States = append(batch.States, SandboxStateObservation{SandboxID: sb.SandboxID, State: state})
				}
			}
			for sandboxID := range last {
				if _, ok := seen[sandboxID]; !ok && !complete {
					batch.States = append(batch.States, SandboxStateObservation{SandboxID: sandboxID, State: StateStopped})
				}
			}
			if complete || len(batch.States) > 0 {
				if err := publish(ctx, batch); err != nil {
					if ctx.Err() == nil {
						logger.Warn("publish sandbox machine states", "error", err)
					}
				} else {
					last = seen
					if complete {
						synced = now
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
