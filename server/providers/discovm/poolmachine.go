package discovm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

const (
	// poolStartTimeout bounds a pool machine's boot until its guest
	// operations answer.
	poolStartTimeout = 10 * time.Minute
	// poolStopTimeout is how long a pool machine gets to power off in order
	// before it is forced off.
	poolStopTimeout = time.Minute
)

// poolMachine hosts a pool in a disco-vm machine of its own, in the provider's
// project, named for its pool so a create that was lost is found rather than
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
	// base is the driver's base the machine, and its sandboxes' machines, are
	// created from (Config.Base); empty is the first the driver lists.
	base string
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
}

func newPoolMachine(e *engine.Engine, cfg Config) *poolMachine {
	return &poolMachine{
		engine:          e,
		base:            strings.TrimSpace(cfg.Base),
		controlPlaneURL: strings.TrimSpace(cfg.ControlPlaneURL),
		policy:          cfg.Env(),
		keys:            newPoolKeys(e.Driver, cfg),
		shell:           []string{"/bin/bash", "-l"},
		logs:            journalCommand,
		logSource:       "pool machine journal",
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
	// Project is the disco-vm project the pool makes its sandboxes' machines
	// in, which is also the network the pool's machine and theirs share.
	Project string `json:"project,omitempty"`
	// Box is the agent, the bundles and the placement placed.
	Box poolruntime.BoxRecord `json:"box,omitzero"`
	// Key is the provisioning key placed, without its secret.
	Key *poolKeyRecord `json:"key,omitempty"`
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

// poolProject is the pool's own disco-vm project, derived from its ID within
// disco-vm's project rule (ADR 26-10-23-551 §3: pool_x becomes pool-x). It is
// the project the pool agent makes its sandboxes' machines in, the network
// the pool's machine and theirs share, and the name of the pool's machine in
// the provider's project, which it never equals (prov-…).
func poolProject(poolID string) (string, error) {
	project := strings.ReplaceAll(strings.ToLower(poolID), "_", "-")
	if err := machine.ValidateProject(project); err != nil {
		return "", fmt.Errorf("pool %q has no disco-vm project: %w", poolID, err)
	}
	return project, nil
}

// poolMachineName is the pool's machine in the provider's project.
func poolMachineName(poolID string) (string, error) { return poolProject(poolID) }

// errNoPoolMachine is a pool whose machine was never created, or was removed.
var errNoPoolMachine = errors.New("the pool has no machine")

// instance finds the pool's machine, or reports errNoPoolMachine.
func (h *poolMachine) instance(ctx context.Context, pool *model.Pool) (engine.Instance, error) {
	name, err := poolMachineName(pool.ID)
	if err != nil {
		return engine.Instance{}, err
	}
	inst, err := h.engine.Get(ctx, name)
	if errors.Is(err, engine.ErrNotFound) {
		return engine.Instance{}, fmt.Errorf("pool %s: %w: it is created when the pool reconciles", pool.ID, errNoPoolMachine)
	}
	return inst, err
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
	return h.converge(ctx, pool, maker, onceBegin(begin), false)
}

// repairPoolHost restarts the pool's machine in place, on the same disk, and
// provisions and enrolls it again whatever the record says, so a server that
// lost the pool's key is mended by the provider (ADR 26-10-24-953 §4). A pool
// with no machine is made one.
func (h *poolMachine) repairPoolHost(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker, begin func(context.Context) error) error {
	gate := onceBegin(begin)
	inst, err := h.instance(ctx, pool)
	if errors.Is(err, errNoPoolMachine) {
		return h.converge(ctx, pool, maker, gate, true)
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
	return h.converge(ctx, pool, maker, gate, true)
}

// converge brings the pool's machine to what the provider wants (ADR
// 26-10-23-551 §1): made from the base on the pool's network when there is
// none, started when it is stopped, given a provisioning key that is not due
// for renewal, and provisioned when what it was placed with moved — its
// bundles, or its bootstrap and environment, which the provider's
// configuration decides — or always when force. Its record is written to the pool as it goes, so a pass that
// fails part way still says what it placed, a key above all.
func (h *poolMachine) converge(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker, gate func(context.Context) error, force bool) error {
	if maker == nil {
		return errors.New("a pool maker is required")
	}
	if err := h.keys.check(); err != nil {
		return err
	}
	project, err := poolProject(pool.ID)
	if err != nil {
		return err
	}
	if !h.engine.Driver.Capabilities().Isolation {
		// A pool machine is on a network of the pool's own, and its
		// sandboxes' machines isolated on it; a driver that cannot is not
		// enabled (ADR 26-10-23-551 §4).
		return fmt.Errorf("the %s driver cannot isolate machines or place them on networks, which a pool's machine and its sandboxes' need", h.engine.Driver.Name())
	}
	record := decodeRuntimeState(pool)
	inst, err := h.instance(ctx, pool)
	if err != nil && !errors.Is(err, errNoPoolMachine) {
		return err
	}
	made, started := false, false
	if err != nil {
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
		record = poolRuntimeState{}
		if inst, err = h.create(ctx, pool, project); err != nil {
			return err
		}
		made = true
	}
	record.Instance, record.Project = inst.ID, project
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
		if err := h.engine.Start(ctx, inst, engine.StartOptions{Timeout: poolStartTimeout}); err != nil {
			return fmt.Errorf("start pool %s's machine: %w", pool.ID, err)
		}
		started = true
	}
	if made || started {
		markStarted(pool)
	}
	guest, err := h.engine.Guest(ctx, inst)
	if err != nil {
		return fmt.Errorf("reach pool %s's machine: %w", pool.ID, err)
	}
	// The key is placed before anything the agent runs on, and recorded the
	// moment it is: an issued key whose record is lost would be one nobody
	// renews.
	key, err := h.keys.ensure(ctx, pool, guest, record.Key)
	record.Key = key
	if werr := h.writeState(pool, record); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	provisioned := record.Box.Agent != ""
	placement, err := h.placement(ctx, pool, inst, guest, maker, project)
	if err != nil {
		if provisioned && !force {
			slog.WarnContext(ctx, "keeping the pool's machine as it is: what it is made of does not resolve", "pool", pool.ID, "error", err)
			return nil
		}
		return err
	}
	current, err := record.Box.Current(placement)
	if err != nil {
		return err
	}
	if !force && current {
		return nil
	}
	// The apply restarts the agent the moved bundles carry, so placement is
	// closed first, as it is before any agent is replaced.
	if err := gate(ctx); err != nil {
		return err
	}
	box, err := poolruntime.ProvisionBox(ctx, guest, maker, placement)
	if err != nil {
		if provisioned && !force && !made && !started {
			// A running pool is never taken down over its bundles (ADR
			// 26-10-24-953 §5): it keeps what it last provisioned, and the
			// next reconcile tries again.
			slog.WarnContext(ctx, "keeping the pool's machine as it is: its moved bundles did not apply", "pool", pool.ID, "error", err)
			return nil
		}
		return fmt.Errorf("provision pool %s's machine: %w", pool.ID, err)
	}
	if provisioned && record.Box.Placement != box.Placement {
		if err := poolruntime.RestartPoolUnits(ctx, guest); err != nil {
			return fmt.Errorf("restart pool %s's agent on its new placement: %w", pool.ID, err)
		}
	}
	record.Box = box
	return h.writeState(pool, record)
}

// create makes the pool's machine from the base, on the pool's network alone,
// off the org's default one, so no other machine of the provider's reaches it
// (ADR 26-10-23-551 §4). It is not isolated: boxd peers an isolated machine
// only with machines that are not, so the pool's machine keeps what the
// driver gives a machine (the pool's HostCredential says so).
func (h *poolMachine) create(ctx context.Context, pool *model.Pool, project string) (engine.Instance, error) {
	base, err := h.baseRef(ctx)
	if err != nil {
		return engine.Instance{}, err
	}
	inst, err := h.engine.Create(ctx, base, engine.CreateOptions{Name: project, Networks: []string{project}})
	if err != nil {
		return engine.Instance{}, fmt.Errorf("create pool %s's machine from %s: %w", pool.ID, base, err)
	}
	return inst, nil
}

// placement is what the pool's machine is made the pool's box with: its
// bootstrap, naming its own engine, the network its sandboxes' machines are
// made on, and where its agent listens; and the agent and bundles for the
// machine's platform.
func (h *poolMachine) placement(ctx context.Context, pool *model.Pool, inst engine.Instance, guest machine.Guest, maker poolruntime.PoolMaker, project string) (poolruntime.BoxPlacement, error) {
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
	bootstrap.MachineProject = project
	bootstrap.MachineBase = h.base
	bootstrap.MachineNetwork = project
	// Where the server reaches the pool's agent, and where its sandboxes'
	// machines reach the pool, from which the agent derives their endpoints
	// and their fence (ADR 26-10-29-481 §§1–2). A driver with no path between
	// its machines has no private URL, and carries no pool.
	if bootstrap.PublicURL, err = h.engine.URL(ctx, inst); err != nil {
		return poolruntime.BoxPlacement{}, fmt.Errorf("pool %s's machine's public URL: %w", pool.ID, err)
	}
	if bootstrap.PrivateURL, err = h.engine.PrivateURL(ctx, inst); err != nil {
		return poolruntime.BoxPlacement{}, fmt.Errorf("pool %s's machine's private URL: %w", pool.ID, err)
	}
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
	return poolruntime.BoxPlacement{Bootstrap: bootstrap, Box: box, Env: h.policy}, nil
}

// agentListenURL is where the pool agent listens in its machine: where the
// driver's service for the machine listens (Service.Listen), which its one
// URL reaches, as a machine sandbox's agent does (ADR 26-10-27-345 §2). Only
// TCP is taken: no remote driver serves an instance on anything else.
func (h *poolMachine) agentListenURL(ctx context.Context, inst engine.Instance) (string, error) {
	service, err := h.engine.Driver.Service(ctx, inst)
	if err != nil {
		return "", fmt.Errorf("the %s driver's service of machine %s: %w", h.engine.Driver.Name(), inst.Name, err)
	}
	address, ok := strings.CutPrefix(service.Listen, "tcp:")
	if !ok || address == "" {
		return "", fmt.Errorf("the %s driver gives machine %s's service %q, and the pool agent listens on TCP alone", h.engine.Driver.Name(), inst.Name, service.Listen)
	}
	return "http://" + address, nil
}

// acquireAgentClient reaches the pool agent at its machine's one URL (ADR
// 26-10-09-106 §3): boxd's public HTTPS URL, the same across stop and start.
// The URL needs no credential of the driver's; the agent authenticates every
// caller but its health and metadata.
func (h *poolMachine) acquireAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	inst, err := h.instance(ctx, pool)
	if err != nil {
		return nil, err
	}
	url, err := h.engine.URL(ctx, inst)
	if err != nil {
		if unreachable(err) {
			return nil, fmt.Errorf("pool %s's machine: %w: %w", pool.ID, err, sandbox.ErrPoolNotReachable)
		}
		return nil, fmt.Errorf("pool %s's machine: %w", pool.ID, err)
	}
	return transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, strings.TrimRight(url, "/"), nil), nil
}

