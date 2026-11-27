package discovm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

const (
	// poolStartTimeout bounds a pool machine's boot until its guest
	// operations answer: a start's, and a create's, which boots it
	// (create), from the provider's create on.
	poolStartTimeout = 10 * time.Minute
	// poolStopTimeout is how long a pool machine gets to power off in order
	// before it is forced off.
	poolStopTimeout = time.Minute
)

// poolMachine hosts a pool in a disco-vm machine of its own, in the provider's
// scope, named for its pool so a create that was lost is found rather than
// duplicated. It is a remote driver's shape (ADR 26-10-09-106 §1): the machine
// boots a driver base, and is made the pool's box as a Docker pool's container
// is (poolruntime.ProvisionBox, ADR 26-10-24-953 §2), with its bootstrap
// naming its own engine (ADR 26-10-23-551 §§1, 3) and the provisioning key it
// calls that engine with (poolkey.go) placed beside it.
//
// An operator reaches the host through the driver's guest operations, never
// through the pool agent: the console and the log are what is asked for when
// the pool agent is what refuses to come up.
type poolMachine struct {
	engine *engine.Engine
	// scopeEngine opens the provider's driver in another disco-vm scope
	// (engineIn): removal opens the pool's own, to empty it.
	scopeEngine func(scope string) (*engine.Engine, error)
	// base is the driver's base the machine, and its sandboxes' machines, are
	// created fresh from (Config.Base, settings.base), which the driver is
	// made with and the pool's agent makes its own with; empty is the
	// driver's default.
	base string
	// driverConfig is the provider's driver configuration, without its
	// secret, which the pool's agent makes its engine with.
	driverConfig json.RawMessage
	// controlPlaneURL is where the machine reaches this server
	// (Config.ControlPlaneURL).
	controlPlaneURL string
	// policy is what the pool agent reads from its environment.
	policy map[string]string
	keys   poolKeys
	// shell is the console's command in the machine.
	shell []string
	// logs is the command that prints the machine's log, and logSource names
	// what it prints for the operator reading it.
	logs      func(sandbox.PoolLogOptions) []string
	logSource string
	// limitInterval is how long a pool goes between its driver's Checks for
	// its machine limit (checkMachineLimit).
	limitInterval time.Duration
	// progress records what bringing the pool's machine up is doing, for a
	// client whose sandbox waits on the pool; nil says nothing.
	progress sandbox.PoolProgressReporter
	// endpoints is the server's own view of each pool machine's URLs, with
	// the credential each answers, on a driver whose URLs answer only one
	// (pooledge.go).
	endpoints serviceEndpoints
}

// machineLimitInterval bounds how often a running pool's pass runs the
// driver's Check for its machine limit (ADR 26-11-15-831, Open: how often
// Check runs). Every pool is reconciled each scan, once a minute, and boxd's
// Check is several calls to the org's API; a limit an org's owner moves is
// seen within this, and the recorded one stands between.
const machineLimitInterval = 10 * time.Minute

// machineLimitCheckTimeout bounds a running pool's Check, which reaches the
// provider's home endpoint, not the one the rest of the pass calls: one that
// does not answer holds up the pass no longer than this.
const machineLimitCheckTimeout = 30 * time.Second

func newPoolMachine(e *engine.Engine, cfg settings, scopeEngine func(scope string) (*engine.Engine, error), progress sandbox.PoolProgressReporter) *poolMachine {
	return &poolMachine{
		engine:          e,
		scopeEngine:     scopeEngine,
		base:            cfg.base(),
		driverConfig:    cfg.driverConfig,
		controlPlaneURL: strings.TrimSpace(cfg.ControlPlaneURL),
		policy:          cfg.Env(),
		keys:            newPoolKeys(e.Driver, cfg),
		shell:           []string{"/bin/bash", "-l"},
		logs:            journalCommand,
		logSource:       "pool machine journal",
		limitInterval:   machineLimitInterval,
		progress:        progress,
	}
}

// journalCommand reads a Linux pool machine's journal for this boot: the pool
// agent's unit and everything under it, which is what an operator needs when
// the agent will not register.
func journalCommand(opts sandbox.PoolLogOptions) []string {
	args := []string{"journalctl", "--no-pager", "--boot"}
	if opts.Tail > 0 {
		args = append(args, "--lines", strconv.Itoa(opts.Tail))
	} else {
		args = append(args, "--no-tail")
	}
	if opts.Follow {
		args = append(args, "--follow")
	}
	return args
}

// poolRuntimeState is the provider's record of a pool's machine, kept on the
// pool's row (ADR 26-10-23-551 §1), since the machine outlives the server's
// process: what it placed there, which is compared with what it now wants on
// every reconcile, never with anything the machine reports.
type poolRuntimeState struct {
	// Instance is the machine's ID at the driver.
	Instance string `json:"instance"`
	// MachinePool is the disco-vm scope the pool makes its sandboxes' machines
	// in, which is also the network the pool's machine and theirs share.
	MachinePool string `json:"machinePool,omitempty"`
	// Box is the agent, the bundles and the placement placed.
	Box poolruntime.BoxRecord `json:"box,omitzero"`
	// Key is the provisioning key placed, without its secret.
	Key *poolKeyRecord `json:"key,omitempty"`
	// MaxMachines is the driver's machine limit, by guest OS, as its Check
	// last read it with the provider's key (ADR 26-11-15-831 §2): what the
	// pool's bootstrap carries when a pass's Check fails, or before one has
	// answered since the server started. Absent is no cap, or one the
	// provider would not say.
	MaxMachines map[machine.OS]int `json:"maxMachines,omitempty"`
	// LimitCheckedAt is when the driver's Check was last run for
	// MaxMachines, whether or not it answered: the next is due
	// limitInterval after it.
	LimitCheckedAt time.Time `json:"limitCheckedAt,omitzero"`
	// SandboxEdge is the credential placed for the pool's sandboxes to reach
	// its listener with, without its token, on a driver whose URLs answer
	// only one (ADR 26-11-16-619 §2). Absent is none placed.
	SandboxEdge *sandboxEdgeRecord `json:"sandboxEdge,omitempty"`
}

