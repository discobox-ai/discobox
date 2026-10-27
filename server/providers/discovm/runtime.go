package discovm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// poolHost is what runs a pool's agent, and how an operator reaches it when
// the agent will not answer. Where that is follows from whether the
// configured disco-vm driver is a local one (newPoolHost), never from its name.
type poolHost interface {
	// ensurePoolHost brings up what runs the pool's agent and makes it the
	// pool's box with maker. begin is called before anything that starts or
	// replaces an agent is created, started or provisioned.
	ensurePoolHost(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker, begin func(context.Context) error) error
	// repairPoolHost restarts that host in place, keeping its disk, and
	// provisions and enrolls it again.
	repairPoolHost(ctx context.Context, pool *model.Pool, maker poolruntime.PoolMaker, begin func(context.Context) error) error
	// removePoolHost removes it and everything it kept, and anything of the
	// provider's it was given. A host that was never created is already
	// removed.
	removePoolHost(ctx context.Context, pool *model.Pool) error
	// acquireAgentClient reaches the pool agent's API.
	acquireAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error)
	openConsole(ctx context.Context, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.PTY, error)
	openLogs(ctx context.Context, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error)
}

// newPoolHost places a pool's agent where the driver's machines are (ADR
// 26-10-09-106 §1). A local driver's provider is this host's disk, so its
// machines run on this host and the pool agent runs beside the server as a
// host process, under the driver's own state directory. Any other driver's
// machines belong to a service (boxd's API), not to the process that booted
// them, so the pool agent runs in a Linux machine of its own there, booted
// from the driver's base (ADR 26-10-23-551 §1).
func newPoolHost(e *engine.Engine, cfg Config) poolHost {
	if d, ok := e.Driver.(*local.Driver); ok {
		return &hostAgent{root: d.Dir()}
	}
	return newPoolMachine(e, cfg)
}

// isLocal reports whether driver's provider is this host's disk: a disco-vm
// local driver (vz, hcs, fake), whose images and instances are files in its
// directory and whose machines are this host's processes. disco-vm reports
// this by type rather than by a capability, and it is the one thing this
// package asks of a driver's type, so nothing is written per driver name.
func isLocal(driver machine.Driver) bool {
	_, ok := driver.(*local.Driver)
	return ok
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
	driver, project, err := openDriver(cfg.Driver, providerID, providerKeys(cfg))
	if err != nil {
		return nil, err
	}
	e, err := engine.Open(driver, project)
	if err != nil {
		return nil, err
	}
	return &Runtime{engine: e, host: newPoolHost(e, cfg)}, nil
}

// providerKeys is the key a remote driver calls its service with: the
// provider's API key (Config.APIKey), which its configuration holds, never
// the server's environment. A provider made before it held one fails every
// call to the service, saying what to set. A local driver keeps no key of the
// caller's, and makes none of this.
func providerKeys(cfg Config) machine.KeySource {
	driver := strings.TrimSpace(cfg.Driver)
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return func(context.Context) (machine.Key, error) {
			return machine.Key{}, fmt.Errorf("the %s provider on the %s driver has no API key: set its apiKey", ProviderType, driver)
		}
	}
	return machine.StaticKey(machine.Key{Driver: driver, Secret: key})
}

// openDriver makes the named driver in the disco-vm project (disco-vm ADR
// 0004) of the provider instance providerID's engine, calling a remote
// driver's service with keys.
//
// The project is the driver's configuration, but which one follows from what
// the driver is: a local driver's is localProject, any other's is the
// provider's own (remoteProject). So the driver is made in localProject first,
// which construction alone cannot get wrong, and made again in the provider's
// project when it is not local. Construction touches no hypervisor or service.
func openDriver(name, providerID string, keys machine.KeySource) (machine.Driver, string, error) {
	driver, err := newDriver(name, localProject, nil)
	if err != nil || isLocal(driver) {
		return driver, localProject, err
	}
	project, err := remoteProject(providerID)
	if err != nil {
		return nil, "", err
	}
	driver, err = newDriver(name, project, keys)
	return driver, project, err
}

