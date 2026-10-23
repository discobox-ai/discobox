// Package discovm registers the "vm" provider type: pools whose sandboxes are
// disco-vm machines (github.com/discobox-ai/vm), one per sandbox, run by the
// disco-vm engine this server embeds (ADR 26-10-09-106 §§1–2). What a person
// sees is spelled vm, never disco-vm (ADR 26-10-15-629).
//
// It is a poolruntime.RuntimeProvider beside dockerworker.Engine, not a
// dockerworker.Driver: a vm pool has none of the engine's Docker mechanics,
// whatever its driver runs a machine as. Its driver is
// disco-vm's, named in the provider's configuration and passed to disco-vm as
// it is. Nothing here is written per driver: where a pool's agent runs follows
// from what the driver reports it can do.
package discovm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

const ProviderType = "vm"

// Config is the persisted provider instance configuration.
type Config struct {
	poolruntime.PoolPolicy

	// Driver is the disco-vm driver the provider's pools run on, by
	// disco-vm's name for it. It is passed to disco-vm as it is: what a pool
	// on it looks like follows from what the driver reports it can do.
	//
	// There is no state directory to configure. Every provider instance of a
	// driver on a host shares that driver's disco-vm state root (stateRoot),
	// because the limits the engine enforces — vz's two running macOS guests —
	// are the host's (ADR 26-10-09-106 §4).
	Driver string `json:"driver,omitempty"`
	// Base is the ID of the driver's base a remote driver's pool machine
	// boots from (machine.Driver.Bases): a pool machine is a driver base the
	// pool agent is placed on, never an image of discobox's (ADR 26-10-23-551
	// §1). It is an ID, never a name, because a name can be given to another
	// base after an operator chose it (a boxd snapshot's). Empty is the first
	// base the driver lists: on boxd its default image when the account has
	// one, and otherwise its oldest snapshot. A local driver's pool agent runs
	// on this host, and boots no base.
	Base string `json:"base,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	_, err = newDriver(cfg.Driver)
	return err
}

func FactoryWithPoolManager(poolManager poolruntime.PoolManager) sandbox.ProviderFactory {
	return func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		cfg, err := Decode(instance.Config)
		if err != nil {
			return nil, err
		}
		runtime, err := newRuntime(cfg, instance.ID)
		if err != nil {
			return nil, err
		}
		return poolruntime.New(runtime, Definition(), poolManager), nil
	}
}

// Definition describes the vm provider for provider catalogs.
func Definition() sandbox.ProviderDefinition {
	return sandbox.ProviderDefinition{
		Name:        ProviderType,
		Icon:        "server",
		Description: "Runs each sandbox as a virtual machine, on whichever VM driver it is configured with.",
		ConfigFields: append([]sandbox.ProviderConfigField{
			// Immutable: a pool's host was made through the driver, and only
			// that driver can reach it to remove it.
			{Key: "driver", Label: "Driver", Type: "string", Required: true, Immutable: true, Description: "The VM driver to run machines on: vz on macOS, or boxd from any host (it authenticates with BOXD_API_KEY in the server's environment)."},
			{Key: "base", Label: "Base", Type: "string", Description: "On a remote driver such as boxd, the ID of the driver's base a pool's machine boots from. Empty is the first base the driver lists: boxd's default image, or its oldest snapshot when the account has no default image, so name one there.", Advanced: true},
		}, poolruntime.PoolPolicyConfigFields()...),
	}
}

// stateRoot is this host's disco-vm state root for a driver: its image store
// and its instances, shared by every provider instance on that driver. It is
// under the platform's data directory, as vz's pool disks are: on macOS that is
// ~/Library/Application Support.
//
// A root per driver, because the limits the engine enforces are a driver's,
// and a root is one tag namespace that two drivers' images would share.
func stateRoot(driver string) string {
	if home := strings.TrimSpace(xdg.DataHome); home != "" {
		return filepath.Join(home, "discobox", ProviderType, driver)
	}
	return filepath.Join(os.TempDir(), "discobox", ProviderType, driver)
}
