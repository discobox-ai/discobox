// Package discovm registers a provider type for each disco-vm driver this
// build links (github.com/discobox-ai/vm), by the driver's own name: pools
// whose sandboxes are machines of that driver, run by the disco-vm engine
// this server embeds (ADR 26-10-09-106 §§1–2, ADR 26-11-11-080 §1). What a
// person sees of discobox's own is spelled VM, never disco-vm (ADR
// 26-10-15-629); a driver's descriptions are shown as disco-vm writes them.
//
// It is a poolruntime.RuntimeProvider beside dockerworker.Engine, not a
// dockerworker.Driver: a pool on it has none of the engine's Docker
// mechanics. One implementation serves every type: its fields, its secret and
// its validation are the driver's, read from disco-vm's registry, and nothing
// here is written per driver beyond the import that links one.
package discovm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/adrg/xdg"
	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// driverKey is the config member holding the driver's own configuration,
// passed to disco-vm as machine.Config.DriverConfig, apart from discobox's
// pool-hosting fields so the two never share a key (ADR 26-11-11-080 §2).
const driverKey = "driver"

// stateDir names this package's state roots under the platform's data
// directory, one per driver (stateRoot). It is the retired vm type's name,
// kept so a root made under it is found (ADR 26-11-11-080 keeps 629's state
// root under discobox/vm/<driver>).
const stateDir = "vm"

// unlinked are disco-vm driver names never registered as provider types:
// fake, which only tests link, and could otherwise reach a test's provider
// catalog.
var unlinked = []string{"fake"}

// Types are the provider types this build registers: each disco-vm driver it
// links, by disco-vm's name for it.
func Types() []string {
	return slices.DeleteFunc(machine.Names(), func(name string) bool { return slices.Contains(unlinked, name) })
}

// Config is a provider instance's configuration: discobox's pool-hosting
// fields, and the driver's own under driver.
type Config struct {
	poolruntime.PoolPolicy

	// Driver is the driver's own configuration, a JSON object its
	// definition describes, given to disco-vm as it is but for its secret
	// (secretKey), which travels apart as machine.Config.Keys. Empty is the
	// driver's defaults.
	Driver json.RawMessage `json:"driver,omitempty"`
	// Base is the driver's base a pool machine, and its sandboxes' machines,
	// are made fresh from: disco-vm's machine.Config.Base, in the driver's
	// own terms, passed to it as it is. A machine is a driver base the agent
	// is placed on, never an image of discobox's (ADR 26-10-23-551 §§1, 3).
	// Empty is the driver's default. A driver that takes none refuses it.
	Base string `json:"base,omitempty"`
	// ControlPlaneURL is where a driver reached at its URLs has its pool
	// machine reach this server, when it dials it. Empty, the server opens a
	// channel to each pool machine instead, and needs no URL a machine can
	// reach (ADR 26-10-30-875 §4). A held driver refuses it.
	ControlPlaneURL string `json:"controlPlaneUrl,omitempty"`
	// DeliverProviderKey opts in to placing the driver's key itself on a
	// pool's machine when the driver cannot issue the pool a narrower key
	// that expires (ADR 26-10-23-551 §4). It reaches everything the key
	// reaches, nothing discobox does expires, renews or revokes it, and
	// whoever reads it on a pool's machine keeps it after the pool is gone.
	// Without it, such a pool is blocked, and a failed issue is retried,
	// never answered with this key.
	DeliverProviderKey bool `json:"deliverProviderKey,omitempty"`
}

// settings is a provider's configuration as disco-vm takes it: the driver's
// configuration without its secret, and the secret apart.
type settings struct {
	Config
	name string
	// driverConfig is Config.Driver without the secret.
	driverConfig json.RawMessage
	// key is the driver's secret's value, or empty.
	key string
}