// baseRef is the ID of the driver's base a new pool machine is created from:
// the configured one, found by its ID alone, or the first the driver lists. It
// asks the driver each time, as the driver reads its service for each call.
func (h *poolMachine) baseRef(ctx context.Context) (string, error) {
	name := h.engine.Driver.Name()
	bases, err := h.engine.Driver.Bases(ctx)
	if err != nil {
		return "", fmt.Errorf("list the %s driver's bases: %w", name, err)
	}
	if h.base == "" {
		if len(bases) == 0 {
			return "", fmt.Errorf("the %s driver offers no base to boot a pool's machine from: set the provider's base to one's ID", name)
		}
		return bases[0].ID, nil
	}
	for _, base := range bases {
		if base.ID == h.base {
			return base.ID, nil
		}
	}
	return "", fmt.Errorf("the %s driver has no base with ID %q, which the provider's base names", name, h.base)
}

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
	}
	// Every key of the pool is revoked once the machine that held them is
	// gone (ADR 26-10-23-551 §4).
	return h.keys.revokeAll(ctx, pool, decodeRuntimeState(pool).Key)
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
// Unknown asks the machine once more, through Engine.Guest, which makes the
// same call to the service and returns its error, and only a service that
// cannot be reached is waited on; any other error is the pool's.
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
	case unreachable(err):
		return state, fmt.Errorf("pool %s's machine cannot be asked for its state: %w: %w", pool.ID, err, sandbox.ErrPoolNotReachable)
	default:
		return state, fmt.Errorf("pool %s's machine cannot be asked for its state: %w", pool.ID, err)
	}
}