func decodeRuntimeState(pool *model.Pool) poolRuntimeState {
	var state struct {
		poolRuntimeState
		// Placement is where a record from before the box's record held its
		// placement kept it, which is read so that record's pool is not
		// placed again over nothing.
		Placement string `json:"placement,omitempty"`
	}
	if len(pool.RuntimeState) > 0 {
		// A record that does not read is a record lost: it costs a placement,
		// not a key, since the pool's keys are found by name.
		_ = json.Unmarshal(pool.RuntimeState, &state)
	}
	if state.Box.Placement == "" {
		state.Box.Placement = state.Placement
	}
	return state.poolRuntimeState
}

// machinePool is the pool's own disco-vm scope, derived from its ID within
// disco-vm's scope rule (ADR 26-10-23-551 §3: pool_x becomes pool-x). It is
// the scope the pool agent makes its sandboxes' machines in, the network
// the pool's machine and theirs share, and the name of the pool's machine in
// the provider's scope, which it never equals (prov-…).
func machinePool(poolID string) (string, error) {
	scope := strings.ReplaceAll(strings.ToLower(poolID), "_", "-")
	if err := machine.ValidatePool(scope); err != nil {
		return "", fmt.Errorf("pool %q has no disco-vm scope: %w", poolID, err)
	}
	return scope, nil
}

// poolMachineName is the pool's machine in the provider's scope.
func poolMachineName(poolID string) (string, error) { return machinePool(poolID) }

// errNoPoolMachine is a pool whose machine was never created, or was removed.
var errNoPoolMachine = errors.New("the pool has no machine")

// instance finds the pool's machine, or reports errNoPoolMachine. A remote
// driver's service that does not answer the lookup is
// sandbox.ErrPoolNotReachable (notReachable), as the other calls that ask the
// service about the machine are: a pool reconcile waits on it rather than
// recording a failure and repairing the pool for a network fault, and so does
// a create waiting for the pool's agent.
func (h *poolMachine) instance(ctx context.Context, pool *model.Pool) (engine.Instance, error) {
	name, err := poolMachineName(pool.ID)
	if err != nil {
		return engine.Instance{}, err
	}
	inst, err := h.engine.Get(ctx, name)
	switch {
	case err == nil:
		return inst, nil
	case errors.Is(err, engine.ErrNotFound):
		return engine.Instance{}, fmt.Errorf("pool %s: %w: it is created when the pool reconciles", pool.ID, errNoPoolMachine)
	default:
		return engine.Instance{}, fmt.Errorf("pool %s's machine cannot be looked up: %w", pool.ID, notReachable(err))
	}
}

// onceBegin calls begin the first time it is called, and nothing after: a
// pass closes placement once, before the first thing that starts or replaces
// an agent.
func onceBegin(begin func(context.Context) error) func(context.Context) error {
	begun := false
	return func(ctx context.Context) error {
		if begun || begin == nil {
			return nil
		}
		begun = true
		return begin(ctx)
	}
}

func (h *poolMachine) ensurePoolHost(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker, begin func(context.Context) error) error {
	steps := poolruntime.NewHostSteps(pool.ID)
	defer steps.Finish(ctx)
	return h.converge(ctx, pool, maker, onceBegin(begin), false, steps)
}

// repairPoolHost restarts the pool's machine in place, on the same disk, and
// provisions and enrolls it again whatever the record says, so a server that
// lost the pool's key is mended by the provider (ADR 26-10-24-953 §4). A pool
// with no machine is made one.
func (h *poolMachine) repairPoolHost(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker, begin func(context.Context) error) error {
	// A repair provisions the pool's machine again: every step of it is
	// bringing the host up.
	steps := poolruntime.NewHostSteps(pool.ID)
	steps.BringingUp(ctx)
	gate := onceBegin(begin)
	inst, err := h.instance(ctx, pool)
	if errors.Is(err, errNoPoolMachine) {
		return h.converge(ctx, pool, maker, gate, true, steps)
	}
	if err != nil {
		return err
	}
	if _, err := h.state(ctx, pool, inst); err != nil {
		return err
	}
	if err := gate(ctx); err != nil {
		return err
	}
	// Forced off is still off, and off is what a restart needs.
	if err := h.engine.Stop(ctx, inst, poolStopTimeout); err != nil && h.engine.State(ctx, inst) != engine.Stopped {
		return fmt.Errorf("stop pool %s's machine: %w", pool.ID, err)
	}
	steps.Step(ctx, "stopped the pool's machine to restart it", "machine", inst.ID)
	return h.converge(ctx, pool, maker, gate, true, steps)
}

