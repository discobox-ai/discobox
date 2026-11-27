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
	"sync/atomic"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/x/id"
	"golang.org/x/sync/singleflight"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
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
// that was lost is found by name. A sandbox's machine is made fresh from the
// driver's base, or, where the driver commits, from its plan's snapshot
// (machinesnapshot.go), and every start places the platform bundle's agent layer, its units, its pins
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
	CompleteSuspend       func(context.Context, string, string, int64) error
	AuthorizeStart        func(context.Context, string, int64) error
	ProjectID             string
	PoolID                string
	ControlPlanePublicKey string
	// AgentTLS has every sandbox serve its agent's API over TLS, the pool's
	// setting, which a machine takes at its next start (ADR 26-10-29-481
	// §3).
	AgentTLS bool
	// Root is where the runtime reads and writes pool state.
	Root layout.Root
	// Engine is the pool's own disco-vm engine, in the pool's disco-vm pool.
	Engine *engine.Engine
	// Base is the driver's base as its configuration names it
	// (machine.Config.Base): what a fresh machine starts from, which the
	// driver is made with. The runtime only keys snapshots by it, so a base
	// moved is a snapshot built again.
	Base string
	// HoldToPool holds every sandbox's machine to its pool the way its
	// driver carries a pool (PoolFencing; ADR 26-10-23-551 §4, ADR
	// 26-11-16-619 §1): on Network alone on a driver that fences on
	// networks; on no network, with Egress closing DNS, on one that fences at
	// its edge. Only a driver that is not Bare already, a stripping one, is
	// asked to strip a machine (619 §1's Isolate, kept as the runtime's
	// bare). A driver that carries no pool is refused for it.
	HoldToPool bool
	// Network is the provider's network the pool's machine and its sandboxes'
	// machines share, and no other machine of the provider's. It is empty on
	// a driver that fences at its edge, which has none.
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
	// PoolEdge is how each sandbox reaches the edge the pool sits behind, as
	// it is when a sandbox's runtime-config document is decided: the
	// address it dials in place of the edge's name and the pool listener's
	// credential (ADR 26-11-16-619 §§2–3). Nil for a pool its sandboxes reach
	// directly.
	PoolEdge func() *sandboxconfig.RuntimeEdge
	// IdentityKey is the pool's identity key, which signs the runtime-config
	// documents the pool delivers its sandboxes (ADR 26-10-08-127 §3).
	IdentityKey ed25519.PrivateKey
	// Platform is the one platform the pool hosts (ADR 0145 §1).
	Platform platform.Platform
	// AgentLayers is the pool's asset cache, which a sandbox's agent layer is
	// read from.
	AgentLayers AgentLayers
	// Audit is the pool proxy's audit, which a purge takes the sandbox's
	// trail from. It is required: a purge that could not take it would
	// report the sandbox's data gone while its recorded traffic is not.
	Audit AuditPurger
}

// MachineSandboxRuntime runs a pool's sandboxes as disco-vm machines.
type MachineSandboxRuntime struct {
	sandboxCore
	engine  *engine.Engine
	base    string
	bare    bool
	network string
	egress  *machine.Egress
	// observed is each sandbox's power state as the runtime last saw it,
	// which answers what the driver cannot say for now (an unreachable
	// service).
	observed sync.Map
	// endpoints caches each running sandbox's pool endpoint until a dial
	// fails, the machine changes state, or its edge credential needs renewal.
	endpoints sync.Map // sandbox ID → *machineEndpointCache
	// resuming holds each sandbox the state poll is resuming from its
	// provider's pause, so a slow resume is not begun again.
	resuming sync.Map // sandbox ID → struct{}
	// resumeFailed is when the state poll's last resume of each sandbox
	// failed, which holds its next off for resumeRetryInterval.
	resumeFailed sync.Map // sandbox ID → time.Time
}

var _ Runtime = (*MachineSandboxRuntime)(nil)

// NewMachineSandboxRuntime is a machine pool's runtime over its engine.
func NewMachineSandboxRuntime(cfg MachineSandboxRuntimeConfig) (*MachineSandboxRuntime, error) {
	if cfg.Engine == nil {
		return nil, errors.New("a machine runtime needs its engine")
	}
	if cfg.Audit == nil {
		return nil, errors.New("a machine runtime needs the pool proxy's audit, which a purge takes a sandbox's trail from")
	}
	var bare bool
	var network string
	if cfg.HoldToPool {
		caps := cfg.Engine.Driver.Capabilities()
		fencing, err := PoolFencing(cfg.Engine.Driver.Name(), caps)
		if err != nil {
			return nil, err
		}
		network = strings.TrimSpace(cfg.Network)
		switch {
		case fencing == FencedOnNetwork && network == "":
			return nil, errors.New("an isolated sandbox machine needs the pool's network, or it reaches nothing")
		case fencing == FencedAtEdge && network != "":
			return nil, fmt.Errorf("the %s driver fences a sandbox's machine at its provider's edge, on no network, and was given the network %s", cfg.Engine.Driver.Name(), network)
		case fencing == FencedAtEdge && (cfg.Egress == nil || len(cfg.Egress.Allow) == 0 || !cfg.Egress.CloseDNS):
			return nil, fmt.Errorf("the %s driver fences a sandbox's machine at its provider's edge, which needs its egress fenced to the pool's host with DNS closed", cfg.Engine.Driver.Name())
		}
		// Only a driver that gives a machine something of the account is
		// asked to strip it: one that gives nothing is Bare already.
		bare = caps.Bare == machine.BareStrip
	}
	if cfg.Egress != nil && !cfg.Engine.Driver.Capabilities().Fences(cfg.Egress) {
		return nil, fmt.Errorf("the %s driver cannot fence a sandbox's machine's egress to its pool (ADR 26-10-29-481 §4)", cfg.Engine.Driver.Name())
	}
	r := &MachineSandboxRuntime{
		sandboxCore: sandboxCore{
			paths:                 sandboxpath.For(cfg.Platform),
			projectID:             cfg.ProjectID,
			poolID:                cfg.PoolID,
			controlPlanePublicKey: cfg.ControlPlanePublicKey,
			authorizeStart:        cfg.AuthorizeStart,
			completeSuspend:       cfg.CompleteSuspend,
			agentTLS:              cfg.AgentTLS,
			sandboxIdleTimeout:    cfg.SandboxIdleTimeout,
			poolEndpoints:         cfg.PoolEndpoints,
			poolEdge:              cfg.PoolEdge,
			identityKey:           cfg.IdentityKey,
			root:                  cfg.Root,
			gitCache:              gitcache.New(cfg.Root.PoolGitCache(cfg.ProjectID, cfg.PoolID)),
			agentLayers:           cfg.AgentLayers,
			audit:                 cfg.Audit,
		},
		engine:  cfg.Engine,
		base:    strings.TrimSpace(cfg.Base),
		bare:    bare,
		network: network,
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
	// Env is the sandbox user's environment, which names who owns what the
	// pool serves the sandbox (GitOriginPath).
	Env map[string]string `json:"env,omitempty"`
	// Bootstrap is what sandbox.json is rendered from.
	Bootstrap sandboxconfig.Document `json:"bootstrap"`
	// Started reports a machine started since it was made: one that was not
	// is created, not stopped.
	Started bool `json:"started,omitempty"`
	// Snapshot is the key of the snapshot the machine's root was made from,
	// at its create or by its last new root; empty is a root made fresh. A
	// machine made from one whose agent never answers on its first start
	// drops the snapshot and is made again fresh (ADR 26-11-03-986 §6,
	// remakeFresh); a new root from one whose agent never answers is given a
	// fresh root in its place, and the snapshot stands (remakeOnFreshRoot).
	Snapshot string `json:"snapshot,omitempty"`
	// NewRoot asks that the machine be given a new root before it next
	// boots, by a rebuild of a machine that keeps its data on a volume
	// (remakeOnNewRoot). It is recorded before the driver is asked and
	// dropped once the driver has given the root, so an upgrade cut short is
	// finished by the next start (disco-vm ADR 26-11-03-782 §3).
	NewRoot bool `json:"newRoot,omitempty"`
	// NewRootSnapshot is the key of the plan's snapshot the new root is a
	// copy of, as the rebuild that asked for it found it (snapshotFor);
	// empty is a fresh root from the driver's base.
	NewRootSnapshot string `json:"newRootSnapshot,omitempty"`
	// NewRootAsked records that the driver was asked for the new root with
	// NewRootSnapshot as it stands: an upgrade cut short is finished only
	// with the From it was begun with, so the next start asks again with
	// the same one rather than looking the snapshot up again.
	NewRootAsked bool `json:"newRootAsked,omitempty"`
	// DataOnVolume is that the machine's sandbox agent keeps the sandbox's
	// data on the machine's data volume, its working root included, as the
	// pool read from the volume (recordDataOnVolume): only then is it given a
	// new root. Its key is not the "dataOnVolume" a pool from before ADR
	// 26-11-14-302 wrote, which did not ask about the working root: a record
	// with only that key reads as false, so a machine is rebuilt on the root
	// it has until a start on this pool reads its volume again (§4).
	DataOnVolume bool `json:"dataAndWorkingRootOnVolume,omitempty"`
	// Frozen is that the pool froze the machine (freezeLocked): written,
	// under the power lock, before the driver is asked, and cleared if the
	// freeze fails, once a thaw's after-thaw steps have run (thawedLocked),
	// or once the machine is seen stopped. Only a paused machine this says
	// the pool froze is suspended; any other paused machine is one its
	// provider paused (ADR 26-11-21-395 §4).
	Frozen bool `json:"frozen,omitempty"`
}

// snapshotProvenName marks, in a sandbox's tree, that the agent of its
// machine made from a snapshot has answered: from then on the machine holds
// the sandbox's work, and nothing blames the snapshot or makes the machine
// again. It is written where the answer is seen (waitForSandboxAgent), on
// its own, so no caller going away or record written beside it loses it.
const snapshotProvenName = "snapshot-proven"

func (r *MachineSandboxRuntime) snapshotProvenPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), snapshotProvenName)
}

