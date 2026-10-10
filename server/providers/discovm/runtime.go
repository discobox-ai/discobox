package discovm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// poolHost is what runs a pool's agent, and how an operator reaches it when
// the agent will not answer. Where that is follows from what the configured
// disco-vm driver reports it can do (newPoolHost), never from its name.
type poolHost interface {
	// ensurePoolHost brings up what runs the pool's agent. begin is called
	// before anything that starts an agent is created or started.
	ensurePoolHost(ctx context.Context, pool *model.Pool, begin func(context.Context) error) error
	// repairPoolHost restarts that host in place, keeping its disk.
	repairPoolHost(ctx context.Context, pool *model.Pool, begin func(context.Context) error) error
	// removePoolHost removes it and everything it kept. A host that was never
	// created is already removed.
	removePoolHost(ctx context.Context, pool *model.Pool) error
	openConsole(ctx context.Context, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.PTY, error)
	openLogs(ctx context.Context, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error)
}

// newPoolHost places a pool's agent where the driver's machines are (ADR
// 26-10-09-106 §1). A remote driver's machines belong to a service (boxd's
// API, a Docker daemon), not to the process that booted them, so the pool
// agent runs in a Linux machine of its own there. A local hypervisor's machines run on this host, so the pool agent
// runs beside the server as a host process.
func newPoolHost(e *engine.Engine) poolHost {
	if e.Driver.Capabilities().Remote {
		return newPoolMachine(e)
	}
	return &hostAgent{root: e.Root}
}

// Runtime is the discovm poolruntime.RuntimeProvider: one disco-vm engine, on
// the driver the provider configures, and where its pools are hosted.
type Runtime struct {
	engine *engine.Engine
	host   poolHost
	// agent is the disco-vm binary images are built with (Config.Agent).
	agent string
}

var _ poolruntime.RuntimeProvider = (*Runtime)(nil)

func newRuntime(cfg Config) (*Runtime, error) {
	e, err := openEngine(stateRoot(strings.TrimSpace(cfg.Driver)), cfg.Driver)
	if err != nil {
		return nil, err
	}
	return &Runtime{engine: e, host: newPoolHost(e), agent: strings.TrimSpace(cfg.Agent)}, nil
}

// newDriver constructs the named disco-vm driver. The name is disco-vm's, and
// so is the registry it is looked up in: this package knows no driver by name.
// Construction touches no hypervisor, so validation may call it too.
func newDriver(name string) (machine.Driver, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%s driver is required (one of %s)", ProviderType, strings.Join(machine.Names(), ", "))
	}
	driver, err := machine.New(name)
	if err != nil {
		return nil, fmt.Errorf("%s driver %q is not available in this build (have %s)", ProviderType, name, strings.Join(machine.Names(), ", "))
	}
	return driver, nil
}

// openEngine opens the state root for a driver, with its shims started as this
// binary's hidden subcommand rather than as a disco-vm binary nobody ships.
func openEngine(root, name string) (*engine.Engine, error) {
	driver, err := newDriver(name)
	if err != nil {
		return nil, err
	}
	e, err := engine.Open(root, driver)
	if err != nil {
		return nil, fmt.Errorf("open %s state %s: %w", ProviderType, root, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	e.ShimCommand = shimCommand(exe, e.Root, driver.Name())
	return e, nil
}

// Close releases nothing: the engine holds no process-wide resource, and a
// running machine is its shim's, or its remote service's, not this value's.
func (r *Runtime) Close() error { return nil }

// EnsurePool brings up the pool's host.
//
// It mints no bootstrap. A bootstrap is handed to a pool agent as it starts, and
// nothing starts one yet: a host pool agent is staged in #64, and the pool
// machine's image installs one in #123.
func (r *Runtime) EnsurePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, _ poolagent.MintBootstrap, _ []string, begin func(context.Context) error) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	return r.host.ensurePoolHost(ctx, pool, begin)
}

func (r *Runtime) RepairPool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, _ poolagent.MintBootstrap, _ string, _ []string, begin func(context.Context) error) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	return r.host.repairPoolHost(ctx, pool, begin)
}

func (r *Runtime) RemovePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	if err := r.host.removePoolHost(ctx, pool); err != nil {
		return err
	}
	pool.RuntimeState = nil
	pool.Ready = false
	pool.Schedulable = false
	pool.Degraded = false
	return nil
}

// errNoPoolAgent is every driver's answer until a discovm pool runs an agent.
var errNoPoolAgent = errors.New("a discovm pool runs no pool agent yet: a host pool agent is staged in #64, and a pool machine's in #127")

func (r *Runtime) AcquirePoolAgentClient(_ context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	return nil, errNoPoolAgent
}

func (r *Runtime) OpenConsole(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.PTY, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	return r.host.openConsole(ctx, pool, opts)
}

func (r *Runtime) OpenLogs(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	return r.host.openLogs(ctx, pool, opts)
}

func requirePool(pool *model.Pool) error {
	if pool == nil || pool.ID == "" {
		return errors.New("pool is required")
	}
	return nil
}
