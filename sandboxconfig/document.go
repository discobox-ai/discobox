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
	Runtime RuntimeLayer  `json:"runtime"`
	Image   ImageLayer    `json:"image"`
	Project *ProjectLayer `json:"project,omitempty"`
}

// RuntimeLayer carries pool-agent/control-plane-owned sandbox identity,
// resources, and per-sandbox intent. Every field here is runtime-owned; no
// other layer may set it, except where explicitly noted as an override grant
// on ImageLayer/ProjectLayer.
type RuntimeLayer struct {
	SandboxID    string       `json:"sandboxId"`
	Image        string       `json:"image,omitempty"` // resolved image reference (digest ref once ADR's follow-on lands)
	Provider     Provider     `json:"provider"`
	AgentRuntime AgentRuntime `json:"agentRuntime"`
	Resources    Resources    `json:"resources"`
	Sources      []Source     `json:"sources,omitempty"`

	Model               string            `json:"model,omitempty"`
	ModelReasoningLevel string            `json:"modelReasoningLevel,omitempty"`
	ModelServiceTier    string            `json:"modelServiceTier,omitempty"`
	Prompt              []string          `json:"prompt,omitempty"`
	User                User              `json:"user"`
	Env                 map[string]string `json:"env,omitempty"`

	// HarnessMode selects run vs config mode; it is a selection, not a
	// capability grant, so it stays runtime-owned.
	HarnessMode string `json:"harnessMode,omitempty"`

	// Files overlays onto the image's declared files, by path.
	Files []File `json:"files,omitempty"`
}

// Provider is non-secret provider context for the sandbox runtime.
type Provider struct {
	Kind       string            `json:"kind"`
	ProjectID  string            `json:"projectId,omitempty"`
	PoolID     string            `json:"poolId,omitempty"`
	Endpoints  map[string]string `json:"endpoints,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	PublicKeys map[string]string `json:"publicKeys,omitempty"`
}

// AgentRuntime holds sandbox-agent daemon-local runtime settings.
type AgentRuntime struct {
	ListenAddress          string `json:"listenAddress"`
	WorkingRoot            string `json:"workingRoot"`
	RuntimeDir             string `json:"runtimeDir"`
	DatabasePath           string `json:"databasePath"`
	ResourceSampleInterval string `json:"resourceSampleInterval,omitempty"`
	ResourceRetentionCount int    `json:"resourceRetentionCount,omitempty"`
}

// Resources is the sandbox's provider-normalized resource allocation — the
// single representation superseding the SandboxResources/SandboxConfig
// duplication in the pre-ADR manifest (docs/adr/0012 §5).
type Resources struct {
	CPUCores       float64 `json:"cpuCores"`
	MemoryMB       int64   `json:"memoryMb"`
	DiskMB         int64   `json:"diskMb"`
	TimeoutSeconds int64   `json:"timeoutSeconds"`
}

// Source is a worker-materialized source the sandbox-agent bind-mounts from
// /.discobox/sources/<slug> onto its in-sandbox target.
type Source struct {
	Slug   string `json:"slug"`
	Target string `json:"target"`
	UID    int64  `json:"uid,omitempty"`
	GID    int64  `json:"gid,omitempty"`
}

// User is the resolved sandbox user identity.
type User struct {
	Name          string `json:"name,omitempty"`
	UID           *int64 `json:"uid,omitempty"`
	GID           *int64 `json:"gid,omitempty"`
	HomeDirectory string `json:"homeDirectory,omitempty"`
}

// File is a file to write into the harness's home directory when the harness
// is installed.
type File struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	CreateOnly bool   `json:"createOnly,omitempty"`
	Template   bool   `json:"template,omitempty"`
}

// ImageLayer carries the immutable harness contract and defaults baked into
// one sandbox image, snapshotted from the registered image's OCI label.
type ImageLayer struct {
	HarnessID          string `json:"harnessId,omitempty"`
	HarnessName        string `json:"harnessName,omitempty"`
	HarnessDescription string `json:"harnessDescription,omitempty"`

	// RunCommand and RelaunchCommand are an override grant: the project layer
	// may replace them wholesale.
	RunCommand      []string `json:"runCommand,omitempty"`
	RelaunchCommand []string `json:"relaunchCommand,omitempty"`
	// ConfigCommand is image-owned only; no other layer may set it.
	ConfigCommand []string `json:"configCommand,omitempty"`

	Files   []File            `json:"files,omitempty"`
	Volumes []harness.Volume  `json:"volumes,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// ProjectLayer is the resolved source repository's contribution, read once at
// the commit pool-agent clones. It is never re-read once the sandbox is
// running (docs/adr/0012 §7).
type ProjectLayer struct {
	// RunCommand and RelaunchCommand are override-grant only: when non-empty
	// they replace the image's value wholesale.
	RunCommand      []string `json:"runCommand,omitempty"`
	RelaunchCommand []string `json:"relaunchCommand,omitempty"`

	WorkingDirectorySubpath string `json:"workingDirectorySubpath,omitempty"`

	// FilesAdd appends new files by path; it never overrides an existing
	// image- or runtime-declared path.
	FilesAdd []File `json:"filesAdd,omitempty"`
}