// snapshotProven reports whether the sandbox's machine's agent has answered
// since it was made from a snapshot.
func (r *MachineSandboxRuntime) snapshotProven(sandboxID string) bool {
	_, err := os.Stat(r.snapshotProvenPath(sandboxID))
	return err == nil
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
	return fsdurable.WriteFile(path, data, 0o600)
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
//
// A paused machine (disco-vm ADR 26-11-21-438) is StatusSuspended where the
// pool's record says the pool froze it (ADR 26-11-21-395 §4). Any other is one
// its provider paused (Archil at a sandbox's session limit): StatusPaused,
// held, with its memory, running nothing, and resumed by a start
// (engine.Start). The pool froze nothing, so it is not StatusSuspended (ADR
// 26-11-16-619 §5), but it is reported as a pause, not as running
// (stateFromStatus). Reading either resumes nothing, and its endpoint goes,
// since a paused machine dials nothing.
func (r *MachineSandboxRuntime) status(ctx context.Context, sandboxID string, inst machine.Instance, record machineRecord) (Status, error) {
	switch r.engine.State(ctx, inst) {
	case engine.Running:
		return r.observe(sandboxID, StatusRunning), nil
	case engine.Paused:
		r.endpoints.Delete(sandboxID)
		if record.Frozen {
			return r.observe(sandboxID, StatusSuspended), nil
		}
		return r.observe(sandboxID, StatusPaused), nil
	case engine.Stopped:
		r.endpoints.Delete(sandboxID)
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
	return &Sandbox{
		ID:        inst.ID,
		SandboxID: sandboxID,
		Status:    status,
		Image:     r.snapshotBase(),
		CreatedAt: inst.Created,
		Metadata:  metadata,
		Env:       copyMap(record.Env),
		// A machine record always says: a machine made before snapshots
		// were is one made fresh.
		StartedFrom: &StartedFrom{Snapshot: record.Snapshot},
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
// aside. A machine of the pool's disco-vm pool that no sandbox's record names is
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
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return nil, err
	}
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
				"reason", "a spec change", "running", existing.Status == StatusRunning || existing.Status == StatusSuspended)
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
	case moved && (existing.Status == StatusRunning || existing.Status == StatusSuspended):
		// Restarted into them, as a container is (ADR 26-10-24-953 §1), a
		// frozen one too, as a frozen container is replaced. A stopped
		// machine boots on them at its next start.
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
	proxyEnv, err := r.stageSandbox(ctx, sandboxID, req)
	if err != nil {
		return nil, false, err
	}
	record, _, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return nil, false, err
	}
	r.publishSandboxPhase(ctx, sandboxID, PhaseCreatingContainer)
	inst, snapshot, err := r.ensureInstance(ctx, sandboxID, req)
	if err != nil {
		return nil, false, err
	}
	if record.Instance != inst.ID {
		// Made just now, or found by name after a create that was lost: this
		// machine has not been started by this record. An upgrade it asked
		// for stays asked for: a driver may give the machine a new ID part
		// way through one.
		record = machineRecord{Instance: inst.ID, Snapshot: snapshot, NewRoot: record.NewRoot, NewRootSnapshot: record.NewRootSnapshot, NewRootAsked: record.NewRootAsked}
	}
	if existing != nil {
		r.remakeOnNewRoot(ctx, inst, &record, req)
	}
	listen, err := r.agentListenAddress(ctx, inst)
	if err != nil {
		return nil, false, err
	}
	doc, err := buildSandboxDocument(r.paths, r.projectID, sandboxID, r.poolID, r.controlPlanePublicKey, r.poolPublicKey(), r.snapshotBase(), r.poolEndpoints, req, proxyEnv, project.Project)
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
	record.Env = envWithSandboxUser(nil, user)
	record.Bootstrap = doc
	if err := r.writeMachineRecord(sandboxID, record); err != nil {
		return nil, false, err
	}
	// A frozen machine is restarted on its new bootstrap as a running one
	// is, as a frozen container is replaced running.
	running := existing != nil && (existing.Status == StatusRunning || existing.Status == StatusSuspended)
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
// volume of its own, apart from its root, so that making it again gives it a
// new root and keeps the volume (disco-vm ADR 26-11-03-782): the machine has
// a data volume, and the pool read from it, at the machine's last start,
// that its sandbox agent wired the sandbox's declared volumes and sources onto
// it (record.DataOnVolume, read at every start by recordDataOnVolume; the
// data contract, pool-agent/DESIGN.md). It is the machine's, not the driver's: a machine
// made before its driver attached a data volume, or whose pinned agent
// predates the wiring, has its data on its root, and is made again in place.
func (r *MachineSandboxRuntime) keepsDataOnVolume(inst machine.Instance, record machineRecord) bool {
	return record.DataOnVolume && inst.Data != nil && r.engine.Driver.Capabilities().Attaches(machine.VolumeData)
}

// recordDataOnVolume reads, from the running machine, whether its sandbox
// agent keeps the sandbox's data on its data volume this start
// (sandboxconfig.MachineDataWiredFile, naming this sandbox and the token of
// the start the machine is on, sandboxconfig.MachineWiredFile), and records
// the answer in machine.json at every start, either way: the agent is the
// sandbox's pin, and the start that placed it may have moved it to one that
// wires nothing, which leaves the last start's line on the volume. Only a
// sandbox's own agent writes them, and saying so falsely costs that sandbox
// alone: what it keeps off the volume goes with its next root.
func (r *MachineSandboxRuntime) recordDataOnVolume(ctx context.Context, sandboxID string, inst machine.Instance) error {
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil || !ok {
		return err
	}
	onVolume := false
	if inst.Data != nil {
		if onVolume, err = r.wiredThisStart(ctx, sandboxID, inst); err != nil {
			return err
		}
	}
	if onVolume == record.DataOnVolume {
		return nil
	}
	record.DataOnVolume = onVolume
	return r.writeMachineRecord(sandboxID, record)
}

// wiredThisStart reports whether the running machine's data volume says its
// agent wired it for sandboxID on the start the machine is on: the volume's
// line names the token this start's /run holds. Both are read as files,
// bounded, and nothing is run in the guest, whose commands are the box's.
func (r *MachineSandboxRuntime) wiredThisStart(ctx context.Context, sandboxID string, inst machine.Instance) (bool, error) {
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		return false, err
	}
	token, ok := readGuestFile(ctx, guest, sandboxconfig.MachineWiredFile)
	if !ok || token == "" {
		// None this start: an agent that wires nothing.
		return false, nil
	}
	wired, ok := readGuestFile(ctx, guest, sandboxconfig.MachineDataWiredFile)
	// Its working root too: a line from an agent that left it on the root
	// keeps the machine on the root it has (ADR 26-11-14-302 §4).
	return ok && wired == sandboxconfig.MachineDataWiredLine(sandboxID, token, true), nil
}