// decode reads the named driver's provider configuration.
func decode(name string, data []byte) (settings, error) {
	cfg, err := poolruntime.DecodeConfig[Config](data, name)
	if err != nil {
		return settings{}, err
	}
	s := settings{Config: cfg, name: name}
	if len(bytes.TrimSpace(cfg.Driver)) == 0 || bytes.Equal(bytes.TrimSpace(cfg.Driver), []byte("null")) {
		return s, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(cfg.Driver, &fields); err != nil || fields == nil {
		return settings{}, fmt.Errorf("%s: %s is the driver's configuration, a JSON object", name, driverKey)
	}
	if secret, ok := secretOf(name); ok {
		if raw, ok := fields[secret.Key]; ok {
			delete(fields, secret.Key)
			if err := json.Unmarshal(raw, &s.key); err != nil {
				return settings{}, fmt.Errorf("%s: %s.%s is text", name, driverKey, secret.Key)
			}
			s.key = strings.TrimSpace(s.key)
		}
	}
	if len(fields) > 0 {
		if s.driverConfig, err = json.Marshal(fields); err != nil {
			return settings{}, err
		}
	}
	return s, nil
}

// secretOf is the driver's secret, its key (ADR 26-11-12-461 in disco-vm:
// at most one), if it takes one.
func secretOf(name string) (machine.SecretField, bool) {
	def, err := machine.DefinitionOf(name)
	if err != nil || len(def.Secrets) == 0 {
		return machine.SecretField{}, false
	}
	return def.Secrets[0], true
}

// Validator is the named driver's provider config validation.
func Validator(name string) sandbox.ProviderConfigValidator {
	return func(data json.RawMessage) error { return validate(name, data) }
}

func validate(name string, data json.RawMessage) error {
	s, err := decode(name, data)
	if err != nil {
		return err
	}
	// The instance's scope is not known here, and the driver's kind and
	// configuration are all there is to check. Construction touches no
	// hypervisor or service, and refuses a configuration as disco-vm does.
	driver, err := newDriver(s, localPool, nil, nil)
	if err != nil {
		return err
	}
	return validateFor(s, isLocal(driver))
}

// PoolOSValidator is whether a pool on a provider of the named driver may name
// os for its sandboxes: whether the driver boots that OS, as disco-vm reports
// it (ADR 26-11-11-963 §1), and, on a driver reached at its URLs, fences its
// machines (ADR 26-11-11-080 §1). Linux on every driver that boots it; hcs
// boots neither, and its pools are refused until the Windows work 963 §13
// defers; archil fences nothing, and every pool on it is refused, saying so,
// when it is created rather than at its machine.
func PoolOSValidator(name string) sandbox.PoolOSValidator {
	return func(data json.RawMessage, os string) error {
		s, err := decode(name, data)
		if err != nil {
			return err
		}
		driver, err := newDriver(s, localPool, nil, nil)
		if err != nil {
			return err
		}
		if !isLocal(driver) {
			if err := fences(driver); err != nil {
				return err
			}
		}
		return bootsOS(name, driver.Capabilities(), os)
	}
}

// fences refuses a driver reached at its URLs that cannot carry a pool: its
// pool machine is on a network of the pool's own, and its sandboxes'
// machines isolated on it (ADR 26-10-23-551 §4), which a driver without
// Isolation cannot do.
func fences(driver machine.Driver) error {
	if !driver.Capabilities().Isolation {
		return fmt.Errorf("the %s driver cannot isolate machines or place them on networks, which a pool's machine and its sandboxes' need: its pools are refused", driver.Name())
	}
	return nil
}

// bootsOS is whether a driver whose capabilities are caps boots os.
func bootsOS(driver string, caps machine.Capabilities, os string) error {
	if slices.Contains(caps.GuestOS, machine.OS(os)) {
		return nil
	}
	return fmt.Errorf("the %s driver does not boot %s sandboxes", driver, os)
}

// validateFor checks discobox's fields by the driver's kind (ADR 26-11-11-080
// §2), and the driver's secret: a driver that takes one needs it. A held
// driver (local: hcs) has its pools' machines on this host, so it takes no
// control plane URL, base or key delivery; a driver reached at its URLs takes
// a base where its definition says it does.
func validateFor(s settings, local bool) error {
	def, err := machine.DefinitionOf(s.name)
	if err != nil {
		return err
	}
	base, url := strings.TrimSpace(s.Base), strings.TrimSpace(s.ControlPlaneURL)
	if base != "" && (local || !def.TakesBase()) {
		return fmt.Errorf("the %s driver takes no base", s.name)
	}
	if local {
		// The server makes a held driver with no key: it acts as its state
		// root's owner.
		if url != "" || s.DeliverProviderKey || s.key != "" {
			return fmt.Errorf("the %s driver runs its pools on this host: it takes no key, control plane URL or key delivery", s.name)
		}
		return nil
	}
	if secret, ok := secretOf(s.name); ok && s.key == "" {
		return fmt.Errorf("the %s driver needs its %s (%s.%s)", s.name, secret.Label, driverKey, secret.Key)
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

// FactoryWithPoolManager makes the named driver's provider instances. hub is
// the server's carrier hub, which a pool with no controlPlaneUrl is reached
// through (ADR 26-10-30-875). endpoints is where the driver may connect, read
// as each instance is made: every connection the engine makes to the
// driver's service is checked against it as it is dialed, the instance's
// carried endpoints allowed (ADR 26-11-11-080 §§3, 8).
func FactoryWithPoolManager(name string, poolManager poolruntime.PoolManager, hub *carrierhub.Hub, endpoints func() sandbox.EndpointPolicy) sandbox.ProviderFactory {
	definition := Definition(name)
	return func(_ context.Context, instance *model.SandboxProviderInstance) (sandbox.Provider, error) {
		s, err := decode(name, instance.Config)
		if err != nil {
			return nil, err
		}
		runtime, err := newRuntime(s, instance.ID, endpoints().Dial(carriedValues(instance), nil))
		if err != nil {
			return nil, err
		}
		var streams poolruntime.StreamSink
		if !isLocal(runtime.engine.Driver) {
			if streams, err = poolruntime.ControlPlaneStreams(name, s.ControlPlaneURL, hub); err != nil {
				return nil, err
			}
		}
		return poolruntime.New(runtime, definition, poolManager, streams), nil
	}
}

// carriedValues are the values of instance's carried endpoint fields (ADR
// 26-11-11-080 §8), which its dialer allows as the operator's.
func carriedValues(instance *model.SandboxProviderInstance) []string {
	if len(instance.CarriedEndpoints) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(instance.Config, &fields) != nil {
		return nil
	}
	var values []string
	for _, key := range instance.CarriedEndpoints {
		raw, ok := sandbox.ConfigValue(fields, key)
		var value string
		if ok && json.Unmarshal(raw, &value) == nil && value != "" {
			values = append(values, value)
		}
	}
	return values
}

// Definition describes the named driver's provider type for provider
// catalogs: discobox's pool-hosting fields, by the driver's kind, and the
// driver's own fields and secret under driver, from disco-vm's registry. A
// field disco-vm adds shows up here with no change to discobox (ADR
// 26-11-11-080 §2). Every driver field is Immutable: it decides where a
// pool's machines are (§5). A Privileged one is an endpoint the key is sent
// to, and Advanced whatever disco-vm marks it (§3).
func Definition(name string) sandbox.ProviderDefinition {
	def, err := machine.DefinitionOf(name)
	if err != nil {
		panic(fmt.Sprintf("discovm: driver %q is not linked: %v", name, err))
	}
	local := false
	if driver, err := newDriver(settings{name: name}, localPool, nil, nil); err == nil {
		local = isLocal(driver)
	}
	var fields []sandbox.ProviderConfigField
	// A held driver is made with no key, so its catalog lists none.
	for _, secret := range def.Secrets {
		if local {
			break
		}
		fields = append(fields, sandbox.ProviderConfigField{
			Key:         driverKey + "." + secret.Key,
			Label:       secret.Label,
			Type:        sandbox.ProviderConfigFieldSecret,
			Description: secret.Description + " Give discobox an account of its own: every pool of this provider can reach what the key reaches.",
			Placeholder: secret.Placeholder,
			Required:    true,
		})
	}
	for _, field := range def.Current() {
		fields = append(fields, sandbox.ProviderConfigField{
			Key:         driverKey + "." + field.Key,
			Label:       field.Label,
			Type:        field.Type,
			Description: field.Description,
			Placeholder: field.Placeholder,
			Required:    field.Required,
			Advanced:    field.Advanced || field.Privileged,
			Immutable:   true,
			Default:     field.Default,
			Privileged:  field.Privileged,
		})
	}
	if !local {
		fields = append(fields,
			sandbox.ProviderConfigField{Key: "controlPlaneUrl", Label: "Control Plane URL", Type: "string", Placeholder: "https://discobox.example.com", Description: "Where this provider's pool machines reach this server. Leave it empty and this server opens a channel to each pool machine instead, so it needs no public URL."},
			sandbox.ProviderConfigField{Key: "deliverProviderKey", Label: "Deliver Provider Key", Type: "boolean", Advanced: true, Description: "When the driver cannot issue a pool a narrower key that expires, place this provider's own key on the pool's machine instead. It reaches everything the key reaches, discobox never expires, renews or revokes it, and whoever reads it there keeps it: rotate it when a pool machine is lost or suspected. Without it such a pool is blocked."},
		)
		if def.TakesBase() {
			fields = append(fields, sandbox.ProviderConfigField{Key: "base", Label: def.Base.Label, Type: "string", Placeholder: def.Base.Placeholder, Default: def.Base.Default, Advanced: true, Description: def.Base.Description + " A pool's machine and its sandboxes' machines are made from it, and the pool's keys run on it: name one only your account controls."})
		}
	}
	return sandbox.ProviderDefinition{
		Name:         name,
		Icon:         "server",
		Description:  def.Description,
		ConfigFields: append(fields, poolruntime.PoolPolicyConfigFields()...),
	}
}

// Register registers a provider type for each driver of Types with manager,
// whose endpoint policy (ADR 26-11-11-080 §3) its instances dial under, as
// the providers service checks a write under it. A driver whose name is
// already a provider type panics, as disco-vm's own duplicate registration
// does: a disco-vm driver added under a taken name fails the build's tests,
// never a user's server (ADR 26-11-11-080 §1).
func Register(manager *sandbox.ProviderManager, poolManager poolruntime.PoolManager, hub *carrierhub.Hub) {
	for _, name := range Types() {
		if _, taken := manager.GetProviderDefinition(name); taken {
			panic(fmt.Sprintf("discovm: the disco-vm driver %q is named as a provider type that is already registered: do not link it", name))
		}
		manager.RegisterProviderDefinition(name, Definition(name))
		manager.RegisterFactory(name, FactoryWithPoolManager(name, poolManager, hub, manager.EndpointPolicy))
		manager.RegisterProviderConfigValidator(name, Validator(name))
		manager.RegisterPoolOSValidator(name, PoolOSValidator(name))
		for _, variable := range environment(name) {
			if _, set := os.LookupEnv(variable); set {
				slog.Warn("the server's environment sets a VM driver's variable, which it does not read: set the provider's field instead", "driver", name, "variable", variable)
			}
		}
	}
}

// environment is every variable the named driver's command line reads a
// field, its base or its secret from. The server reads none of them (ADR
// 26-11-11-080 §2), but for the upgrade's one carry (§8), so one set in its
// environment is a setting that does nothing, and is named at start.
func environment(name string) []string {
	def, err := machine.DefinitionOf(name)
	if err != nil {
		return nil
	}
	var vars []string
	for _, field := range def.ConfigFields {
		if field.Env != "" {
			vars = append(vars, field.Env)
		}
	}
	for _, secret := range def.Secrets {
		vars = append(vars, secret.Env...)
	}
	return vars
}

// errNoKey is a driver's call made with no key, for a provider made before
// it held one.
var errNoKey = errors.New("no key")

// stateRoot is this host's disco-vm state root for a driver: its image store
// and its instances, shared by every provider instance on that driver. It is
// under the platform's data directory: on macOS that is ~/Library/Application
// Support.
//
// A root per driver, because the limits the engine enforces are a driver's,
// and a root is one tag namespace that two drivers' images would share.
func stateRoot(driver string) string {
	if home := strings.TrimSpace(xdg.DataHome); home != "" {
		return filepath.Join(home, "discobox", stateDir, driver)
	}
	return filepath.Join(os.TempDir(), "discobox", stateDir, driver)
}
