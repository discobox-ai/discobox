// Package discovm registers the "discovm" provider type: pools whose sandboxes
// are disco-vm machines (github.com/discobox-ai/vm), one per sandbox, run by the
// disco-vm engine this server embeds (ADR 26-10-09-106 §§1–2).
//
// It is a poolruntime.RuntimeProvider beside dockerworker.Engine, not a
// dockerworker.Driver: a discovm pool runs no Docker at all. Its driver is
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

const ProviderType = "discovm"

// Config is the persisted provider instance configuration.
type Config struct {
	poolruntime.PoolPolicy

	// Driver is the disco-vm driver the provider's pools run on, by
	// disco-vm's name for it. It is passed to disco-vm as it is: what a pool
	// on it looks like follows from what the driver reports it can do.
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
	_, err = newDriver(cfg.Driver)
	return err
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
		Description: "Runs each sandbox as a disco-vm machine, on whichever disco-vm driver it is configured with.",
		ConfigFields: append([]sandbox.ProviderConfigField{
			// Immutable: a pool's host was made through the driver, and only
			// that driver can reach it to remove it.
			{Key: "driver", Label: "Driver", Type: "string", Required: true, Immutable: true, Description: "The disco-vm driver to run machines on, as disco-vm names it: for example vz on macOS, or boxd from any host (it authenticates with BOXD_API_KEY in the server's environment)."},
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