// guestRecordReadTimeout bounds a read of one of the agent's records from
// its guest, which the box could hold open.
const guestRecordReadTimeout = 30 * time.Second

// readGuestFile is a small record file of the guest at path, trimmed, read
// through the guest's copy with its size and time bounded: the box writes it
// and can make it anything. Not there, or not a small regular file, is not
// ok.
func readGuestFile(ctx context.Context, guest machine.Guest, path string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, guestRecordReadTimeout)
	defer cancel()
	rc, err := guest.CopyFrom(ctx, path)
	if err != nil {
		return "", false
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	header, err := tr.Next()
	if err != nil || header.Typeflag != tar.TypeReg || header.Size > 256 {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(tr, 256))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

// remakeOnNewRoot records that a sandbox's machine being made again is given
// a new root at its next start (upgradeMachine) when it keeps its data on a
// volume, as a container is built again from its image: a copy of its plan's
// snapshot where req has one (snapshotFor), and otherwise a fresh one from
// the driver's base, which a nil req, a rebuild with no create in hand,
// always is. One that does not keep its data on a volume keeps its root,
// which is all the sandbox has, and is placed again on it. A new root already
// asked for keeps the root it was asked from: the driver may have begun it,
// and finishes it only from that one (a pool from before newRootSnapshot
// asked every new root fresh).
func (r *MachineSandboxRuntime) remakeOnNewRoot(ctx context.Context, inst machine.Instance, record *machineRecord, req *workerapimodel.PoolSandboxCreateRequest) {
	if record.NewRoot || !r.keepsDataOnVolume(inst, *record) {
		return
	}
	slog.InfoContext(ctx, "giving a sandbox machine a new root at its next start, keeping its volumes", "machine", inst.Name)
	record.NewRoot, record.NewRootSnapshot, record.NewRootAsked = true, "", false
	if req == nil {
		return
	}
	// Only looked up here: the start that gives the root holds it and reads
	// it again (newRootFrom), since it may be gone by then.
	_, key, release := r.snapshotFor(ctx, req)
	release()
	record.NewRootSnapshot = key
}

// rebuildInPlace makes a running sandbox's machine again on the bootstrap
// now recorded for it: it is stopped and started, and its start gives it the
// new root its record asks for (remakeOnNewRoot), or places again on the
// root it has — sandbox.json, the agent, its units and pins — so its agent
// boots on what was placed. The caller holds the sandbox's power lock.
func (r *MachineSandboxRuntime) rebuildInPlace(ctx context.Context, sandboxID string) error {
	if err := r.stopLocked(ctx, sandboxID); err != nil {
		return err
	}
	return r.startLocked(ctx, sandboxID)
}

// upgradeMachine gives the stopped machine the new root its record asks for
// (disco-vm ADR 26-11-03-782): a copy of its plan's snapshot where the record
// names one that is still published (newRootFrom), and otherwise a fresh one
// from the driver's base. It keeps its name, its settings and its data and
// cache volumes, and its old root is gone. The pool's record of what it
// placed goes with that root, so the start that follows places everything
// again, the agent layer with it. Which root it is asked for is recorded
// before the driver is asked, so an upgrade that fails part way fails the
// start and is finished by the next with the same one. A snapshot gone
// before the driver copied it is a fresh root instead. One the driver
// refuses outright (ErrUnsupported: a driver that attaches no data volume
// now, or has no base) is dropped, and the machine is placed again on the
// root it has, as a machine without a volume is.
func (r *MachineSandboxRuntime) upgradeMachine(ctx context.Context, sandboxID string, inst machine.Instance, record *machineRecord) (machine.Instance, error) {
	from, release, err := r.newRootFrom(ctx, sandboxID, record)
	if err != nil {
		return inst, err
	}
	defer release()
	if !record.NewRootAsked {
		record.NewRootAsked = true
		if err := r.writeMachineRecord(sandboxID, *record); err != nil {
			return inst, err
		}
	}
	upgraded, err := r.engine.Upgrade(ctx, inst.Name, engine.UpgradeOptions{From: from})
	if from != "" && (errors.Is(err, engine.ErrNotFound) || errors.Is(err, machine.ErrNotFound)) {
		// The snapshot went before the driver copied it, which leaves the
		// machine as it was: a fresh root, recorded before it is asked for.
		slog.WarnContext(ctx, "a snapshot is gone; giving the sandbox's machine a fresh root", "snapshot", from, "sandboxId", sandboxID, "error", err)
		record.NewRootSnapshot = ""
		if err := r.writeMachineRecord(sandboxID, *record); err != nil {
			return inst, err
		}
		upgraded, err = r.engine.Upgrade(ctx, inst.Name, engine.UpgradeOptions{})
	}
	switch {
	case errors.Is(err, machine.ErrUnsupported):
		slog.WarnContext(ctx, "the driver gives a sandbox machine no new root; placing it again on the one it has", "sandboxId", sandboxID, "error", err)
		upgraded = inst
	case err != nil:
		return inst, fmt.Errorf("give sandbox %s's machine a new root: %w", sandboxID, err)
	default:
		slog.InfoContext(ctx, "gave a sandbox machine a new root, keeping its volumes", "sandboxId", sandboxID, "snapshot", record.NewRootSnapshot)
		if err := fsdurable.Remove(r.placedRecordPath(sandboxID)); err != nil {
			return inst, err
		}
		// The driver keeps a machine's service across a stop and start, and
		// need not across a new root: the bootstrap placed on it names the
		// one the machine has now.
		listen, err := r.agentListenAddress(ctx, upgraded)
		if err != nil {
			return inst, err
		}
		record.Bootstrap.Runtime.AgentRuntime.ListenAddress = listen
		// A new root is unproven: one from a snapshot whose agent never
		// answers blames the snapshot (startLocked), and a fresh one has
		// nothing of a snapshot left to blame.
		if err := os.Remove(r.snapshotProvenPath(sandboxID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return inst, err
		}
		record.Snapshot = record.NewRootSnapshot
	}
	record.Instance, record.NewRoot = upgraded.ID, false
	record.NewRootSnapshot, record.NewRootAsked = "", false
	return upgraded, r.writeMachineRecord(sandboxID, *record)
}

// newRootFrom is the snapshot VM the machine's new root is a copy of, held
// until release is called so nothing destroys it while the driver copies it,
// and empty for a fresh root. Only the VM the plan's done record names is
// copied (snapshotState; ADR 26-11-03-986 §§4-5). Not yet asked of the
// driver, a snapshot not published now is a fresh root, recorded in record.
// Asked already, the driver may have begun the root and finishes it only
// from the same VM: it is asked again from it while it is published, and
// while no VM has its name and no build of the plan is under way, when the
// driver either finishes without it or says it is not found
// (upgradeMachine); a VM of the name no record names, or a build under way
// that may make one, fails the start, and the next start asks again.
func (r *MachineSandboxRuntime) newRootFrom(ctx context.Context, sandboxID string, record *machineRecord) (string, func(), error) {
	key := record.NewRootSnapshot
	if key == "" {
		return "", func() {}, nil
	}
	name := snapshotMachineName(key)
	held := r.holdSnapshot(key)
	state, _, err := r.snapshotState(ctx, key)
	switch {
	case err != nil:
		held()
		return "", nil, fmt.Errorf("look up snapshot %s for sandbox %s's new root: %w", name, sandboxID, err)
	case state == snapshotPublished:
		return name, held, nil
	case record.NewRootAsked && state == snapshotAbsent && !r.buildingSnapshots()[key]:
		// A build already past its hold check makes a VM of this name next:
		// no VM now is no snapshot only while none is under way.
		return name, held, nil
	case record.NewRootAsked:
		held()
		return "", nil, fmt.Errorf("sandbox %s's new root was begun from snapshot %s, which is being built again; it is finished at a later start", sandboxID, name)
	}
	held()
	slog.WarnContext(ctx, "the snapshot a sandbox machine's new root was to be copied from is gone; giving it a fresh root", "snapshot", name, "sandboxId", sandboxID)
	record.NewRootSnapshot = ""
	return "", func() {}, nil
}

// remakeMachine destroys the sandbox's machine and makes a new one, fresh
// from the driver's base, and records it as made and never started: its
// bootstrap names where the new machine's agent listens. The caller holds
// the sandbox's power lock.
func (r *MachineSandboxRuntime) remakeMachine(ctx context.Context, sandboxID string) error {
	if err := r.removeMachine(ctx, sandboxID); err != nil {
		return err
	}
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoContainer
	}
	inst, _, err := r.ensureInstance(ctx, sandboxID, nil)
	if err != nil {
		return err
	}
	listen, err := r.agentListenAddress(ctx, inst)
	if err != nil {
		return err
	}
	if err := os.Remove(r.snapshotProvenPath(sandboxID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	record.Instance, record.Started, record.Snapshot = inst.ID, false, ""
	// A new machine: nothing of the old one's root or volume is left.
	record.NewRoot, record.NewRootSnapshot, record.NewRootAsked, record.DataOnVolume = false, "", false, false
	record.Bootstrap.Runtime.AgentRuntime.ListenAddress = listen
	return r.writeMachineRecord(sandboxID, record)
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
	inst, err := r.instance(ctx, sb.SandboxID)
	if err != nil {
		return err
	}
	r.remakeOnNewRoot(ctx, inst, &record, nil)
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

// ensureInstance is the sandbox's machine, made when it has none: from its
// plan's snapshot where there is one (snapshotFor), and otherwise fresh from
// the driver's base, which a nil req always is. It is made at the size req
// sends, which is recorded for a machine made again with no request in hand,
// or else at the size recorded (ADR 26-11-23-445 §§2, 5). It returns the key
// of the snapshot it was made from, empty for none. A create whose answer was
// lost made one already, which is found by its name.
func (r *MachineSandboxRuntime) ensureInstance(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxCreateRequest) (machine.Instance, string, error) {
	size := requestedSize(req)
	if req != nil {
		if err := r.recordBoxSize(sandboxID, size); err != nil {
			return machine.Instance{}, "", err
		}
	} else {
		recorded, err := r.recordedBoxSize(sandboxID)
		if err != nil {
			return machine.Instance{}, "", err
		}
		size = recorded
	}
	inst, err := r.instance(ctx, sandboxID)
	if err == nil {
		return inst, "", r.fenced(inst)
	}
	if !errors.Is(err, ErrNotFound) {
		return inst, "", err
	}
	name, err := machineName(sandboxID)
	if err != nil {
		return machine.Instance{}, "", err
	}
	opts := r.createOptions(name, size.machine())
	// Where the driver attaches them, the sandbox's data and its cache are
	// on volumes of the machine's own, which a new root keeps
	// (keepsDataOnVolume). The cache is the machine's alone, under a key of
	// its name that nothing saves: no machine shares a cache. A snapshot
	// build is made with neither: a committed VM holds no volume.
	caps := r.engine.Driver.Capabilities()
	if caps.Attaches(machine.VolumeData) {
		opts.Data = &machine.DataVolume{}
	}
	if caps.Attaches(machine.VolumeCache) {
		opts.Cache = &machine.CacheVolume{Key: name}
	}
	from, snapshot, release := r.snapshotFor(ctx, req)
	defer release()
	if from != "" {
		opts.From = from
		inst, err = r.engine.Create(ctx, opts)
		switch {
		case errors.Is(err, engine.ErrNotFound) || errors.Is(err, machine.ErrNotFound):
			// The snapshot went between its lookup and this create.
			slog.WarnContext(ctx, "a snapshot is gone; making the sandbox's machine fresh", "snapshot", from, "sandboxId", sandboxID)
			opts.From, snapshot = "", ""
		case errors.Is(err, machine.ErrUnsupported):
			// The driver will not copy it as asked: a copy keeps its
			// source's size on archil, and the snapshot is not at this
			// box's. Its key holds the size, so this is not expected; a
			// fresh machine at the asked size keeps the box from failing
			// on the driver's refusal (ADR 26-11-23-445 §5).
			slog.WarnContext(ctx, "the driver refuses to make the sandbox's machine from a snapshot; making it fresh", "snapshot", from, "sandboxId", sandboxID, "error", err)
			opts.From, snapshot = "", ""
		}
	}
	if opts.From == "" {
		inst, err = r.engine.Create(ctx, opts)
	}
	if errors.Is(err, machine.ErrExists) {
		if inst, err = r.instance(ctx, sandboxID); err != nil {
			return inst, "", err
		}
		return inst, "", r.fenced(inst)
	}
	if err != nil {
		// A create past the provider's quota fails here, and says so (ADR
		// 26-10-09-106 §4).
		return machine.Instance{}, "", fmt.Errorf("create sandbox %s's machine: %w", sandboxID, err)
	}
	return inst, snapshot, nil
}

// createOptions is how every machine of the pool is made, a sandbox's or a
// snapshot build's: isolated as its driver fences a pool (on the pool's
// network, or on none at an edge; PoolFencing), fenced to its hop, with
// its pool URL on and its ingress URL off, both asked strictly, so a driver
// that cannot give the one or withhold the other refuses the machine (ADR
// 26-10-23-551 §4, ADR 26-10-29-481 §4, disco-vm ADR 26-11-14-610). The pool,
// its peer, reaches it at its pool URL alone; an ingress URL would be an
// inbound path from outside the pool that the guest's root answers and the
// pool's fence never sees. It is made at size, each zero field the driver's
// default (ADR 26-11-23-445 §5).
func (r *MachineSandboxRuntime) createOptions(name string, size MachineSize) engine.CreateOptions {
	opts := engine.CreateOptions{Name: name, Egress: r.egress, CPUs: size.CPUs, Memory: size.MemoryBytes}
	opts.URLs.Ingress = machine.URLRequest{Mode: machine.URLOff, Strict: true}
	opts.URLs.Pool = machine.URLRequest{Mode: machine.URLOn, Strict: true}
	opts.Bare = r.bare
	if r.network != "" {
		opts.Networks = []string{r.network}
	}
	return opts
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
// want: nil is no fence, DNS is closed in both or neither (an empty list
// closes it whatever CloseDNS says, as machine.Egress does), and the allowed
// hosts compare as a set.
func sameEgress(got, want *machine.Egress) bool {
	if got == nil || want == nil {
		return got == want
	}
	closesDNS := func(e *machine.Egress) bool { return e.CloseDNS || len(e.Allow) == 0 }
	if closesDNS(got) != closesDNS(want) {
		return false
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

// agentListenAddress is where the sandbox agent listens in the machine: the
// address the driver gives the instance's one service, which its URL reaches.
// The driver picks it, the same for every boot of the instance, so it is
// placed in the bootstrap once (disco-vm ADR 0005).
func (r *MachineSandboxRuntime) agentListenAddress(ctx context.Context, inst machine.Instance) (string, error) {
	listen, err := r.engine.Driver.Listen(ctx, inst)
	if err != nil {
		return "", fmt.Errorf("the service address of sandbox machine %s: %w", inst.Name, err)
	}
	address, ok := strings.CutPrefix(listen, "tcp:")
	if !ok || address == "" {
		return "", fmt.Errorf("the %s driver gives machine %s's service %q, and the sandbox agent listens on TCP alone", r.engine.Driver.Name(), inst.Name, listen)
	}
	return address, nil
}

// place puts in the running machine what its agent boots on — sandbox.json,
// and the agent layer when it is not there yet, its units and its pins — and
// runs `discobox-provision apply`, which wires the running machine in place
// and starts the agent (ADR 26-11-23-458). Where it was placed before with
// the same, apply mounts nothing and starts the agent its boot did not. Where
// it was never placed in this instance, or was placed with something else, it
// asks the agent it placed whether that agent wires a machine in place, and
// applies with the agent restarted onto what was placed (§4): the machine is
// started once, and provisioned while it runs.
//
// An agent too old to wire in place (§6) would mount the machine's volumes
// under the services already running on those paths, which only
// discobox-wire.service at a boot, before them, may mount (ADR 26-11-02-023
// §3). For it place reports moved, and then applies nothing and records
// nothing, and the caller restarts the machine onto it.
//
// after is how the machine came to boot on this. Restarted, it is the
// placement after that restart: the machine booted on what was placed, its
// agent started by its own units as a container's does, so it is placed
// again, applies nothing either, and is recorded. A pool agent gone between
// the two leaves the old record, and the next start restarts the machine
// again rather than take it as placed. A restart whose stop was forced may
// have lost what was copied before it, and a record would have every later
// start skip the layer: that placement is returned, for the caller to record
// once the agent has answered.
func (r *MachineSandboxRuntime) place(ctx context.Context, sandboxID string, inst machine.Instance, record machineRecord, after placeAfter) (bool, *placedRecord, error) {
	restarted := after != notRestarted
	pins, ok, err := r.deliveredPins(sandboxID)
	if err != nil {
		return false, nil, err
	}
	if !ok {
		return false, nil, fmt.Errorf("sandbox %s: %w", sandboxID, ErrUndelivered)
	}
	agent, ok, err := r.sandboxAgent(ctx, sandboxID)
	if err != nil {
		return false, nil, fmt.Errorf("the agent of sandbox %s's pins %v: %w", sandboxID, pins, err)
	}
	if !ok {
		return false, nil, fmt.Errorf("sandbox %s is pinned to %v, and this pool has no asset cache to place its agent from", sandboxID, pins)
	}
	// A machine takes the pool's setting at every start, since its bootstrap
	// is placed again: a bootstrap that moved with it restarts the machine
	// onto it below (ADR 26-10-29-481 §3).
	if record.Bootstrap.Runtime.AgentRuntime.TLS != r.agentTLS {
		record.Bootstrap.Runtime.AgentRuntime.TLS = r.agentTLS
		if err := r.writeMachineRecord(sandboxID, record); err != nil {
			return false, nil, err
		}
	}
	bootstrap, bootstrapDigest, err := renderBootstrap(record)
	if err != nil {
		return false, nil, err
	}
	// The keypair the agent serves its API over TLS with goes beside it, the
	// one the pool holds now, so a start places one due for renewal anew. It
	// is no part of the bootstrap's digest: a renewed keypair moves nothing
	// the machine must restart for.
	var keypair *sandboxconfig.AgentTLS
	if r.agentTLS {
		material, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID)
		if err != nil {
			return false, nil, err
		}
		keypair = &material.Agent
	}
	placed, had, err := r.readPlaced(sandboxID)
	if err != nil {
		return false, nil, err
	}
	had = had && placed.Container == inst.ID
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		return false, nil, err
	}
	// A machine first placed on is refused when its base carries no systemd,
	// which every unit placed below runs under (ADR 26-11-16-619 §4).
	if !had {
		if err := provisioner.RequireSystemd(ctx, guest); err != nil {
			return false, nil, fmt.Errorf("sandbox %s's machine: %w", sandboxID, err)
		}
	}
	if err := guest.CopyTo(ctx, "/", bootstrapTree(bootstrap, keypair)); err != nil {
		return false, nil, fmt.Errorf("place sandbox %s's bootstrap: %w", sandboxID, err)
	}
	reader, writer := io.Pipe()
	go func() {
		// The placement after a restart finds the layer the one before it
		// placed, and its agent running from it: it is not written again.
		writer.CloseWithError(agentlayer.Archive(writer, agent, restarted || had && placed.Layer == agent.Digest, pins, agentlayer.MachineUnits))
	}()
	defer reader.Close()
	moved := !restarted && (!had || placed.Layer != agent.Digest || !samePins(placed.Pins, pins) || placed.Bootstrap != bootstrapDigest)
	var result provisioner.Result
	switch {
	case moved || restarted:
		if err := guest.CopyTo(ctx, "/", reader); err != nil {
			return false, nil, fmt.Errorf("place sandbox %s's agent: %w", sandboxID, err)
		}
		if restarted {
			break
		}
		var inPlace bool
		if inPlace, err = mountsInPlace(ctx, guest); err != nil {
			return false, nil, fmt.Errorf("ask sandbox %s's agent whether it wires its machine in place: %w", sandboxID, err)
		}
		if !inPlace {
			// The unit mounts at the boot the restart makes, and the
			// placement after it applies.
			slog.InfoContext(ctx, "the agent placed in a sandbox machine cannot wire it in place; restarting the machine onto it", "sandboxId", sandboxID)
			return true, nil, nil
		}
		result, err = provisioner.Run(ctx, guest, machine.ExecRequest{Argv: applyInPlaceCommand}, nil)
	default:
		result, err = provisioner.Provision(ctx, guest, provisioner.Plan{
			Dir:     "/",
			Tree:    reader,
			Command: machine.ExecRequest{Argv: applyCommand},
		})
	}
	if err != nil {
		if tail := strings.TrimSpace(string(result.Stderr)); tail != "" {
			err = fmt.Errorf("%w: %s", err, tail)
		}
		return false, nil, fmt.Errorf("provision sandbox %s's machine: %w", sandboxID, err)
	}
	placement := placedRecord{Container: inst.ID, Layer: agent.Digest, Pins: pins, Bootstrap: bootstrapDigest}
	// How the pool reaches the agent is what the bootstrap just placed says.
	if err := r.recordAgentTLS(sandboxID, record.Bootstrap.Runtime.AgentRuntime.TLS); err != nil {
		return false, nil, err
	}
	if after == restartedForced {
		// Recorded once its agent answers naming this layer, or naming none
		// where the instance held no other (recordForcedPlacement): were
		// this layer's copy lost, an older agent would answer, naming its
		// own.
		return false, &placement, nil
	}
	return false, nil, r.writePlaced(sandboxID, placement)
}

// applyCommand is `discobox-provision apply` on a machine placed before with
// what it is placed now: it mounts nothing, and starts the agent its boot
// did not.
var applyCommand = []string{agentlayer.AgentPath, "provision", "apply"}

// applyInPlaceCommand is apply on a machine whose placement moved: it wires
// the running machine in place, and restarts an agent running on what was
// placed before onto what was placed now (ADR 26-11-23-458 §§2, 4).
var applyInPlaceCommand = []string{agentlayer.AgentPath, "provision", "apply", "--restart-agent"}

// mountsInPlaceCommand asks the placed agent whether its apply wires a
// running machine in place (ADR 26-11-23-458 §6): it exits 0 where it does,
// and an agent from before answers its usage, a failure.
var mountsInPlaceCommand = []string{agentlayer.AgentPath, "provision", "can-mount-in-place"}

// mountsInPlace asks the agent placed in guest whether it wires a running
// machine in place. Any exit but 0 is no; only a command that could not be
// run is an error.
func mountsInPlace(ctx context.Context, guest machine.Guest) (bool, error) {
	code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: mountsInPlaceCommand}, nil, io.Discard, io.Discard)
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// placeAfter is how a machine came to boot on what place puts in it.
type placeAfter int

const (
	// notRestarted is a machine running before the placement.
	notRestarted placeAfter = iota
	// restartedInOrder is a machine restarted onto the placement, stopped in
	// order: what was copied before the restart is on its disk.
	restartedInOrder
	// restartedForced is one whose stop was forced off after its timeout.
	restartedForced
)

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
	tw := configTree(&buf)
	writeTreeFile(tw, "etc/discobox/"+sandboxDocumentName, 0o644, data)
	if keypair != nil {
		agentTLSDir := "etc/discobox/" + sandboxconfig.AgentTLSDirName + "/"
		_ = tw.WriteHeader(&tar.Header{Name: agentTLSDir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)})
		for _, file := range agentTLSFiles(*keypair) {
			writeTreeFile(tw, agentTLSDir+file.name, int64(file.mode), file.data)
		}
	}
	_ = tw.Close()
	return &buf
}

// configTree starts a tar, placed at a machine's root, of what goes in
// /etc/discobox: its directories, public, as the machine's own are.
func configTree(w io.Writer) *tar.Writer {
	tw := tar.NewWriter(w)
	for _, dir := range []string{"etc/", "etc/discobox/"} {
		_ = tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)})
	}
	return tw
}