// converge brings the pool's machine to what the provider wants (ADR
// 26-10-23-551 §1): made from the base on the pool's network when there is
// none, started when it is stopped, given a provisioning key that is not due
// for renewal, and provisioned when what it was placed with moved — its
// bundles, or its bootstrap and environment, which the provider's
// configuration decides — or always when force. Its record is written to the pool as it goes, so a pass that
// fails part way still says what it placed, a key above all. Each step it
// takes is logged to steps, which the caller started with the pass.
func (h *poolMachine) converge(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker, gate func(context.Context) error, force bool, steps *poolruntime.HostSteps) error {
	if err := pool.Outbound.RejectLocalEndpoints(); err != nil {
		return err
	}
	if maker == nil {
		return errors.New("a pool maker is required")
	}
	scope, err := machinePool(pool.ID)
	if err != nil {
		return err
	}
	// A pool made before its create refused the driver (fences). On a
	// driver that fences on networks, the pool's machine and its sandboxes'
	// share the pool's own network; at its provider's edge, there is none.
	fencing, err := fences(h.engine.Driver)
	if err != nil {
		return err
	}
	networks := poolNetworks(fencing, scope)
	// What the pool's row holds of the machine as the pass starts: the key
	// placed is saved at once only where the record then differs.
	saved := slices.Clone(pool.RuntimeState)
	record := decodeRuntimeState(pool)
	inst, err := h.instance(ctx, pool)
	if err != nil && !errors.Is(err, errNoPoolMachine) {
		return err
	}
	made, started := false, false
	// created is a machine this pass made from the base, which no agent ran
	// in before; a machine adopted under the pool's name is made in the
	// record's eyes, but may already run one. startedAt is the moment the
	// pool's agent counts as started from when this pass creates or starts
	// the machine: just before it does (ADR 26-11-15-294 §1).
	created := false
	var startedAt time.Time
	// A machine that is not the one the record names was made in its place —
	// by a pass that made it and stopped before its record was saved, or by
	// anyone else under the pool machine's name — so nothing the record says
	// was placed is on it. It is taken as made: its name, URLs and bundles
	// are the same, so the record would otherwise call it provisioned and no
	// pass would ever place the pool's agent on it. Only one on the networks
	// create makes it on (the pool's own, or none at an edge), not stripped,
	// is taken: anything else is not the pool's to key (ADR 26-10-23-551 §4).
	replaced := err == nil && record.Instance != "" && record.Instance != inst.ID
	stripped := inst.Bare && h.engine.Driver.Capabilities().Bare == machine.BareStrip
	if replaced && (stripped || !slices.Equal(inst.Networks, networks)) {
		return fmt.Errorf("pool %s: the machine %s under its name is not the one it recorded, and is not on the networks its pool's machine is made on (%v): nothing is placed on it", pool.ID, inst.ID, networks)
	}
	if err != nil || replaced {
		if err := gate(ctx); err != nil {
			return err
		}
		// A machine lost outside discobox is made again, and registers anew
		// (ADR 26-10-23-551 §1); the keys it was given go with it (§4). A
		// record that was lost costs a placement, not a key: they are found
		// by name.
		if err := h.keys.revokeAll(ctx, pool, record.Key); err != nil {
			return err
		}
		// What the server held of the machine the record named goes with it.
		h.endpoints.forget(record.Instance)
		record = poolRuntimeState{}
		if !replaced {
			// No machine is made that no key could reach.
			if err := h.keys.keyable(pool); err != nil {
				return err
			}
			startedAt = time.Now()
			vmCtx, releaseVM := h.progress.Hold(ctx, pool.ID, sandbox.PoolPhaseStartingVM)
			inst, err = h.create(vmCtx, pool, scope, networks)
			releaseVM()
			if err != nil {
				return err
			}
			// Made running (create): the state check below finds it up
			// and starts nothing.
			created, started = true, true
			steps.BringingUp(ctx)
			steps.Step(ctx, "made the pool's machine", "machine", inst.ID)
			steps.Step(ctx, "started the pool's machine", "machine", inst.ID)
			// create ran the driver's Check, so its limit is read.
			record.MaxMachines, record.LimitCheckedAt = machineLimit(h.engine.Driver.Capabilities()), time.Now()
		}
		made = true
	}
	record.Instance, record.MachinePool = inst.ID, scope
	if err := h.writeState(pool, record); err != nil {
		return err
	}
	state, err := h.state(ctx, pool, inst)
	if err != nil {
		return err
	}
	if state != engine.Running {
		if err := gate(ctx); err != nil {
			return err
		}
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		vmCtx, releaseVM := h.progress.Hold(ctx, pool.ID, sandbox.PoolPhaseStartingVM)
		err := h.engine.Start(vmCtx, inst, engine.StartOptions{Timeout: poolStartTimeout})
		releaseVM()
		if err != nil {
			return fmt.Errorf("start pool %s's machine: %w", pool.ID, err)
		}
		started = true
		steps.BringingUp(ctx)
		steps.Step(ctx, "started the pool's machine", "machine", inst.ID)
	}
	if made || started {
		markStarted(pool)
	}
	if created || started {
		// Told now, not once the machine answers: a booting machine does
		// not, this pass then fails as not reachable, and the pass that
		// settles starts nothing.
		sandbox.ReportAgentStarted(ctx, startedAt, sandbox.AgentStartBox)
	}
	guest, err := h.engine.Guest(ctx, inst)
	if err != nil {
		return fmt.Errorf("reach pool %s's machine: %w", pool.ID, notReachable(err))
	}
	steps.Step(ctx, "reached the pool's machine")
	// The key is placed before anything the agent runs on, and saved on the
	// pool's row the moment it is, not when the pass settles: an issued key
	// whose record is lost would be one nobody renews, and the agent this
	// pass starts calls its driver at once, with a sentinel the pool's proxy
	// is answered for by the row alone (resolveDriverKey).
	key, err := h.keys.ensure(ctx, pool, guest, record.Key)
	record.Key = key
	if werr := h.writeState(pool, record); werr != nil {
		return errors.Join(err, werr)
	}
	if !bytes.Equal(pool.RuntimeState, saved) {
		if rerr := maker.RecordHost(ctx, pool.RuntimeState, pool.HostCredential); rerr != nil {
			return errors.Join(err, fmt.Errorf("save pool %s's key record: %w", pool.ID, rerr))
		}
	}
	if err != nil {
		return err
	}
	if h.checkMachineLimit(ctx, pool, &record) {
		if err := h.writeState(pool, record); err != nil {
			return err
		}
	}
	provisioned := record.Box.Agent != ""
	placement, err := h.placement(ctx, pool, inst, guest, maker, scope, networks, record.MaxMachines)
	if err != nil {
		if provisioned && !force {
			slog.WarnContext(ctx, "keeping the pool's machine as it is: what it is made of does not resolve", "pool", pool.ID, "error", err)
			sandbox.ReportBoxKept(ctx, fmt.Errorf("what its machine is made of does not resolve: %w", err))
			return nil
		}
		return err
	}
	// The sandboxes' credential is no part of the placement: the agent reads
	// it as it decides each sandbox's document, so a renewal restarts nothing.
	err = h.ensureSandboxEdge(ctx, pool, inst, guest, &record, force)
	if werr := h.writeState(pool, record); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	current, err := record.Box.Current(placement)
	if err != nil {
		return err
	}
	if !force && current {
		steps.Step(ctx, "the pool's box is current")
		if started {
			// The machine's boot started the agent its box already holds,
			// which comes up on its own: no call of this pass spans it, so
			// the phase is reported, not held.
			h.progress.Report(ctx, pool.ID, sandbox.PoolPhaseWaitingForPoolAgent)
		}
		pool.OutboundAppliedRevision = pool.Outbound.Revision()
		// A pass that applied the in-machine placement and stopped before
		// its old key was retired retires it now.
		return h.retireKey(ctx, pool, guest, &record)
	}
	// The apply restarts the agent the moved bundles carry, so placement is
	// closed first, as it is before any agent is replaced.
	steps.BringingUp(ctx)
	if err := gate(ctx); err != nil {
		return err
	}
	// Provisioning a box this pass made, started or repairs, or one never
	// provisioned (its machine was still booting when the pass that made it
	// failed as not reachable), is bringing the pool up; re-applying moved
	// bundles in a running one is not, and reports nothing, as the engine's
	// in-place apply does. The apply's
	// start of the agent returns only once the agent's first report has
	// landed (ADR 26-11-15-602), so the held phase covers its coming up and
	// nothing waits on it afterwards.
	agentCtx := ctx
	if made || started || force || !provisioned {
		var releaseAgent func()
		agentCtx, releaseAgent = h.progress.Hold(ctx, pool.ID, sandbox.PoolPhaseStartingPoolAgent)
		defer releaseAgent()
	}
	if replacesAgent(created, started, provisioned, !current) {
		// Recorded before the apply, whose units hand over to the new agent
		// (ADR 26-11-15-602 §1, 26-11-26-536 §4).
		sandbox.ReportAgentStarted(ctx, time.Now(), sandbox.AgentStartInPlace)
	}
	var placed poolruntime.BoxRecord
	if provisioned {
		placed = record.Box
	}
	box, err := poolruntime.ProvisionBox(agentCtx, guest, maker, placement, placed)
	if err != nil {
		// A slot still draining from the last handover moved nothing, and
		// failed nothing: the pool serves as it was, and the reconciler
		// tries again once it may have exited.
		if errors.Is(err, sandbox.ErrPoolSlotDraining) {
			return err
		}
		if provisioned && !force && !made && !started {
			// A running pool is never taken down over its bundles (ADR
			// 26-10-24-953 §5): it keeps what it last provisioned, and the
			// next reconcile tries again.
			slog.WarnContext(ctx, "keeping the pool's machine as it is: its moved bundles did not apply", "pool", pool.ID, "error", err)
			sandbox.ReportBoxKept(ctx, fmt.Errorf("its moved bundles did not apply: %w", err))
			if pool.OutboundStatus != nil && pool.OutboundAppliedRevision != pool.Outbound.Revision() {
				return fmt.Errorf("%w: %w", sandbox.ErrPoolConfigApply, err)
			}
			return nil
		}
		return fmt.Errorf("provision pool %s's machine: %w", pool.ID, err)
	}
	steps.Step(ctx, "provisioned the pool's box")
	record.Box = box
	pool.OutboundAppliedRevision = pool.Outbound.Revision()
	if err := h.writeState(pool, record); err != nil {
		return err
	}
	return h.retireKey(ctx, pool, guest, &record)
}

