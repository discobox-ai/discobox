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
// from whether the driver's provider is this host's disk.
package discovm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

const ProviderType = "vm"

// Config is the persisted provider instance configuration.
type Config struct {
	poolruntime.PoolPolicy

	// Driver is the disco-vm driver the provider's pools run on, by
	// disco-vm's name for it. It is passed to disco-vm as it is: what a pool
	// on it looks like follows from whether it is a local driver.
	//
	// There is no state directory to configure. Every provider instance of a
	// driver on a host shares that driver's disco-vm state root (stateRoot),
	// because the limits the engine enforces — vz's two running macOS guests —
	// are the host's (ADR 26-10-09-106 §4).
	Driver string `json:"driver,omitempty"`
	// Base is the ID of the driver's base a remote driver's pool machine, and
	// its sandboxes' machines, boot from (machine.Driver.Bases): a machine is a
	// driver base the agent is placed on, never an image of discobox's (ADR
	// 26-10-23-551 §§1, 3). It is an ID, never a name, because a name can be
	// given to another base after an operator chose it (a boxd snapshot's).
	// Empty is the first base the driver lists: on boxd its default image when
	// the account has one, and otherwise its oldest snapshot. A local driver's
	// pool agent runs on this host, and boots no base.
	Base string `json:"base,omitempty"`
	// APIKey is a remote driver's key for its service (boxd's), which the
	// engine calls with. A local driver has none.
	APIKey string `json:"apiKey,omitempty"`
	// ControlPlaneURL is where a remote driver's pool machine reaches this
	// server, when it dials it. Empty, the server opens a channel to each
	// pool machine instead, and needs no URL a machine can reach (ADR
	// 26-10-30-875 §4).
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	// DeliverProviderKey opts in to placing APIKey itself on a pool's machine
	// when the driver cannot issue the pool a narrower key that expires (ADR
	// 26-10-23-551 §4). It reaches everything APIKey reaches, nothing discobox
	// does expires, renews or revokes it, and whoever reads it on a pool's
	// machine keeps it after the pool is gone. Without it, such a pool is
	// blocked, and a failed issue is retried, never answered with this key.
	DeliverProviderKey bool `json:"deliverProviderKey,omitempty"`
}

func Decode(data json.RawMessage) (Config, error) {
	return poolruntime.DecodeConfig[Config](data, ProviderType)
}

func Validate(data json.RawMessage) error {
	cfg, err := Decode(data)
	if err != nil {
		return err
	}
	// The provider instance's project is not known here, and the driver's
	// kind is all there is to check the rest against.
	driver, err := newDriver(cfg.Driver, localProject, nil)
	if err != nil {
		return err
	}
	return validateFor(cfg, isLocal(driver))
}

// validateFor checks what a driver of the kind needs: a remote driver its key,
// and any URL its pool machines reach this server at, a local one neither.
func validateFor(cfg Config, local bool) error {
	key, url := strings.TrimSpace(cfg.APIKey), strings.TrimSpace(cfg.ControlPlaneURL)
	if local {
		if key != "" || url != "" || cfg.DeliverProviderKey {
			return fmt.Errorf("the %s driver runs its pools on this host: it takes no API key, control plane URL or key delivery", cfg.Driver)
		}
		return nil
	}
	if key == "" {
		return fmt.Errorf("the %s driver needs its API key (apiKey)", cfg.Driver)
	}
	// With no URL the server opens a channel to each pool machine instead
	// (ADR 26-10-30-875 §4).
	if url == "" {
		return nil
	}
	endpoint, err := wire.Parse(url)
	if err != nil {
		return fmt.Errorf("controlPlaneUrl: %w", err)
	}
	if !endpoint.IsIP() {
		return fmt.Errorf("controlPlaneUrl %q: a pool machine reaches this server over http or https", url)
	}
	return nil
}

// FactoryWithPoolManager makes the provider's instances. hub is the server's
// carrier hub, which a remote driver's pool with no controlPlaneUrl is reached
// through (ADR 26-10-30-875); a local driver's pools reach this server on its
// own host and never use it.
func FactoryWithPoolManager(poolManager poolruntime.PoolManager, hub *carrierhub.Hub) sandbox.ProviderFactory {
	return func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		cfg, err := Decode(instance.Config)
		if err != nil {
			return nil, err
		}
		runtime, err := newRuntime(cfg, instance.ID)
		if err != nil {
			return nil, err
		}
		var streams poolruntime.StreamSink
		if !isLocal(runtime.engine.Driver) {
			if streams, err = poolruntime.ControlPlaneStreams(ProviderType, cfg.ControlPlaneURL, hub); err != nil {
				return nil, err
			}
		}
		return poolruntime.New(runtime, Definition(), poolManager, streams), nil
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
			{Key: "driver", Label: "Driver", Type: "string", Required: true, Immutable: true, Description: "The VM driver to run machines on: vz on macOS, or boxd from any host."},
			{Key: "apiKey", Label: "API Key", Type: "password", Description: "On a remote driver such as boxd, the key the server calls the driver's service with. Give discobox an org or account of its own: every pool of this provider can reach what the key reaches."},
			{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Placeholder: "https://discobox.example.com", Description: "On a remote driver such as boxd, where its pool machines reach this server. Leave it empty and this server opens a channel to each pool machine instead, so it needs no public URL."},
			{Key: "deliverProviderKey", Label: "Deliver Provider Key", Type: "boolean", Advanced: true, Description: "When the driver cannot issue a pool a narrower key that expires (boxd issues none from an API key), place this provider's own API key on the pool's machine instead. It reaches everything the key reaches, discobox never expires, renews or revokes it, and whoever reads it there keeps it: rotate it when a pool machine is lost or suspected. Without it such a pool is blocked."},
			{Key: "base", Label: "Base", Type: "string", Description: "On a remote driver such as boxd, the ID of the driver's base a pool's machine and its sandboxes' machines boot from. Empty is the first base the driver lists: boxd's default image, or its oldest snapshot when the account has no default image, so name one there.", Advanced: true},
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
