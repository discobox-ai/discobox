// Package sandboxconfig assembles a sandbox's effective configuration from
// three attribute-owned layers (docs/adr/0012). A field appears on a layer's
// type if and only if that layer may set it; no domain object is embedded in
// more than one layer.
package sandboxconfig

import (
	"strings"

	"github.com/obot-platform/discobox/harness"
)

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
	SandboxID string `json:"sandboxId"`
	// Image is the identity of the image this sandbox actually runs — the
	// resolved image ID the pool host launched, not the (mutable) reference it
	// was asked for. Diagnostic: Effective drops it, so it reaches sandbox.json
	// only under _provenance. That is deliberate — nothing inside the sandbox
	// makes decisions from it, but "which image is this sandbox actually
	// running" is the first question a version-skew investigation asks
	// (ADR 0016).
	Image        string       `json:"image,omitempty"`
	Provider     Provider     `json:"provider"`
	AgentRuntime AgentRuntime `json:"agentRuntime"`
	Sources      []Source     `json:"sources,omitempty"`

	Model               string            `json:"model,omitempty"`
	ModelReasoningLevel string            `json:"modelReasoningLevel,omitempty"`
	ModelServiceTier    string            `json:"modelServiceTier,omitempty"`
	Prompt              []string          `json:"prompt,omitempty"`
	User                User              `json:"user"`
	Env                 map[string]string `json:"env,omitempty"`

	// ProxyEnvs names the subset of Env's keys that carry proxy-trust
	// material (proxy.ClientMaterial.EnvironmentVars) rather than ordinary
	// environment. It is the one list sandbox-agent's runc wrapper reads to
	// know which env vars to republish into a nested Docker container's spec
	// (see docs/adr/0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md);
	// the wrapper never hardcodes env var names itself.
	ProxyEnvs []string `json:"proxyEnvs,omitempty"`

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

// Source is a worker-materialized source the sandbox-agent bind-mounts from
// /.discobox/sources/<slug> onto its in-sandbox target.
type Source struct {
	Slug   string `json:"slug"`
	Target string `json:"target"`
	UID    int64  `json:"uid,omitempty"`
	GID    int64  `json:"gid,omitempty"`
}

// User is the sandbox user identity as the manifest publishes it. Fields the
// request did not give stay unset: the pool agent cannot resolve a sandbox's
// names or invent its ids, so it forwards what it was told and the sandbox
// resolves the rest (ADR 0025 §4). A wholly empty User means the manifest named
// nobody and the image's own account stands (§5).
type User struct {
	Name string `json:"name,omitempty"`
	UID  *int64 `json:"uid,omitempty"`
	GID  *int64 `json:"gid,omitempty"`
	// GroupName is the primary group by name, mutually exclusive with GID and
	// resolvable only inside the sandbox.
	GroupName     string `json:"groupName,omitempty"`
	HomeDirectory string `json:"homeDirectory,omitempty"`
	// AdditionalGroups are supplementary groups, each a name or a numeric GID.
	AdditionalGroups []string `json:"additionalGroups,omitempty"`
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
	// AdditionalGroups names OS groups (already present in the image, e.g.
	// "docker") the sandbox user is added to at boot, alongside its own
	// primary group. Image-owned only, like Volumes: no other layer grants
	// system-level access on the image's behalf.
	AdditionalGroups []string `json:"additionalGroups,omitempty"`
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

// LocalSubnetsToken is a placeholder pool-agent writes into env values that
// must name the sandbox's own directly-connected networks — in practice the
// NO_PROXY list, so a sandbox never sends traffic destined for its own
// networks out through the egress proxy.
//
// It exists so neither side has to know the other's business. pool-agent does
// not know which subnets a sandbox ends up on: Docker allocates them, and the
// nested-Docker bridge does not exist until dockerd first starts. sandbox-agent
// does know, but has no business parsing proxy-variable syntax to work out
// where a subnet list belongs. A token both sides treat as opaque keeps that
// split: pool-agent decides *where* local subnets are needed, sandbox-agent
// decides *what* they are.
const LocalSubnetsToken = "%LOCAL_SUBNETS%" //nolint:gosec // G101: a placeholder token, not a credential.

// ResolveLocalSubnetsToken substitutes LocalSubnetsToken in value with subnets
// (each sandbox-agent's own enumeration of its directly-connected networks),
// joined the same way the rest of an env-list value is: comma-separated. value
// is returned unchanged when it carries no token, so calling this
// unconditionally on every env value is safe.
//
// The substitution collapses any resulting empty entries (a leading, trailing,
// or doubled comma) rather than leaving them for the NO_PROXY parser to trip
// over — that happens whenever subnets is empty, or the token sits at either
// end of value.
func ResolveLocalSubnetsToken(value string, subnets []string) string {
	if !strings.Contains(value, LocalSubnetsToken) {
		return value
	}
	value = strings.ReplaceAll(value, LocalSubnetsToken, strings.Join(subnets, ","))
	return strings.Trim(strings.ReplaceAll(value, ",,", ","), ",")
}