// retireKey takes the key an older server placed off a pool's machine that
// now authenticates in-machine, once the placement that turns that on has
// applied (poolKeys.retire), and records it.
func (h *poolMachine) retireKey(ctx context.Context, pool *model.Pool, guest machine.Guest, record *poolRuntimeState) error {
	key, err := h.keys.retire(ctx, pool, guest, record.Key)
	if key == record.Key {
		return err
	}
	record.Key = key
	if werr := h.writeState(pool, *record); werr != nil && err == nil {
		err = werr
	}
	return err
}

// checkMachineLimit runs the driver's Check, the one call that reads its
// provider's machine limit, and records the limit it then reports (ADR
// 26-11-15-831 §§1–2), once limitInterval has passed since the record's last
// Check; it reports whether it ran one. It runs with the provider's key,
// which can read the limit where the pool's machine's own credential may not.
// A Check that fails does not fail the pass, nor take a running pool down:
// the limit recorded stands, and the next Check is an interval away. Whether
// a running pool should act on a Check that now refuses its account is not
// decided here.
func (h *poolMachine) checkMachineLimit(ctx context.Context, pool *model.Pool, record *poolRuntimeState) bool {
	now := time.Now()
	if now.Sub(record.LimitCheckedAt) < h.limitInterval && !now.Before(record.LimitCheckedAt) {
		return false
	}
	record.LimitCheckedAt = now
	checkCtx, cancel := context.WithTimeout(ctx, machineLimitCheckTimeout)
	defer cancel()
	if err := checkDriver(checkCtx, h.engine, sandbox.ErrPoolNotReachable); err != nil {
		slog.WarnContext(ctx, "keeping the pool's recorded machine limit: its driver's check failed", "pool", pool.ID, "error", err)
		return true
	}
	record.MaxMachines = machineLimit(h.engine.Driver.Capabilities())
	return true
}

// machineLimit is the caps a driver reports on the VMs that may exist, by
// guest OS: none is nil, so a driver that reports none keeps the pool's
// placement fingerprint as it was.
func machineLimit(caps machine.Capabilities) map[machine.OS]int {
	var limit map[machine.OS]int
	for os, n := range caps.MaxMachines {
		if n <= 0 {
			continue
		}
		if limit == nil {
			limit = map[machine.OS]int{}
		}
		limit[os] = n
	}
	return limit
}

