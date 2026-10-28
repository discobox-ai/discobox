package poolagent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// openMachineEngine is a machine pool's own disco-vm engine (ADR 26-10-23-551
// §3): its driver, in the pool's project, calling with the provisioning key
// its maker placed, read again on every call.
func openMachineEngine(root layout.Root, bootstrap Bootstrap, keyFile string) (*engine.Engine, error) {
	driver, err := machine.New(bootstrap.Driver, machine.Config{
		Project: bootstrap.MachineProject,
		Dir:     root.PoolMachines(bootstrap.ProjectID, bootstrap.PoolID),
		Keys:    provisioningKeys(keyFile, bootstrap.Driver),
	})
	if err != nil {
		return nil, fmt.Errorf("the %s driver of this machine pool: %w", bootstrap.Driver, err)
	}
	return engine.Open(driver, bootstrap.MachineProject)
}

// provisioningKeys is the provisioning key at path, read on every call: a key
// the provider renews is a new file, used from the next call (ADR
// 26-10-23-551 §4). Nothing of the file is put in an error, and the key
// prints without its secret.
func provisioningKeys(path, driver string) machine.KeySource {
	return func(context.Context) (machine.Key, error) {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return machine.Key{}, fmt.Errorf("this machine pool has no provisioning key at %s, which its provider places", path)
		}
		if err != nil {
			return machine.Key{}, fmt.Errorf("read the provisioning key: %w", err)
		}
		var key machine.Key
		if json.Unmarshal(data, &key) != nil {
			return machine.Key{}, fmt.Errorf("the provisioning key at %s is not a disco-vm key", path)
		}
		switch {
		case key.Driver != driver:
			return machine.Key{}, fmt.Errorf("the provisioning key at %s is for the %q driver, and this pool's is %q", path, key.Driver, driver)
		case key.Secret == "":
			return machine.Key{}, fmt.Errorf("the provisioning key at %s holds no secret", path)
		}
		return key, nil
	}
}

// newMachineSandboxRuntime is a machine pool's runtime: sandboxes as machines
// of its own engine, each made isolated on the pool's network wherever the
// driver can isolate one, which a remote driver must (ADR 26-10-23-551 §4).
func newMachineSandboxRuntime(root layout.Root, bootstrap Bootstrap, e *engine.Engine, identityKey ed25519.PrivateKey, assets *assetcache.Cache) (*sandboxruntime.MachineSandboxRuntime, error) {
	idleTimeout, err := sandboxruntime.ConfiguredSandboxIdleTimeout()
	if err != nil {
		return nil, err
	}
	tunnelURL, err := proxyagent.ConfiguredTunnelURL()
	if err != nil {
		return nil, err
	}

	return sandboxruntime.NewMachineSandboxRuntime(sandboxruntime.MachineSandboxRuntimeConfig{
		ProjectID:             bootstrap.ProjectID,
		PoolID:                bootstrap.PoolID,
		ControlPlanePublicKey: bootstrap.ControlPlaneKey,
		Root:                  root,
		Engine:                e,
		Base:                  bootstrap.MachineBase,
		Isolate:               true,
		Network:               bootstrap.MachineNetwork,
		SandboxIdleTimeout:    idleTimeout,
		PoolEndpoints:         proxyagent.PoolEndpoints(tunnelURL),
		IdentityKey:           identityKey,
		Platform:              hostedPlatform(),
		AgentLayers:           assets,
	})
}

// machinePoolStatus is what a machine pool says of itself: it runs images
// of its driver's kind (ADR 26-10-09-106 §4), and its capacity is the
// driver's, which runs its sandboxes, not the pool's own machine's.
//
// It schedules nothing yet. Isolating a sandbox's machine strips what its
// provider gives it, but bounds none of its egress: until a machine's only
// way out is its pool's proxy (#129), a sandbox could reach the network
// around the proxy's policy and audit, which ADR 26-10-23-551 §5 makes a
// precondition of the first one.
func machinePoolStatus(driver string, caps machine.Capabilities) poolStatus {
	return poolStatus{
		schedulable: false,
		imageKind:   platform.VM(driver),
		runtime: map[string]any{
			"kind":       string(RuntimeMachine),
			"driver":     driver,
			"status":     "unavailable",
			"message":    "a machine sandbox's egress is not held to its pool's proxy yet, so this pool schedules nothing",
			"maxRunning": caps.MaxRunning,
			"isolation":  caps.Isolation,
		},
	}
}