// writeTreeFile adds the regular file name, of mode, to a tree. A tree is
// written to memory, so it never fails.
func writeTreeFile(tw *tar.Writer, name string, mode int64, data []byte) {
	_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data)), ModTime: time.Unix(0, 0)})
	_, _ = tw.Write(data)
}

// SandboxDialer reaches the sandbox agent at its machine's pool URL, where
// its pool's machine, its peer, reaches it (ADR 26-10-29-481 §1, disco-vm ADR
// 26-11-14-610), over TLS where its
// bootstrap told it to serve TLS (§3). A machine has one service, which is
// the agent's, so no other port in it is reached this way.
func (r *MachineSandboxRuntime) SandboxDialer(ctx context.Context, sandboxID string, port int) (*Dialer, error) {
	if port != SandboxAgentPort {
		return nil, fmt.Errorf("%w: a machine sandbox is reached at its agent alone, not at port %d", ErrUnsupported, port)
	}
	resolve := func(ctx context.Context) (*Dialer, error) {
		value, ok := r.endpoints.Load(sandboxID)
		if !ok {
			if !r.hostsSandbox(sandboxID) {
				return nil, ErrNotFound
			}
			value, _ = r.endpoints.LoadOrStore(sandboxID, &machineEndpointCache{})
		}
		cache, _ := value.(*machineEndpointCache)
		endpoint, err := r.agentEndpoint(ctx, sandboxID, cache)
		if err != nil {
			return nil, err
		}
		dial, err := urlDialer(*endpoint)
		if err != nil {
			return nil, err
		}
		if dial, err = r.agentDialer(sandboxID, dial); err != nil {
			return nil, err
		}
		connect := dial.DialContext
		dial.DialContext = func(ctx context.Context) (net.Conn, error) {
			conn, err := connect(ctx)
			if err != nil {
				cache.endpoint.CompareAndSwap(endpoint, nil)
				// A machine its provider paused since the last poll still
				// reads as running here, and its URL may still resolve:
				// the dial is what finds it. Forgotten, the next use takes
				// the power lock and reads its state, which resumes it
				// (answersRunning).
				if ctx.Err() == nil {
					r.observed.Delete(sandboxID)
				}
			}
			return conn, err
		}
		return dial, nil
	}
	dial, err := resolve(ctx)
	if err != nil || dial.credential == nil {
		return dial, err
	}
	return &Dialer{resolve: resolve}, nil
}