// create makes the pool's machine from the base, on networks: the pool's
// network alone, off the org's default one, so no other machine of the
// provider's reaches it (ADR 26-10-23-551 §4), or none on a driver that
// fences at its provider's edge, whose machines reach each other only at
// their URLs (ADR 26-11-16-619 §1). It is not isolated: boxd peers an isolated machine
// only with machines that are not, so the pool's machine keeps what the
// driver gives a machine (the pool's HostCredential says so). It asks for its
// ingress URL, where the server, the pool's controller, reaches its agent,
// strictly: a driver that cannot give one refuses the machine here rather
// than at the server's first dial. Its pool URL is the driver's default,
// which every driver that carries a pool gives. A sandbox's machine asks for
// its pool URL on and its ingress URL off, both strictly (disco-vm ADR
// 26-11-14-610).
//
// It is made running, as a start leaves it (engine.CreateOptions.Start,
// disco-vm ADR 26-11-23-458), bounded as a start of it is: one create, not a
// machine made stopped and then started, which on archil stops the sandbox
// Archil makes running only for the start to boot it again. A create whose
// start fails leaves no machine.
func (h *poolMachine) create(ctx context.Context, pool *model.Pool, scope string, networks []string) (engine.Instance, error) {
	// The driver checks its account through its provider's API first (boxd's
	// Check: that the key is accepted, and the region enabled), so an
	// account it refuses fails here, saying why, rather than at the machine.
	// A service that does not answer is waited on, as any other call is.
	if err := checkDriver(ctx, h.engine, sandbox.ErrPoolNotReachable); err != nil {
		return engine.Instance{}, fmt.Errorf("pool %s: %w", pool.ID, err)
	}
	// The boot's deadline is bootCtx's, which the engine's own timeout,
	// the same and begun later, never comes before: a create it ends is
	// told by bootCtx's error.
	bootCtx, cancel := context.WithTimeout(ctx, poolStartTimeout)
	defer cancel()
	inst, err := h.engine.Create(bootCtx, engine.CreateOptions{
		Name: scope, Networks: networks,
		URLs:  machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOn, Strict: true}},
		Start: &engine.StartOptions{Timeout: poolStartTimeout},
	})
	if err != nil {
		// A create its deadline ended was a boot that did not finish, the
		// guest's, as a start's is: never waited on, or a guest that never
		// boots would be made, waited on and removed at every pass
		// (REVIEW.md). A service that does not answer before then is
		// waited on, as any other call is.
		if bootCtx.Err() == nil {
			err = notReachable(err)
		}
		return engine.Instance{}, fmt.Errorf("create pool %s's machine from the %s driver's base: %w", pool.ID, h.engine.Driver.Name(), err)
	}
	return inst, nil
}

// poolNetworks is the networks a pool's machine and its sandboxes' are made
// on, as its driver fences them: the pool's own scope on networks, and none
// at its provider's edge.
func poolNetworks(fencing sandboxruntime.Fencing, scope string) []string {
	if fencing == sandboxruntime.FencedOnNetwork {
		return []string{scope}
	}
	return nil
}

// placement is what the pool's machine is made the pool's box with: its
// bootstrap, naming its own engine, the network its sandboxes' machines are
// made on, where its agent listens, and its driver's machine limit; and the
// agent and bundles for the machine's platform. Its URLs are the server's
// own view of them (serviceEndpoints), so a pass on a driver whose URLs mint
// a credential mints none while those held are in their first half-life.
func (h *poolMachine) placement(ctx context.Context, pool *model.Pool, inst engine.Instance, guest machine.Guest, maker poolruntime.PoolMaker, scope string, networks []string, maxMachines map[machine.OS]int) (poolruntime.BoxPlacement, error) {
	bootstrap, err := maker.Bootstrap(ctx)
	if err != nil {
		return poolruntime.BoxPlacement{}, err
	}
	listen, err := h.agentListenURL(ctx, inst)
	if err != nil {
		return poolruntime.BoxPlacement{}, err
	}
	bootstrap.ControlPlaneURL = firstNonEmpty(bootstrap.ControlPlaneURL, h.controlPlaneURL)
	bootstrap.ProjectID = firstNonEmpty(bootstrap.ProjectID, pool.ProjectID)
	bootstrap.PoolID = firstNonEmpty(bootstrap.PoolID, pool.ID)
	bootstrap.AgentListenURL = listen
	bootstrap.Runtime = poolagent.RuntimeMachine
	bootstrap.Driver = h.engine.Driver.Name()
	bootstrap.MachinePool = scope
	bootstrap.MachineBase = h.base
	// The provider's driver configuration, without its secret, so the
	// pool's engine calls the same endpoints and region as the provider's
	// (ADR 26-11-11-080 §6). It is in the placement's fingerprint: a pool
	// whose provider has none keeps the fingerprint it had.
	bootstrap.DriverConfig = h.driverConfig
	if h.keys.inMachine() {
		// The pool's engine authenticates as its machine (ADR 26-11-11-080
		// §9): the driver's in-machine field on, and no key file to read.
		config, err := withInMachine(h.driverConfig, h.keys.inMachineField)
		if err != nil {
			return poolruntime.BoxPlacement{}, err
		}
		bootstrap.DriverConfig = config
	}
	// The key file holds a sentinel for the driver's key, which the pool's
	// engine has its proxy swap (ADR 26-11-11-080 §9).
	bootstrap.KeySentinel = h.keys.sentinel()
	// The network its sandboxes' machines are made on, and none at an edge.
	if len(networks) > 0 {
		bootstrap.MachineNetwork = networks[0]
	}
	// The driver's machine limit, which only the provider's key may read, so
	// the agent reports it (ADR 26-11-15-831 §3). It is in the placement's
	// fingerprint: a pool on a driver that reports none keeps the one it had.
	bootstrap.MaxMachines = maxMachines
	// Where the server reaches the pool's agent, its ingress URL, and where
	// its sandboxes' machines, its peers, reach the pool, its pool URL, from
	// which the agent derives their endpoints and their fence (ADR
	// 26-10-29-481 §§1–2, disco-vm ADR 26-11-14-610). Neither stands in for
	// the other. A driver with no path between its machines has no pool URL,
	// and carries no pool.
	ingress, _, err := h.endpoints.endpoint(ctx, h.engine, inst, machine.URLIngress)
	if err != nil {
		return poolruntime.BoxPlacement{}, fmt.Errorf("pool %s's machine's ingress URL: %w", pool.ID, notReachable(err))
	}
	peer, _, err := h.endpoints.endpoint(ctx, h.engine, inst, machine.URLPool)
	if err != nil {
		return poolruntime.BoxPlacement{}, fmt.Errorf("pool %s's machine's pool URL: %w", pool.ID, notReachable(err))
	}
	bootstrap.IngressURL, bootstrap.PoolURL = ingress.URL, peer.URL
	if err := bootstrap.Validate(); err != nil {
		return poolruntime.BoxPlacement{}, fmt.Errorf("pool %s's bootstrap: %w", pool.ID, err)
	}
	info, err := guest.Info(ctx)
	if err != nil {
		return poolruntime.BoxPlacement{}, fmt.Errorf("read pool %s's machine's platform: %w", pool.ID, err)
	}
	box, err := maker.Box(ctx, imagecache.Platform{OS: info.OS, Architecture: info.Arch})
	if err != nil {
		return poolruntime.BoxPlacement{}, err
	}
	// The pool's box hands over to a new agent beside the running one when
	// what it runs moves, rather than restarting it, so its streams are not
	// cut (ADR 26-11-26-536 §§1, 4): every driver's but one whose machines
	// this server's engine holds on this host, whose host agent is not
	// staged (#64).
	caps := h.engine.Driver.Capabilities()
	return poolruntime.BoxPlacement{Bootstrap: bootstrap, Box: box, Env: h.policy, Outbound: pool.Outbound, Fence: held(caps), HandOver: !held(caps)}, nil
}

