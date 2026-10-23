package sandbox

// ProviderConfigField describes one provider instance configuration field.
type ProviderConfigField struct {
	Key                string `json:"key"`
	Label              string `json:"label"`
	Type               string `json:"type"`
	Description        string `json:"description,omitempty"`
	Placeholder        string `json:"placeholder,omitempty"`
	Required           bool   `json:"required,omitempty"`
	Advanced           bool   `json:"advanced,omitempty"`
	CredentialProvider string `json:"credentialProvider,omitempty"`
	CredentialAuthType string `json:"credentialAuthType,omitempty"`
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

// ActsOnPoolSize reports whether the provider acts on field.
func (d ProviderDefinition) ActsOnPoolSize(field PoolSizeField) bool {
	for _, declared := range d.PoolSizeFields {
		if declared == field {
			return true
		}
	}
	return false
}