// A lifecycle change drops this cache entry. Lookups still in flight may
// finish against it without publishing an endpoint for the next boot.
type machineEndpointCache struct {
	endpoint atomic.Pointer[machine.Endpoint]
	lookup   singleflight.Group
}

// agentEndpoint returns the pool URL of a running sandbox machine and any
// edge credential, refreshing it in the 30 seconds before it expires.
func (r *MachineSandboxRuntime) agentEndpoint(ctx context.Context, sandboxID string, cache *machineEndpointCache) (*machine.Endpoint, error) {
	if endpoint := cache.endpoint.Load(); endpoint != nil &&
		(endpoint.Credential == nil || time.Until(endpoint.Credential.Expires) > 30*time.Second) {
		return endpoint, nil
	}
	result := cache.lookup.DoChan("", func() (any, error) {
		if endpoint := cache.endpoint.Load(); endpoint != nil {
			if endpoint.Credential == nil || time.Until(endpoint.Credential.Expires) > 30*time.Second {
				return endpoint, nil
			}
		}
		if !r.hostsSandbox(sandboxID) {
			return nil, ErrNotFound
		}
		// Waiters have independent deadlines. Bound the shared provider lookup
		// separately so one caller leaving cannot abort the refresh for others.
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		inst, err := r.instance(lookupCtx, sandboxID)
		if err != nil {
			return nil, err
		}
		endpoint, err := r.engine.URL(lookupCtx, inst, machine.URLPool)
		if err != nil {
			return nil, err
		}
		if r.engine.Driver.Capabilities().URLs.Credential && endpoint.Credential == nil {
			return nil, errors.New("the driver gave the sandbox's pool URL without its edge credential")
		}
		cache.endpoint.Store(&endpoint)
		return &endpoint, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return nil, result.Err
		}
		endpoint, _ := result.Val.(*machine.Endpoint)
		return endpoint, nil
	}
}