// held reports whether a driver's machines are held by this server's engine
// under shims, on this host (vz, libkrun, hcs), which disco-vm says by what
// the driver reports, never by its name (ADR 26-11-11-963 §1): its guests'
// ports are reached through the shim, and it issues no key. Such a pool
// machine's network device is shared with the host and the driver's other
// guests, so it is fenced before anything of the pool is placed (§6). boxd's
// pool machine is not: its fence is the pool's network (963 Deferred).
func held(caps machine.Capabilities) bool {
	return caps.Ports && caps.Keys.Mode == machine.KeysNone
}

// agentListenURL is where the pool agent listens in its machine: where the
// driver's service for the machine listens (Driver.Listen), which its
// ingress and pool URLs reach, as a machine sandbox's agent does (ADR
// 26-10-27-345 §2). Only TCP is taken: no remote driver serves an instance on
// anything else.
func (h *poolMachine) agentListenURL(ctx context.Context, inst engine.Instance) (string, error) {
	listen, err := h.engine.Driver.Listen(ctx, inst)
	if err != nil {
		return "", fmt.Errorf("the %s driver's service of machine %s: %w", h.engine.Driver.Name(), inst.Name, notReachable(err))
	}
	address, ok := strings.CutPrefix(listen, "tcp:")
	if !ok || address == "" {
		return "", fmt.Errorf("the %s driver gives machine %s's service %q, and the pool agent listens on TCP alone", h.engine.Driver.Name(), inst.Name, listen)
	}
	return "http://" + address, nil
}

// acquireAgentClient reaches the pool agent at its machine's ingress URL
// alone (ADR 26-10-09-106 §3, disco-vm ADR 26-11-14-610): where the pool's
// controller reaches it, boxd's HTTPS URL, the same across stop and start.
// It never falls back to the pool URL, which only the pool's peers reach. On
// boxd the URL needs no credential of the driver's; the agent authenticates
// every caller but its health and metadata. Through an edge that answers only
// a credential (archil), every request carries the server's own, over
// HTTP/1.1, the control-plane channel's included (ADR 26-11-16-619 §2).
//
// A pool whose machine is gone is one its reconcile creates again, so on this
// path that is the pool not reachable yet, which an explicit start waits out
// (#893), and not an answer; it still matches errNoPoolMachine for what reads
// that.
func (h *poolMachine) acquireAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	inst, err := h.instance(ctx, pool)
	if errors.Is(err, errNoPoolMachine) {
		return nil, fmt.Errorf("%w: %w", sandbox.ErrPoolNotReachable, err)
	}
	if err != nil {
		return nil, err
	}
	url, client, err := h.endpoints.endpoint(ctx, h.engine, inst, machine.URLIngress)
	if err != nil {
		return nil, fmt.Errorf("pool %s's machine: %w", pool.ID, notReachable(err))
	}
	return transport.NewHTTPClientLeaseWithBaseURL(client, strings.TrimRight(url.URL, "/"), nil), nil
}

// removePoolHost removes the pool's machine, then the pool's keys, then
// everything in the pool's own scope. The machine and the keys go first, so
// neither its agent nor anything holding a copy of its key is left to make
// more in the scope while it is emptied; the provider's own key empties it.
func (h *poolMachine) removePoolHost(ctx context.Context, pool *model.Pool) error {
	inst, err := h.instance(ctx, pool)
	switch {
	case errors.Is(err, errNoPoolMachine):
	case err != nil:
		return err
	default:
		if err := h.engine.Remove(ctx, inst, true); err != nil {
			return err
		}
		h.endpoints.forget(inst.ID)
	}
	// Every key of the pool is revoked once the machine that held them is
	// gone (ADR 26-10-23-551 §4).
	if err := h.keys.revokeAll(ctx, pool, decodeRuntimeState(pool).Key); err != nil {
		return err
	}
	return h.emptyPoolScope(ctx, pool)
}

