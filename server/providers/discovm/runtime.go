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
// agent runs in a Linux machine of its own there, booted from the driver's
// base (ADR 26-10-23-551 §1). A local hypervisor's machines run on this host,
// so the pool agent runs beside the server as a host process.
func newPoolHost(e *engine.Engine, base string) poolHost {
	if e.Driver.Capabilities().Remote {
		return newPoolMachine(e, base)
	}
	return &hostAgent{root: e.Root}
}

// Runtime is the discovm poolruntime.RuntimeProvider: one disco-vm engine, on
// the driver the provider configures, and where its pools are hosted.
type Runtime struct {
	engine *engine.Engine
	host   poolHost
}

var _ poolruntime.RuntimeProvider = (*Runtime)(nil)

// newRuntime opens the engine for the provider instance providerID.
func newRuntime(cfg Config, providerID string) (*Runtime, error) {
	driver, err := newDriver(cfg.Driver)
	if err != nil {
		return nil, err
	}
	project, err := engineProject(driver, providerID)
	if err != nil {
		return nil, err
	}
	e, err := openEngine(stateRoot(driver.Name()), driver, project)
	if err != nil {
		return nil, err
	}
	return &Runtime{engine: e, host: newPoolHost(e, strings.TrimSpace(cfg.Base))}, nil
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

// localProject is the disco-vm project (disco-vm ADR 0004) of everything a
// local driver's provider instances make in its state root. It is one for all
// of them, as the root is, because the engine counts the driver's
// running-guest cap (vz's two macOS guests) within a project, and that cap is
// the host's (stateRoot). It is not disco-vm's default project, which is the
// disco-vm CLI's, so what a person makes with the CLI is never discobox's.
const localProject = "discobox"

// engineProject is the disco-vm project a provider instance's engine opens.
//
// A remote driver's machines outlive any store that records them and are
// named for their project, which is how one store tells its own from the
// account's other contents, another server's included (disco-vm ADR 0004). So
// a remote driver's project is the provider instance's own (ADR 26-10-23-551
// §1), spelled from its ID within disco-vm's project rule: prov_x becomes
// prov-x. A local driver's is localProject.
func engineProject(driver machine.Driver, providerID string) (string, error) {
	if !driver.Capabilities().Remote {
		return localProject, nil
	}
	project := strings.ReplaceAll(strings.ToLower(providerID), "_", "-")
	if err := machine.ValidateProject(project); err != nil {
		return "", fmt.Errorf("%s provider %q has no disco-vm project: %w", ProviderType, providerID, err)
	}
	return project, nil
}

// openEngine opens one project of the state root for a driver, with its shims
// started as this binary's hidden subcommand rather than as a disco-vm binary
// nobody ships. The engine keeps its metadata in its own database in the
// root, not in the server's: a root is shared by every server of the user on
// the host, and a database serves one root.
func openEngine(root string, driver machine.Driver, project string) (*engine.Engine, error) {
	e, err := engine.Open(root, driver, nil, project)
	if err != nil {
		return nil, fmt.Errorf("open %s state %s: %w", ProviderType, root, err)
	}
	exe, err := os.Executable()
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	e.ShimCommand = shimCommand(exe, e.Root, driver.Name(), project)
	return e, nil
}

// Close releases the engine's database. A running machine is its shim's, or
// its remote service's, not this value's, and outlives it.
func (r *Runtime) Close() error { return r.engine.Close() }

// EnsurePool brings up the pool's host.
//
// It mints no bootstrap, because nothing here starts a pool agent: a pool
// machine is a driver base with nothing placed on it, and a local driver's
// host agent is not staged.
func (r *Runtime) EnsurePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, _ poolagent.MintBootstrap, begin func(context.Context) error) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	return r.host.ensurePoolHost(ctx, pool, begin)
}

func (r *Runtime) RepairPool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, _ poolagent.MintBootstrap, _ string, begin func(context.Context) error) error {
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
var errNoPoolAgent = errors.New("a vm pool runs no pool agent yet: a host pool agent is staged in #64, and a pool machine's in #127")

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

// BuildGuestImage builds nothing, on any driver. A remote driver's pool machine
// and its sandboxes boot from a driver base that the agent is placed on (ADR
// 26-10-23-551 §1, ADR 26-10-16-423 §7), so it has no image to build. A local
// driver's sandbox image (the macOS image) has no build spec in a checkout.
func (r *Runtime) BuildGuestImage(_ context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, _ sandbox.GuestImageBuildOptions) (*sandbox.GuestImageBuild, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	driver := r.engine.Driver.Name()
	if r.engine.Driver.Capabilities().Remote {
		return nil, fmt.Errorf("a %s pool on the %s driver boots its machines from the driver's base, with the agent placed on it, so it has no image to build: %w", ProviderType, driver, sandbox.ErrGuestImageBuildUnsupported)
	}
	return nil, fmt.Errorf("no VM image is defined for the %s driver to build: %w", driver, sandbox.ErrGuestImageBuildUnsupported)
}