// urlDialer opens a connection to a machine's pool URL. The dial negotiates
// TLS at an HTTPS endpoint, and the transport speaks HTTP/1.1 over it.
func urlDialer(endpoint machine.Endpoint) (*Dialer, error) {
	u, err := url.Parse(endpoint.URL)
	if err != nil {
		return nil, err
	}
	if credential := endpoint.Credential; credential != nil {
		if u.Scheme != "https" || u.Hostname() == "" {
			return nil, errors.New("a sandbox's credentialed pool URL must use HTTPS")
		}
		if err := sandboxconfig.ValidateEdgeCredential(credential.Header, credential.Token); err != nil {
			return nil, fmt.Errorf("the sandbox's pool URL has an invalid edge credential: %w", err)
		}
		if !time.Now().Before(credential.Expires) {
			return nil, errors.New("the sandbox's pool URL has an expired edge credential")
		}
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
		return nil, fmt.Errorf("a machine's URL %q is neither http nor https", endpoint.URL)
	}
	address := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if u.Scheme == "http" {
		return &Dialer{DialContext: func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address)
		}}, nil
	}
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
	return &Dialer{
		DialContext: func(ctx context.Context) (net.Conn, error) {
			return tlsDialer.DialContext(ctx, "tcp", address)
		},
		edgeHost:   u.Host,
		credential: endpoint.Credential,
	}, nil
}