// emptyPoolScope force-removes every machine in the pool's own disco-vm scope
// (machinePool), where its agent makes its sandboxes' machines and its
// snapshots (ADR 26-11-11-963 §2): a committed snapshot, and a build's machine
// still running, are left there by a pool's removal otherwise, since the
// server's engine sees only the provider's scope and the agent that would
// have removed them went with the pool's machine. The driver is opened in
// that scope with the provider's settings, key and dial (scopeEngine). A
// scope that is already empty is emptied; a listing that fails, or a machine
// that is not removed, is an error, so the pool's removal is tried again
// rather than finished over what is left. It removes what the driver lists:
// on boxd, a build machine's Destroy takes its commit's snapshot with it while
// that is still pending, and a failed snapshot with no machine is listed, so
// no snapshot of the scope is left unseen.
func (h *poolMachine) emptyPoolScope(ctx context.Context, pool *model.Pool) error {
	scope, err := machinePool(pool.ID)
	if err != nil {
		return err
	}
	e, err := h.scopeEngine(scope)
	if err != nil {
		return err
	}
	insts, err := e.List(ctx)
	if err != nil {
		return fmt.Errorf("pool %s's scope %s cannot be listed: %w", pool.ID, scope, notReachable(err))
	}
	var errs []error
	for _, inst := range insts {
		if err := e.Remove(ctx, inst, true); err != nil {
			errs = append(errs, fmt.Errorf("remove %s from pool %s's scope %s: %w", inst.Name, pool.ID, scope, err))
		}
	}
	return errors.Join(errs...)
}

// writeState records the machine on the pool row, and the credential its
// host holds.
func (h *poolMachine) writeState(pool *model.Pool, record poolRuntimeState) error {
	state, err := json.Marshal(record)
	if err != nil {
		return err
	}
	pool.RuntimeState = state
	pool.HostCredential = h.keys.hostCredential(record.Key)
	return nil
}

// markStarted is a pool whose machine was just made or started: its agent has
// not reported since.
func markStarted(pool *model.Pool) {
	pool.Ready = false
	pool.Schedulable = false
	pool.Degraded = false
	pool.SetState(model.PoolStateRegistering)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// state is the machine's power state, as far as anyone can tell. A remote
// machine whose service does not answer (boxd's API) is neither: it may be
// running, so it is not started again, and it is not stopped by repair, which
// would end a healthy pool for a network fault. The pool reconciler waits on
// ErrPoolNotReachable rather than failing the pool.
//
// The engine reads Unknown for every failure to ask, a missing, refused or
// too narrow credential among them, and waiting would hide that forever. So
// Unknown asks the machine once more, through Engine.Guest, which asks the
// same service by another call (an attach) and returns its error: a machine
// its provider paused answers ErrPaused, which converge resumes, and only a
// service that cannot be reached is waited on; any other error is the
// pool's.
func (h *poolMachine) state(ctx context.Context, pool *model.Pool, inst engine.Instance) (engine.State, error) {
	state := h.engine.State(ctx, inst)
	if state != engine.Unknown {
		return state, nil
	}
	_, err := h.engine.Guest(ctx, inst)
	switch {
	case err == nil:
		return engine.Running, nil
	case errors.Is(err, machine.ErrNotRunning):
		return engine.Stopped, nil
	case errors.Is(err, machine.ErrPaused):
		// Paused by its provider: not running, and resumed by Start alone.
		return engine.Paused, nil
	default:
		return state, fmt.Errorf("pool %s's machine cannot be asked for its state: %w", pool.ID, notReachable(err))
	}
}

// checkDriver is the engine's driver checking what its first machine needs
// (boxd's account and region, a held driver's hypervisor), saying which it
// was: a service that does not answer, marked with unanswered, or a refusal.
// A pool's create asks it first, marking sandbox.ErrPoolNotReachable so the
// pool waits it out, and so does a check of the provider (ADR 26-11-15-089
// §3), marking sandbox.ErrProviderNotReachable: a provider check has no pool,
// so its message never names a pool agent. Both say the same thing otherwise.
func checkDriver(ctx context.Context, e *engine.Engine, unanswered error) error {
	err := e.Driver.Check(ctx)
	if err == nil {
		return nil
	}
	if unreachable(err) {
		return fmt.Errorf("the %s driver's account cannot be checked yet: %w: %w", e.Driver.Name(), err, unanswered)
	}
	if isLocal(e.Driver) {
		return fmt.Errorf("the %s driver cannot run a machine on this host: %w", e.Driver.Name(), err)
	}
	return fmt.Errorf("the %s driver refuses its account: %w", e.Driver.Name(), err)
}

// notReachable is err marked sandbox.ErrPoolNotReachable when it is a remote
// driver's service not answering (unreachable), and err as it is otherwise.
// Every call that asks the driver's service about the pool's machine, or for
// its keys, reads its error through it (REVIEW.md).
func notReachable(err error) error {
	if unreachable(err) {
		return fmt.Errorf("%w: %w", err, sandbox.ErrPoolNotReachable)
	}
	return err
}

// unreachable reports whether err is a remote driver's service not answering,
// rather than answering with a refusal. It reads disco-vm's
// machine.ErrUnavailable, which a driver over HTTP gives for a 502, 503 or
// 504 (archil's REST API), and the transports the linked remote drivers use:
// a network connection (archil's HTTP client fails as one), and gRPC (boxd's
// API). archil's process API, a WebSocket, is reached only by a guest
// operation, whose error is the guest's, so nothing of it is read here. It is
// the one place this package reads a driver's errors, and a remote driver on
// another transport is added here (REVIEW.md).
func unreachable(err error) bool {
	// An endpoint the server's policy refuses is the provider's to fix,
	// never a fault to wait out, though it fails as a dial does (ADR
	// 26-11-11-080 §3). gRPC keeps only a dial error's text.
	if errors.Is(err, sandbox.ErrEndpointRefused) || strings.Contains(status.Convert(err).Message(), sandbox.ErrEndpointRefused.Error()) {
		return false
	}
	var netErr net.Error
	if errors.Is(err, machine.ErrUnavailable) || errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded:
		return true
	}
	return false
}

