package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ProviderConfigFieldSecret is the type of a field that holds a credential.
// Its value is sealed in the provider instance's row, apart from the rest of
// the config, and no read of the instance returns it (ADR 26-10-31-694).
const ProviderConfigFieldSecret = "secret"

// ProviderConfigField describes one provider instance configuration field.
//
// Key is a path into the config object (ConfigValue): "driver.addr" is addr
// in the object a disco-vm driver is given (ADR 26-11-11-080 §2).
type ProviderConfigField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
	// Immutable fields cannot change while the provider instance has pools.
	// It is for a field that decides what made a pool's host, so that a new
	// value would leave the hosts the old one made with nothing to remove
	// them: a vm pool's driver, whose machines only that driver can
	// reach.
	Immutable bool `json:"immutable,omitempty"`
	// Default is the value the provider takes when the field is unset, shown
	// for a person; the provider applies it, never the catalog.
	Default string `json:"default,omitempty"`
	// Privileged is an endpoint the provider's secret fields are sent to. A
	// write that sets or changes it carries every secret field in the same
	// body, so a sealed secret is only ever sent where someone who supplied
	// it pointed it (ADR 26-11-11-080 §4).
	Privileged bool `json:"privileged,omitempty"`
}

// SizeField names one size field a provider may act on, spelled as the API
// spells it so an error can name exactly what the caller set.
type SizeField string

const (
	// SizeCPU and SizeMemory are a box's size, and a pool's as its boxes'
	// default (ADR 26-11-23-445 §§1, 3).
	SizeCPU    SizeField = "cpuVcpus"
	SizeMemory SizeField = "memoryBytes"
	// SizeStorage is a pool's alone.
	SizeStorage SizeField = "storageBytes"
)

// ProviderDefinition describes a registered provider driver.
type ProviderDefinition struct {
	Name         string                `json:"name,omitempty"`
	Icon         string                `json:"icon,omitempty"`
	Description  string                `json:"description,omitempty"`
	ConfigFields []ProviderConfigField `json:"configFields,omitempty"`

	// SizeFields lists the size fields this provider acts on: CPU and memory
	// for a box and as its pool's default (no provider acts on one and not
	// the other), and the pool-only fields it declares. A box or a pool on
	// the provider may set only these; the sandbox and pool services refuse
	// any other non-zero field, so a size is never accepted and then
	// silently ignored (ADR 26-11-23-445 §6). Empty means the provider acts
	// on none.
	//
	// It is declared by the provider, next to the code that honors it, rather
	// than decided by the services: whether a size means anything depends on
	// the backend, and a list kept anywhere else goes stale the first time a
	// backend changes.
	SizeFields []SizeField `json:"sizeFields,omitempty"`
	// BoxesInPoolVM reports that the provider runs every box of a pool in
	// one VM sized by the pool's CPU and memory, so no box can be larger
	// than a size the pool sets (ADR 26-11-23-445 §7).
	BoxesInPoolVM bool `json:"boxesInPoolVm,omitempty"`
}

// ProviderStatus describes runtime provider availability.
type ProviderStatus struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	Details   any    `json:"details,omitempty"`
}

// SecretFields lists the keys of the fields typed secret: paths, sealed and
// joined at their place in the config.
func (d ProviderDefinition) SecretFields() []string {
	var keys []string
	for _, field := range d.ConfigFields {
		if field.Type == ProviderConfigFieldSecret {
			keys = append(keys, field.Key)
		}
	}
	return keys
}

// JoinProviderConfig is config with sealed's fields set in it: a provider
// instance's whole config, as its provider decodes it, as its validator checks
// it, and as a create of a copy of it is given (ADR 26-10-31-694 §§1, 5).
func JoinProviderConfig(config json.RawMessage, sealed map[string]json.RawMessage) (json.RawMessage, error) {
	if len(sealed) == 0 {
		return config, nil
	}
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(config)) > 0 {
		if err := json.Unmarshal(config, &fields); err != nil {
			return nil, fmt.Errorf("decode provider config: %w", err)
		}
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	for key, value := range sealed {
		if err := SetConfigValue(fields, key, value); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

// ActsOnSize reports whether the provider acts on field.
func (d ProviderDefinition) ActsOnSize(field SizeField) bool {
	return slices.Contains(d.SizeFields, field)
}

// SizeValue is one size field as a request sets it.
type SizeValue struct {
	Field SizeField
	Value float64
}

// CheckSize refuses values of a box's or a pool's size (of names which) that
// the provider instance would not act on, so that a size is never accepted
// and then silently ignored (ADR 26-11-23-445 §6). The error is the caller's
// to report as a bad request.
//
// Zero is always accepted: it is "unset", and clearing a value no provider
// ever acted on has to stay possible. A negative value is refused on every
// provider, because no backend reads one and it would do nothing.
func (d ProviderDefinition) CheckSize(of, instanceName, providerType string, values []SizeValue) error {
	var refused []string
	for _, v := range values {
		if v.Value < 0 {
			return fmt.Errorf("%s %s must not be negative", of, v.Field)
		}
		if v.Value > 0 && !d.ActsOnSize(v.Field) {
			refused = append(refused, string(v.Field))
		}
	}
	if len(refused) == 0 {
		return nil
	}
	accepted := "takes no size settings"
	if len(d.SizeFields) > 0 {
		names := make([]string, len(d.SizeFields))
		for i, field := range d.SizeFields {
			names[i] = string(field)
		}
		accepted = "acts only on " + strings.Join(names, ", ")
	}
	return fmt.Errorf("%s %s would have no effect on provider instance %q: the %s provider %s; leave it unset (0)",
		of, strings.Join(refused, ", "), instanceName, providerType, accepted)
}