// machineAgentPollInterval paces that wait: every pass is a request over the
// network to the machine's URL.
const machineAgentPollInterval = 250 * time.Millisecond

// errAgentNeverUp is a machine's agent that never answered: its machine
// stopped first, or the wait ran out. Until a machine made from a snapshot
// has answered once it blames the snapshot (ADR 26-11-03-986 §6); nothing
// else of a start does, a failed placement included, which the pool or a
// moment of the box's may cause.
var errAgentNeverUp = errors.New("the sandbox agent did not come up")

// waitForSandboxAgent waits for the agent of a machine just started to answer
// at its URL, or for the machine to stop.
func (r *MachineSandboxRuntime) waitForSandboxAgent(ctx context.Context, sandboxID string) error {
	ctx, cancel := context.WithTimeout(ctx, r.agentReadyBound())
	defer cancel()
	var lastErr error
	for {
		sb, err := r.GetSandbox(ctx, sandboxID)
		if errors.Is(err, ErrNotFound) {
			// Deleted or archived under the boot: its agent is never coming,
			// and spinning on it would hold the boot mark for minutes.
			return err
		}
		if err != nil {
			lastErr = err
		} else if err := sandboxAgentTerminalStateError(sb); err != nil {
			return errors.Join(errAgentNeverUp, err)
		} else if dial, err := r.SandboxDialer(ctx, sandboxID, SandboxAgentPort); err != nil {
			lastErr = err
		} else if _, lastErr = sandboxAgentHealth(ctx, dial); lastErr == nil {
			return r.markSnapshotProven(sandboxID)
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("wait for sandbox-agent: %w", errors.Join(errAgentNeverUp, lastErr))
			}
			return ctx.Err()
		case <-time.After(machineAgentPollInterval):
		}
	}
}

// markSnapshotProven records that the agent of a sandbox's machine made from
// a snapshot has answered (snapshotProvenName), once.
func (r *MachineSandboxRuntime) markSnapshotProven(sandboxID string) error {
	record, ok, err := r.readMachineRecord(sandboxID)
	if err != nil || !ok || record.Snapshot == "" || r.snapshotProven(sandboxID) {
		return err
	}
	return fsdurable.WriteFile(r.snapshotProvenPath(sandboxID), nil, 0o600)
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
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	root := r.sandboxRoot(sandboxID)
	if _, err := os.Stat(root); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("archive sandbox %s: %w", sandboxID, err)
		}
		// The tree is the pool's record, on its own machine's disk, and a
		// pool machine made again has none of what the one before it made.
		// The sandbox's machine is found by its name all the same, and is
		// what archiving has to stop: not found is only for a sandbox with
		// neither, which the control plane takes as nothing to tear down.
		if _, err := r.instance(ctx, sandboxID); err != nil {
			return fmt.Errorf("archive sandbox %s: %w", sandboxID, err)
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("archive sandbox %s: %w", sandboxID, err)
		}
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

// DeleteSandbox removes the sandbox's machine, its audit trail in the pool
// proxy (ADR 26-11-14-896 §5), its proxy material and its tree, and returns
// only once the machine is gone: the control plane's delete is confirmed
// rather than accepted (ADR 0022 §3).
func (r *MachineSandboxRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	return r.removeSandbox(ctx, sandboxID, true)
}

// reapSandbox removes what a delete does but the sandbox's audit trail, for
// the reapers, as the Docker runtime's does (ADR 26-11-14-896 §5).
func (r *MachineSandboxRuntime) reapSandbox(ctx context.Context, sandboxID string) error {
	return r.removeSandbox(ctx, sandboxID, false)
}

