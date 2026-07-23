// Package sandboxconfig assembles a sandbox's effective configuration from
// three attribute-owned layers (docs/adr/0012). A field appears on a layer's
// type if and only if that layer may set it; no domain object is embedded in
// more than one layer.
package sandboxconfig

import "github.com/obot-platform/discobox/harness"

// Document is the full, unmerged input to Effective. Runtime is assembled by
// the control plane/pool-agent from the create request, Image is snapshotted
// from the registered harness image's OCI label, and Project is read once
// from the resolved source repository at clone time.
type Document struct {
	Runtime RuntimeLayer
	Image   ImageLayer
	Project *ProjectLayer
}

// RuntimeLayer carries pool-agent/control-plane-owned sandbox identity,
// resources, and per-sandbox intent. Every field here is runtime-owned; no
// other layer may set it, except where explicitly noted as an override grant
// on ImageLayer/ProjectLayer.
type RuntimeLayer struct {
	SandboxID    string
	Image        string // resolved image reference (digest ref once ADR's follow-on lands)
	Provider     Provider
	AgentRuntime AgentRuntime
	Resources    Resources
	Sources      []Source

	Model               string
	ModelReasoningLevel string
	ModelServiceTier    string
	Prompt              []string
	User                User
	Env                 map[string]string

	// HarnessMode selects run vs config mode; it is a selection, not a
	// capability grant, so it stays runtime-owned.
	HarnessMode string

	// Files overlays onto the image's declared files, by path.
	Files []File
}

// Provider is non-secret provider context for the sandbox runtime.
type Provider struct {
	Kind       string
	ProjectID  string
	PoolID     string
	Endpoints  map[string]string
	Metadata   map[string]string
	PublicKeys map[string]string
}

// AgentRuntime holds sandbox-agent daemon-local runtime settings.
type AgentRuntime struct {
	ListenAddress          string
	WorkingRoot            string
	RuntimeDir             string
	DatabasePath           string
	ResourceSampleInterval string
	ResourceRetentionCount int
}

// Resources is the sandbox's provider-normalized resource allocation — the
// single representation superseding the SandboxResources/SandboxConfig
// duplication in the pre-ADR manifest (docs/adr/0012 §5).
type Resources struct {
	CPUCores       float64
	MemoryMB       int64
	DiskMB         int64
	TimeoutSeconds int64
}

// Source is a worker-materialized source the sandbox-agent bind-mounts from
// /.discobox/sources/<slug> onto its in-sandbox target.
type Source struct {
	Slug   string
	Target string
	UID    int64
	GID    int64
}

// User is the resolved sandbox user identity.
type User struct {
	Name          string
	UID           *int64
	GID           *int64
	HomeDirectory string
}

// File is a file to write into the harness's home directory when the harness
// is installed.
type File struct {
	Path       string
	Content    string
	CreateOnly bool
	Template   bool
}

// ImageLayer carries the immutable harness contract and defaults baked into
// one sandbox image, snapshotted from the registered image's OCI label.
type ImageLayer struct {
	HarnessID          string
	HarnessName        string
	HarnessDescription string

	// RunCommand and RelaunchCommand are an override grant: the project layer
	// may replace them wholesale.
	RunCommand      []string
	RelaunchCommand []string
	// ConfigCommand is image-owned only; no other layer may set it.
	ConfigCommand []string

	Files   []File
	Volumes []harness.Volume
	Env     map[string]string
}

// ProjectLayer is the resolved source repository's contribution, read once at
// the commit pool-agent clones. It is never re-read once the sandbox is
// running (docs/adr/0012 §7).
type ProjectLayer struct {
	// RunCommand and RelaunchCommand are override-grant only: when non-empty
	// they replace the image's value wholesale.
	RunCommand      []string
	RelaunchCommand []string

	WorkingDirectorySubpath string

	// FilesAdd appends new files by path; it never overrides an existing
	// image- or runtime-declared path.
	FilesAdd []File
}