// newDriver constructs the named disco-vm driver for project, calling with
// keys. The name is disco-vm's, and so is the registry it is looked up in:
// this package knows no driver by name. Construction touches no hypervisor, so
// validation may call it too.
func newDriver(name, project string, keys machine.KeySource) (machine.Driver, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%s driver is required (one of %s)", ProviderType, strings.Join(machine.Names(), ", "))
	}
	cfg, err := driverConfig(name, project, stateRoot(name), keys)
	if err != nil {
		return nil, err
	}
	driver, err := machine.New(name, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s driver %q is not available in this build (have %s)", ProviderType, name, strings.Join(machine.Names(), ", "))
	}
	return driver, nil
}

// driverConfig is what the named driver is made with for project, by the
// server and by its shims alike.
//
//   - Dir is the host's state root for the driver (stateRoot), which a local
//     driver keeps its images, instances, shims and keys in; a remote driver
//     ignores it.
//   - ShimCommand is this binary's hidden shim subcommand, with the same
//     driver, project and directory, so the server ships no disco-vm binary.
//   - Keys is what the driver calls its service with: a remote driver's is
//     the provider's API key (providerKeys). A local driver's shim and the
//     server make it with none, so they act as its state root's owner.
//     disco-vm reads no key of its own accord.
func driverConfig(name, project, dir string, keys machine.KeySource) (machine.Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return machine.Config{}, err
	}
	return machine.Config{
		Project:     project,
		Dir:         dir,
		ShimCommand: shimCommand(exe, dir, name, project),
		Keys:        keys,
	}, nil
}

// localProject is the disco-vm project of everything a local driver's
// provider instances make in its state root. It is one for all of them, as
// the root is, because the engine counts the driver's running-guest cap (vz's
// two macOS guests) over the driver's instances, which are its project's, and
// that cap is the host's (stateRoot). It is not disco-vm's default project,
// which is the disco-vm CLI's, so what a person makes with the CLI is never
// discobox's.
const localProject = "discobox"

// remoteProject is a remote driver's project for the provider instance
// providerID.
//
// A remote driver's machines outlive any store that records them and are
// named for their project, which is how one store tells its own from the
// account's other contents, another server's included (disco-vm ADR 0004). So
// a remote driver's project is the provider instance's own (ADR 26-10-23-551
// §1), spelled from its ID within disco-vm's project rule: prov_x becomes
// prov-x.
func remoteProject(providerID string) (string, error) {
	project := strings.ReplaceAll(strings.ToLower(providerID), "_", "-")
	if err := machine.ValidateProject(project); err != nil {
		return "", fmt.Errorf("%s provider %q has no disco-vm project: %w", ProviderType, providerID, err)
	}
	return project, nil
}

// Close releases nothing: the engine keeps no state of its own (disco-vm ADR
// 26-10-23-417), and a running machine is its shim's, or its remote
// service's, not this value's, and outlives it. poolruntime.RuntimeProvider
// requires it.
func (r *Runtime) Close() error { return nil }

// EnsurePool brings up the pool's host, and makes it the pool's box.
func (r *Runtime) EnsurePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, maker poolruntime.PoolMaker, begin func(context.Context) error) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	return r.host.ensurePoolHost(ctx, pool, maker, begin)
}

func (r *Runtime) RepairPool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool, maker poolruntime.PoolMaker, _ string, begin func(context.Context) error) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	return r.host.repairPoolHost(ctx, pool, maker, begin)
}

func (r *Runtime) RemovePool(ctx context.Context, _ *model.Project, _ *model.SandboxProviderInstance, pool *model.Pool) error {
	if err := requirePool(pool); err != nil {
		return err
	}
	if err := r.host.removePoolHost(ctx, pool); err != nil {
		return err
	}
	pool.RuntimeState = nil
	pool.HostCredential = nil
	pool.Ready = false
	pool.Schedulable = false
	pool.Degraded = false
	return nil
}

func (r *Runtime) AcquirePoolAgentClient(ctx context.Context, pool *model.Pool) (*transport.HTTPClientLease, error) {
	if err := requirePool(pool); err != nil {
		return nil, err
	}
	return r.host.acquireAgentClient(ctx, pool)
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
	if !isLocal(r.engine.Driver) {
		return nil, fmt.Errorf("a %s pool on the %s driver boots its machines from the driver's base, with the agent placed on it, so it has no image to build: %w", ProviderType, driver, sandbox.ErrGuestImageBuildUnsupported)
	}
	return nil, fmt.Errorf("no VM image is defined for the %s driver to build: %w", driver, sandbox.ErrGuestImageBuildUnsupported)
}