// running finds the pool's machine and requires it to be running: the console
// and the log are read from inside it.
func (h *poolMachine) running(ctx context.Context, pool *model.Pool) (engine.Instance, error) {
	inst, err := h.instance(ctx, pool)
	if err != nil {
		return engine.Instance{}, err
	}
	state, err := h.state(ctx, pool, inst)
	if err != nil {
		return engine.Instance{}, err
	}
	if state != engine.Running {
		return engine.Instance{}, fmt.Errorf("pool %s's machine is %s", pool.ID, state)
	}
	return inst, nil
}

func (h *poolMachine) openConsole(ctx context.Context, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.Stream, error) {
	inst, err := h.running(ctx, pool)
	if err != nil {
		return nil, err
	}
	client, err := h.engine.Guest(ctx, inst)
	if err != nil {
		return nil, err
	}
	argv := h.shell
	if len(opts.Command) > 0 {
		argv = append(slices.Clone(h.shell), "-c", `exec "$@"`, "discobox-console")
		argv = append(argv, opts.Command...)
	}
	process, err := client.Exec(ctx, machine.ExecRequest{
		Argv: argv,
		// A login environment has no TERM: without one, clear, less, top and
		// vim refuse or degrade.
		Env:  []string{"TERM=xterm-256color"},
		TTY:  len(opts.Command) == 0 || opts.TTY,
		Rows: terminalSize(opts.Rows),
		Cols: terminalSize(opts.Cols),
	})
	if err != nil {
		return nil, fmt.Errorf("open a shell in pool %s's machine: %w", pool.ID, err)
	}
	return newGuestProcess(process, true), nil
}

// resolveDriverKey is what the pool's proxy puts in place of the sentinel
// the pool's machine holds, by the record of what was placed there
// (poolKeys.resolve).
func (h *poolMachine) resolveDriverKey(pool *model.Pool, sentinel, host string) (string, error) {
	return h.keys.resolve(decodeRuntimeState(pool).Key, sentinel, host)
}

func (h *poolMachine) openLogs(ctx context.Context, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	inst, err := h.running(ctx, pool)
	if err != nil {
		return nil, err
	}
	client, err := h.engine.Guest(ctx, inst)
	if err != nil {
		return nil, err
	}
	process, err := client.Exec(ctx, machine.ExecRequest{Argv: h.logs(opts)})
	if err != nil {
		return nil, fmt.Errorf("read pool %s's machine log: %w", pool.ID, err)
	}
	// The command reads nothing; its stdin is closed so it cannot wait on it.
	if err := process.CloseStdin(); err != nil {
		_ = process.Close()
		return nil, err
	}
	return &sandbox.PoolLogStream{Source: h.logSource, ReadCloser: newGuestProcess(process, false)}, nil
}

func terminalSize(n int) uint16 {
	if n <= 0 || n > 0xffff {
		return 0
	}
	return uint16(n)
}

// guestProcess is a process in a machine, read as one stream: a TTY carries
// stderr in its output anyway, and a log command explains itself on stderr,
// which is part of what the operator asked to read. Closing it kills the
// process, which is the only way a followed log ends. The output passes through
// an unbuffered pipe, so Wait returns only once the stream has been read to its
// end (or closed): read it out, as the console handler does, before waiting.
type guestProcess struct {
	process machine.Process
	reader  *io.PipeReader
	stderr  *io.PipeReader

	done     chan struct{}
	code     int
	err      error
	closeOne sync.Once
}

func newGuestProcess(process machine.Process, separate bool) *guestProcess {
	reader, writer := io.Pipe()
	p := &guestProcess{process: process, reader: reader, done: make(chan struct{})}
	var stderrWriter *io.PipeWriter
	var errOutput io.Writer = writer
	if separate {
		p.stderr, stderrWriter = io.Pipe()
		errOutput = stderrWriter
	}
	go func() {
		defer close(p.done)
		p.code, p.err = process.Wait(writer, errOutput)
		if stderrWriter != nil {
			_ = stderrWriter.CloseWithError(p.err)
		}
		// A process that ran and exited ends the stream; its exit code is
		// Wait's to report, not the reader's. A stream that broke first is
		// the reader's error, so a followed log cut off by a dropped
		// connection does not read as one that ended.
		_ = writer.CloseWithError(p.err)
	}()
	return p
}

func (p *guestProcess) Stderr() io.Reader {
	if p.stderr == nil {
		return nil
	}
	return p.stderr
}
func (p *guestProcess) CloseWrite() error { return p.process.CloseStdin() }

func (p *guestProcess) Read(b []byte) (int, error)  { return p.reader.Read(b) }
func (p *guestProcess) Write(b []byte) (int, error) { return p.process.Write(b) }

func (p *guestProcess) Resize(_ context.Context, rows, cols int) error {
	return p.process.Resize(terminalSize(rows), terminalSize(cols))
}

func (p *guestProcess) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, p.err
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (p *guestProcess) Close() error {
	p.closeOne.Do(func() {
		_ = p.process.Close()
		_ = p.reader.Close()
		if p.stderr != nil {
			_ = p.stderr.Close()
		}
		<-p.done
	})
	return nil
}

// replacesAgent is whether a pass's apply may restart an agent already
// running in the pool's machine, so the agent's start is recorded in place
// before it: one running before this pass (started false), adopted or
// recorded unprovisioned included, and one this pass booted from a disk no
// record says was provisioned or whose bundles or placement moved. A machine
// this pass created ran no agent before its box start, and one it booted and
// places again with nothing moved (a repair) restarts nothing: it keeps the
// box start its boot recorded, and its booted agent's report counts.
func replacesAgent(created, started, provisioned, moved bool) bool {
	return !created && (!started || !provisioned || moved)
}