// unreachable reports whether err is a remote driver's service not answering,
// rather than answering with a refusal. disco-vm gives no error of its own for
// that, so this reads the transports the linked remote drivers use: a network
// connection, and gRPC (boxd's API). It is the one place this package reads a
// driver's errors, and a remote driver on another transport is added here
// (REVIEW.md).
func unreachable(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
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

func (h *poolMachine) openConsole(ctx context.Context, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.PTY, error) {
	inst, err := h.running(ctx, pool)
	if err != nil {
		return nil, err
	}
	client, err := h.engine.Guest(ctx, inst)
	if err != nil {
		return nil, err
	}
	process, err := client.Exec(ctx, machine.ExecRequest{
		Argv: h.shell,
		// A login environment has no TERM: without one, clear, less, top and
		// vim refuse or degrade.
		Env:  []string{"TERM=xterm-256color"},
		TTY:  true,
		Rows: terminalSize(opts.Rows),
		Cols: terminalSize(opts.Cols),
	})
	if err != nil {
		return nil, fmt.Errorf("open a shell in pool %s's machine: %w", pool.ID, err)
	}
	return newGuestProcess(process), nil
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
	return &sandbox.PoolLogStream{Source: h.logSource, ReadCloser: newGuestProcess(process)}, nil
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
// process, which is the only way a followed log ends.
type guestProcess struct {
	process machine.Process
	reader  *io.PipeReader

	done     chan struct{}
	code     int
	err      error
	closeOne sync.Once
}

func newGuestProcess(process machine.Process) *guestProcess {
	reader, writer := io.Pipe()
	p := &guestProcess{process: process, reader: reader, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.code, p.err = process.Wait(writer, writer)
		// A process that ran and exited ends the stream; its exit code is
		// Wait's to report, not the reader's. A stream that broke first is
		// the reader's error, so a followed log cut off by a dropped
		// connection does not read as one that ended.
		_ = writer.CloseWithError(p.err)
	}()
	return p
}

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
		<-p.done
	})
	return nil
}
