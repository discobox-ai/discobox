package discovm

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// driver is what differs between disco-vm's hypervisors for a discovm pool:
// where the pool's agent runs, how an operator reaches that host when the
// agent will not answer, and which disco-vm images the driver's pools boot.
// The engine — images, instances, and their lifecycle — is the same for all of
// them, and the Runtime owns it.
type driver interface {
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
	// images are the disco-vm images the driver's pools boot, which
	// BuildGuestImage builds.
	images() []guestImage
}

// driverDefinition is one driver this build has: disco-vm's driver for the
// hypervisor, and the pool hosting on top of it.
type driverDefinition struct {
	machine func() (machine.Driver, error)
	pools   func(e *engine.Engine) driver
}

// drivers are the drivers in this build, by disco-vm's name for each. A file
// per driver adds its own, so a build carries exactly the hypervisors its OS
// can run.
var drivers = map[string]driverDefinition{}

// Runtime is the discovm poolruntime.RuntimeProvider: one disco-vm engine and
// the driver that decides how a pool is hosted on it.
type Runtime struct {
	engine *engine.Engine
	driver driver
}

var _ poolruntime.RuntimeProvider = (*Runtime)(nil)

func newRuntime(cfg Config) (*Runtime, error) {
	definition, err := lookupDriver(cfg.Driver)
	if err != nil {
		return nil, err
	}
	e, err := openEngine(stateRoot(), cfg.Driver, definition)
	if err != nil {
		return nil, err
	}
	return &Runtime{engine: e, driver: definition.pools(e)}, nil
}

// openEngine opens the state root for a driver, with its shims started as this
// binary's hidden subcommand rather than as a disco-vm binary nobody ships.
func openEngine(root, name string, definition driverDefinition) (*engine.Engine, error) {
	hypervisor, err := definition.machine()
	if err != nil {
		return nil, fmt.Errorf("%s driver %s: %w", ProviderType, name, err)
	}
	e, err := engine.Open(root, hypervisor)
	if err != nil {
		return nil, fmt.Errorf("open %s state %s: %w", ProviderType, root, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	e.ShimCommand = shimCommand(exe, e.Root, name)
	return e, nil
}

// Close releases nothing: the engine holds no process-wide resource, and a
// running machine is its shim's, or its remote service's, not this value's.
func (r *Runtime) Close() error { return nil }

// EnsurePool brings up the pool's host.
//
// It mints no bootstrap. A bootstrap is handed to a pool agent as it starts, and
// no driver starts one yet: vz stages the host pool agent in #64, and boxd's
// pool image installs it in #123.
func (r *Runtime) EnsurePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, _ poolagent.MintBootstrap, _ []string, begin func(context.Context) error) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	return r.driver.ensurePoolHost(ctx, pool, begin)
}

func (r *Runtime) RepairPool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, _ poolagent.MintBootstrap, _ string, _ []string, begin func(context.Context) error) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	return r.driver.repairPoolHost(ctx, pool, begin)
}

func (r *Runtime) RemovePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	if err := r.driver.removePoolHost(ctx, pool); err != nil {
		return err
	}
	pool.RuntimeState = nil
	pool.Ready = false
	pool.Schedulable = false
	pool.Degraded = false
	return nil
}

// errNoPoolAgent is every driver's answer until a discovm pool runs an agent.
var errNoPoolAgent = errors.New("a discovm pool runs no pool agent yet: the vz driver stages one in #64 and the boxd driver in #127")

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
	return r.driver.openConsole(ctx, pool, opts)
}

func (r *Runtime) OpenLogs(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	return r.driver.openLogs(ctx, pool, opts)
}

func requirePool(pool *model.Pool) error {
	if pool == nil || pool.ID == "" {
		return errors.New("pool is required")
	}
	return nil
}
