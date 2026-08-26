package sandbox

// ImageRef pins the image a sandbox runs.
//
// Name is what to pull; Digest is which image that must turn out to be. The
// pair travels to the pool host, which resolves images and enforces the pin
// (ADR 0016 §1, §6).
//
// Digest is the registry digest the image was pulled under — an index digest
// for a multi-platform image — which both Docker image stores report in
// RepoDigests. For a locally built image, which was never pushed and has no
// RepoDigests, it is the image ID.
//
// It used to be documented as a config digest, "the same value a local Docker
// daemon reports as an image ID". That was true of the classic image store and
// false of the containerd one, which reports the index digest and is the
// default in current Docker — so the recorded pin was a value the daemon would
// never produce, and every sandbox on a published multi-arch image refused to
// launch against an image that was sitting right there.
//
// An empty Digest means unpinned: sandboxes on the default image, and sandboxes
// created before pinning existed. Those run whatever Name resolves to.
type ImageRef struct {
	Name   string `json:"name"`
	Digest string `json:"digest,omitempty"`
}

// DefaultSandboxImageName is the image a sandbox with no harness config runs.
//
// It names a local tag because a development build has no published image to
// reach for: `task build:images` writes exactly this. A release overwrites it
// at link time with the sandbox agent built for that release, which is why it
// is a var. DISCOBOX_DEFAULT_SANDBOX_IMAGE overrides either.
var DefaultSandboxImageName = "discobox-sandbox-agent:local"

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

	// LocalSourceRoots are the host paths under which a client-local source
	// directory can be bind-mounted and cloned in place, because this provider
	// instance runs its sandboxes on the control plane's own filesystem and
	// exposes those paths to them.
	//
	// Reachability is a property of the path, not of the provider: a Docker
	// instance mounts the host directories its configuration names and nothing
	// else, so a directory outside them is as unreachable as one on another
	// machine. A provider that shares the whole filesystem says "/". Empty
	// means no local directory is reachable at all.
	//
	// This is per instance, not per driver: the same driver is local or remote
	// depending on its configuration, so instances fill it in at construction
	// rather than the package-level Definition doing so. It answers only "can
	// this provider reach the control plane's files at this path"; whether
	// those are the *client's* files additionally requires the client to be on
	// the same host, which the provider cannot know. Callers must check both.
	//
	// Default empty: a missing root costs a source push, a wrong one produces a
	// sandbox bound to a path that does not exist.
	LocalSourceRoots []string `json:"localSourceRoots,omitempty"`
}

// ProviderStatus describes runtime provider availability.
type ProviderStatus struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	Details   any    `json:"details,omitempty"`
}
