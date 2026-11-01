package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
)

// ProviderConfigFieldSecret is the type of a field that holds a credential.
// Its value is sealed in the provider instance's row, apart from the rest of
// the config, and no read of the instance returns it (ADR 26-10-31-694).
const ProviderConfigFieldSecret = "secret"

// ProviderConfigField describes one provider instance configuration field.
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
}

// PoolSizeField names one field of a pool's size, spelled as the API
// spells it so an error can name exactly what the caller set.
type PoolSizeField string

const (
	PoolSizeCPU     PoolSizeField = "cpuVcpus"
	PoolSizeMemory  PoolSizeField = "memoryBytes"
	PoolSizeStorage PoolSizeField = "storageBytes"
)

// ProviderDefinition describes a registered provider driver.
type ProviderDefinition struct {
	Name         string                `json:"name,omitempty"`
	Icon         string                `json:"icon,omitempty"`
	Description  string                `json:"description,omitempty"`
	ConfigFields []ProviderConfigField `json:"configFields,omitempty"`

	// PoolSizeFields lists the pool size fields this provider acts on. A
	// pool on the provider may set only these; the pool service refuses any
	// other non-zero field at create and update, so a pool setting never
	// silently does nothing. Empty means the provider acts on none.
	//
	// It is declared by the provider, next to the code that honors it, rather
	// than decided by the pool service: whether a size means anything depends
	// on the backend (a per-pool VM can be sized; a shared Docker host cannot),
	// and a list kept anywhere else goes stale the first time a backend changes.
	PoolSizeFields []PoolSizeField `json:"poolSizeFields,omitempty"`
}

// ProviderStatus describes runtime provider availability.
type ProviderStatus struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	Details   any    `json:"details,omitempty"`
}

// SecretFields lists the keys of the fields typed secret.
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
	maps.Copy(fields, sealed)
	return json.Marshal(fields)
}

// ActsOnPoolSize reports whether the provider acts on field.
func (d ProviderDefinition) ActsOnPoolSize(field PoolSizeField) bool {
	for _, declared := range d.PoolSizeFields {
		if declared == field {
			return true
		}
	}
	return false
}
