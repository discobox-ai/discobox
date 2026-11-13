package discovm

import (
	"context"
	"encoding/json"
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
	openConsole(ctx context.Context, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.Stream, error)
	openLogs(ctx context.Context, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error)
}

// newPoolHost places a pool's agent where the driver's machines are (ADR
// 26-10-09-106 §1). A local driver's provider is this host's disk, so its
// machines run on this host and the pool agent runs beside the server as a
// host process, under the driver's own state directory. Any other driver's
// machines belong to a service (boxd's API), not to the process that booted
// them, so the pool agent runs in a Linux machine of its own there, booted
// from the driver's base (ADR 26-10-23-551 §1).
func newPoolHost(e *engine.Engine, s settings) poolHost {
	if d, ok := e.Driver.(*local.Driver); ok {
		return &hostAgent{root: d.Dir()}
	}
	return newPoolMachine(e, s)
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

// newRuntime opens the engine for the provider instance providerID, every
// connection its driver makes to its service through dial.
func newRuntime(s settings, providerID string, dial machine.DialFunc) (*Runtime, error) {
	driver, scope, err := openDriver(s, providerID, providerKeys(s), dial)
	if err != nil {
		return nil, err
	}
	e, err := engine.Open(driver, scope)
	if err != nil {
		return nil, err
	}
	return &Runtime{engine: e, host: newPoolHost(e, s)}, nil
}

// providerKeys is the key a driver reached at its URLs calls its service
// with: the provider's secret, which its row seals and its resolve joins
// (ADR 26-11-11-080 §2), passed apart from the driver's configuration and
// never read from the server's environment. A provider without one fails
// every call to the service, saying what to set. A held driver keeps no key
// of the caller's, and makes none of this.
func providerKeys(s settings) machine.KeySource {
	if s.key == "" {
		return func(context.Context) (machine.Key, error) {
			field := "its key"
			if secret, ok := secretOf(s.name); ok {
				field = driverKey + "." + secret.Key
			}
			return machine.Key{}, fmt.Errorf("the %s provider has %w: set %s", s.name, errNoKey, field)
		}
	}
	return machine.StaticKey(machine.Key{Driver: s.name, Secret: s.key})
}

// openDriver makes the configured driver in the disco-vm scope (disco-vm ADR
// 0004) of the provider instance providerID's engine, calling its service
// with keys through dial, and making a fresh VM from its base (disco-vm ADR
// 26-11-03-606).
//
// The scope is the driver's configuration, but which one follows from what
// the driver is: a held (local) driver's is localPool, any other's is the
// provider's own (remotePool). So the driver is made in localPool first,
// which construction alone cannot get wrong, and made again in the provider's
// scope when it is not local. Construction touches no hypervisor or service.
func openDriver(s settings, providerID string, keys machine.KeySource, dial machine.DialFunc) (machine.Driver, string, error) {
	driver, err := newDriver(s, localPool, nil, nil)
	if err != nil || isLocal(driver) {
		return driver, localPool, err
	}
	scope, err := remotePool(providerID)
	if err != nil {
		return nil, "", err
	}
	driver, err = newDriver(s, scope, keys, dial)
	return driver, scope, err
}

// newDriver constructs the configured disco-vm driver for scope, calling with
// keys through dial. The name is disco-vm's, and so is the registry it is
// looked up in. Construction touches no hypervisor, so validation may call
// it too, and refuses a configuration as disco-vm does: its message names
// the field and never its value.
func newDriver(s settings, scope string, keys machine.KeySource, dial machine.DialFunc) (machine.Driver, error) {
	cfg, err := driverConfig(s.name, scope, stateRoot(s.name), keys, s.driverConfig)
	if err != nil {
		return nil, err
	}
	cfg.Base = strings.TrimSpace(s.Base)
	cfg.Dial = dial
	// disco-vm's error names the driver, the field and never its value.
	return machine.New(s.name, cfg)
}

// driverConfig is what the named driver is made with for scope, by the
// server and by its shims alike.
//
//   - Dir is the host's state root for the driver (stateRoot), which a held
//     driver keeps its images, instances, shims and keys in; one reached at
//     its URLs ignores it.
//   - ShimCommand is this binary's hidden shim subcommand, with the same
//     driver, scope and directory, so the server ships no disco-vm binary.
//   - Keys is what the driver calls its service with: the provider's secret
//     (providerKeys). A held driver's shim and the server make it with none,
//     so they act as its state root's owner. disco-vm reads no key of its
//     own accord.
//   - DriverConfig is the driver's own configuration from the provider's
//     row, without its secret. No driver reads the environment for it, and
//     neither does the server (ADR 26-11-11-080 §2).
func driverConfig(name, scope, dir string, keys machine.KeySource, config json.RawMessage) (machine.Config, error) {
	exe, err := os.Executable()
	if err != nil {
		return machine.Config{}, err
	}
	return machine.Config{
		Pool:         scope,
		Dir:          dir,
		ShimCommand:  shimCommand(exe, dir, name, scope),
		Keys:         keys,
		DriverConfig: config,
	}, nil
}

// localPool is the disco-vm scope of everything a local driver's
// provider instances make in its state root. It is one for all of them, as
// the root is, because the engine counts the driver's running-guest cap (vz's
// two macOS guests) over the driver's instances, which are its scope's, and
// that cap is the host's (stateRoot). It is not disco-vm's default scope,
// which is the disco-vm CLI's, so what a person makes with the CLI is never
// discobox's.
const localPool = "discobox"

// remotePool is a remote driver's scope for the provider instance
// providerID.
//
// A remote driver's machines outlive any store that records them and are
// named for their scope, which is how one store tells its own from the
// account's other contents, another server's included (disco-vm ADR 0004). So
// a remote driver's scope is the provider instance's own (ADR 26-10-23-551
// §1), spelled from its ID within disco-vm's scope rule: prov_x becomes
// prov-x.
func remotePool(providerID string) (string, error) {
	scope := strings.ReplaceAll(strings.ToLower(providerID), "_", "-")
	if err := machine.ValidatePool(scope); err != nil {
		return "", fmt.Errorf("provider %q has no disco-vm scope: %w", providerID, err)
	}
	return scope, nil
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

func (r *Runtime) OpenConsole(ctx context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.Stream, error) {
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
		return nil, fmt.Errorf("a pool on the %s driver boots its machines from the driver's base, with the agent placed on it, so it has no image to build: %w", driver, sandbox.ErrGuestImageBuildUnsupported)
	}
	return nil, fmt.Errorf("no VM image is defined for the %s driver to build: %w", driver, sandbox.ErrGuestImageBuildUnsupported)
}
