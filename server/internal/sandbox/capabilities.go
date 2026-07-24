package sandbox

// ImageRef pins the image a sandbox runs.
//
// Name is what to pull; Digest is which image that must turn out to be. The
// pair travels to the pool host, which resolves images and enforces the pin
// (ADR 0016 §1, §6). Digest is a config digest — the same value a local Docker
// daemon reports as an image ID, and what HarnessConfig.ImageDigest records —
// not a manifest digest, which never-pushed local builds do not have.
//
// An empty Digest means unpinned: sandboxes on the default image, and sandboxes
// created before pinning existed. Those run whatever Name resolves to.
type ImageRef struct {
	Name   string `json:"name"`
	Digest string `json:"digest,omitempty"`
}

const DefaultSandboxImageName = "discobox-sandbox-agent:local"

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

// ProviderDefinition describes a registered provider driver.
type ProviderDefinition struct {
	Name         string                `json:"name,omitempty"`
	Icon         string                `json:"icon,omitempty"`
	Description  string                `json:"description,omitempty"`
	ConfigFields []ProviderConfigField `json:"configFields,omitempty"`

	// LocalSourceBind reports whether this provider instance runs its sandboxes
	// on the control plane's own filesystem, so a client-local source directory
	// can be bind-mounted and cloned in place.
	//
	// This is per instance, not per driver: the same driver is local or remote
	// depending on its configuration, so instances decide it at construction
	// rather than the package-level Definition doing so. It answers only "can
	// this provider reach the control plane's files"; whether those are the
	// *client's* files additionally requires the client to be on the same host,
	// which the provider cannot know. Callers must check both.
	//
	// Default false: a wrong false costs a source push, a wrong true produces a
	// sandbox bound to a path that does not exist.
	LocalSourceBind bool `json:"localSourceBind,omitempty"`
}

// ProviderStatus describes runtime provider availability.
type ProviderStatus struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	Details   any    `json:"details,omitempty"`
}
