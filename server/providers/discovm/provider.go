// Package discovm registers the "discovm" provider type: pools whose sandboxes
// are disco-vm machines (github.com/discobox-ai/vm), one per sandbox, run by the
// disco-vm engine this server embeds (ADR 26-10-09-106 §§1–2).
//
// It is a poolruntime.RuntimeProvider beside dockerworker.Engine, not a
// dockerworker.Driver: a discovm pool runs no Docker at all. Like the engine it
// has drivers, and they are disco-vm's — vz on macOS and boxd from any host —
// because where a pool's agent runs is the driver's: beside the server on a
// local hypervisor, in a machine of its own on a remote one.
package discovm

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

const ProviderType = "discovm"

// Config is the persisted provider instance configuration.
type Config struct {
	poolruntime.PoolPolicy

	// Driver is the disco-vm driver the provider's pools run on.
	//
	// There is no state directory to configure. Every provider instance on a
	// host shares one disco-vm state root (stateRoot), because the limits the
	// engine enforces — vz's two running macOS guests — are the host's, and
	// the engine counts them over the instances in its root (ADR 26-10-09-106
	// §4). A root per provider instance would give each its own two.
	Driver string `json:"driver,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	_, err = lookupDriver(cfg.Driver)
	return err
}

// lookupDriver finds a driver this build has. A driver is in a build only
// where its hypervisor can run, so vz is absent from every build but macOS.
func lookupDriver(name string) (driverDefinition, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return driverDefinition{}, fmt.Errorf("%s driver is required (one of %s)", ProviderType, strings.Join(driverNames(), ", "))
	}
	definition, ok := drivers[name]
	if !ok {
		return driverDefinition{}, fmt.Errorf("%s driver %q is not available in this build (have %s)", ProviderType, name, strings.Join(driverNames(), ", "))
	}
	return definition, nil
}

func driverNames() []string {
	return slices.Sorted(maps.Keys(drivers))
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager) sandbox.ProviderFactory {
	return func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		cfg, err := Decode(instance.Config)
		if err != nil {
			return nil, err
		}
		runtime, err := newRuntime(cfg)
		if err != nil {
			return nil, err
		}
		return poolruntime.New(runtime, Definition(), poolManager), nil
	}
}

// Definition describes the disco-vm provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        "disco-vm",
		Icon:        "server",
		Description: "Runs each sandbox as a disco-vm machine: macOS guests on this Mac (vz), or Linux microVMs on boxd.",
		ConfigFields: append([]sandbox.ProviderConfigField{
			// Immutable: a pool's host is the driver's machine or process, which
			// only that driver can reach to remove.
			{Key: "driver", Label: "Driver", Type: "string", Required: true, Immutable: true, Description: "The disco-vm driver: vz (macOS only) or boxd. boxd authenticates with BOXD_API_KEY in the server's environment."},
		}, poolruntime.PoolPolicyConfigFields()...),
	}
}

// stateRoot is this host's disco-vm state root: its image store and its
// instances, for every driver and every provider instance. It is under the
// platform's data directory, as vz's pool disks are: on macOS that is
// ~/Library/Application Support.
func stateRoot() string {
	if home := strings.TrimSpace(xdg.DataHome); home != "" {
		return filepath.Join(home, "discobox", ProviderType)
	}
	return filepath.Join(os.TempDir(), "discobox", ProviderType)
}