func (r *MachineSandboxRuntime) removeSandbox(ctx context.Context, sandboxID string, purge bool) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	// Before the lock is let go of: what removal leaves of the sandbox's
	// locks is nothing, whether it got as far as the tree or not; a later
	// lock of it makes the file again.
	defer r.forgetLocks(sandboxID)
	if err := r.removeMachine(ctx, sandboxID); err != nil {
		return err
	}
	// Once the machine is gone, and before the tree, as the Docker
	// runtime's.
	if purge {
		if err := r.purgeAudit(ctx, sandboxID); err != nil {
			return err
		}
	}
	r.observed.Delete(sandboxID)
	r.endpoints.Delete(sandboxID)
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
	name, err := machineName(sandboxID)
	if err != nil {
		return err
	}
	if err := r.destroyMachine(ctx, name); err != nil {
		return fmt.Errorf("sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// destroyMachine destroys the pool's machine named name, a sandbox's or a
// snapshot's, and confirms its driver no longer has it. One already gone is
// not an error.
func (r *MachineSandboxRuntime) destroyMachine(ctx context.Context, name string) error {
	inst, err := r.engine.Driver.Instance(ctx, name)
	if errors.Is(err, machine.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up machine %s: %w", name, err)
	}
	if err := r.engine.Remove(ctx, inst, true); err != nil {
		return fmt.Errorf("remove machine %s: %w", name, err)
	}
	if _, err := r.engine.Driver.Instance(ctx, name); !errors.Is(err, machine.ErrNotFound) {
		if err == nil {
			err = errors.New("its driver still has it")
		}
		return fmt.Errorf("confirm machine %s is removed: %w", name, err)
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
// each running sandbox's own, which is on that sandbox's disk and which its
// agent empties in place (sandbox agent's cache/clear), and its snapshots. Nothing is shared
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
	if err := r.clearSnapshots(ctx); err != nil {
		errs = append(errs, fmt.Errorf("drop the pool's snapshots: %w", err))
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
// container pool does (ADR 26-10-01-876), removes the machines of the pool's
// project that no sandbox of this pool names: orphans of a pool whose
// records were lost (ADR 26-10-23-551 §1), and sweeps its snapshots (ADR
// 26-11-03-986 §5). Its first pass is at the agent's start.
func (r *MachineSandboxRuntime) WatchSandboxVolumes(ctx context.Context, logger *slog.Logger, held HeldSandboxes) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(sandboxVolumeReapInterval)
	defer ticker.Stop()
	for {
		// An unheld sandbox goes the way a delete takes it: its machine,
		// proxy material and published images with its tree.
		reapUnheldSandboxVolumes(ctx, r.sandboxesRoot(), held, sandboxVolumeRetention, time.Now(), logger, r.reapSandbox)
		r.reapOrphanMachines(ctx, logger, held, time.Now())
		r.sweepSnapshots(ctx, logger, time.Now())
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

// reapOrphanMachines removes the machines of the pool's disco-vm pool that look
// like a sandbox's and that neither a tree of this pool nor a sandbox the
// control plane holds names (reapOrphans). Only a name a generated sandbox ID
// spells is considered (`sbx-` and its whole random part, id.IsGenerated): a
// machine of any other name in the project is nothing this runtime
// made, and is left alone however it got there. One the control
// plane holds is kept: its create finds it by name.
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
	var found []string
	for _, inst := range instances {
		// Only a name a generated sandbox ID spells back is one: the machine
		// goes through DeleteSandbox under that ID, so a name merely beginning
		// `sbx-`, or one id.Canonical reads as a short ID (`sbx-test`), is no
		// sandbox's and is left alone.
		if sandboxID := id.Canonical(id.PrefixSandbox, inst.Name); sandboxID != inst.Name && id.IsGenerated(sandboxID) {
			found = append(found, inst.Name)
		}
	}
	reapOrphans(ctx, logger, held, stored, now, orphanSweep{
		kind:    "sandbox machine",
		markers: filepath.Join(r.root.PoolData(r.projectID, r.poolID), orphanMarkersName),
		found:   found,
		nameOf: func(sandboxID string) (string, bool) {
			name, err := machineName(sandboxID)
			return name, err == nil
		},
		// The machine goes as a delete takes its sandbox, with what else the
		// pool keeps for it.
		remove: func(ctx context.Context, name string) error {
			return r.reapSandbox(ctx, id.Canonical(id.PrefixSandbox, name))
		},
	})
}

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

// resumeInBackground resumes a sandbox its provider paused, unless a resume
// of it is already under way: the state poll's reconcile of a pause the pool
// did not make (ADR 26-11-16-619 §5), which can take as long as a start.
//
// One whose resume failed is not tried again by the poll for
// resumeRetryInterval: a provider that refuses it, or pauses it again at once,
// would otherwise be asked every poll. A use still resumes it at once
// (EnsureSandboxRunning).
func (r *MachineSandboxRuntime) resumeInBackground(ctx context.Context, logger *slog.Logger, sandboxID string) {
	if value, ok := r.resumeFailed.Load(sandboxID); ok {
		if failed, ok := value.(time.Time); ok && time.Since(failed) < resumeRetryInterval {
			return
		}
	}
	if _, busy := r.resuming.LoadOrStore(sandboxID, struct{}{}); busy {
		return
	}
	go func() {
		defer r.resuming.Delete(sandboxID)
		if err := r.resumePaused(ctx, sandboxID); err != nil {
			r.resumeFailed.Store(sandboxID, time.Now())
			if ctx.Err() == nil {
				logger.Warn("resume a sandbox machine its provider paused", "sandboxId", sandboxID, "error", err, "retryIn", resumeRetryInterval)
			}
			return
		}
		r.resumeFailed.Delete(sandboxID)
	}()
}

// resumeRetryInterval is how long the state poll waits to resume again a
// sandbox whose resume failed.
const resumeRetryInterval = 5 * time.Minute

// resumePaused starts a sandbox whose machine its provider paused, which
// resumes it with what it held (engine.Start), and places what its agent
// boots on as any start does. Under the power lock it starts only one still
// paused: one stopped since the poll saw it stays stopped.
func (r *MachineSandboxRuntime) resumePaused(ctx context.Context, sandboxID string) error {
	unlock, err := r.lockPower(ctx, sandboxID)
	if err != nil {
		return err
	}
	defer unlock()
	sb, err := r.powerTarget(ctx, sandboxID)
	if err != nil || sb.Status != StatusPaused {
		return err
	}
	slog.InfoContext(ctx, "resuming a sandbox machine its provider paused", "sandboxId", sandboxID)
	return r.startLocked(ctx, sandboxID)
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
			var paused, settle []string
			for _, sb := range sandboxes {
				switch sb.Status {
				case StatusPaused:
					paused = append(paused, sb.SandboxID)
				case StatusSuspended:
				default:
					settle = append(settle, sb.SandboxID)
				}
				state := stateFromStatus(sb.Status)
				seen[sb.SandboxID] = state
				if complete || last[sb.SandboxID] != state {
					batch.States = append(batch.States, SandboxStateObservation{SandboxID: sb.SandboxID, State: state, StartedFrom: sb.StartedFrom})
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
			// Resumed only once the batch that reports the pause is out: a
			// resume publishes its own states, reported after this batch's
			// time, and the control plane drops a report older than one it
			// holds, so a pause published after them would never be seen.
			for _, sandboxID := range paused {
				r.resumeInBackground(ctx, logger, sandboxID)
			}
			// A machine the pool froze that no longer reads paused is
			// settled after the batch too, for the same reason.
			for _, sandboxID := range settle {
				r.settleFrozenInBackground(ctx, logger, sandboxID)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
