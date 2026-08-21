// Package sandboxconfig assembles a sandbox's effective configuration from
// three attribute-owned layers (docs/adr/0012). A field appears on a layer's
// type if and only if that layer may set it; no domain object is embedded in
// more than one layer.
package sandboxconfig

import (
	"strings"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/sandboxuser"
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
	Git                 GitIdentity       `json:"git"`
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
	// UID and GID are the ownership the sandbox must give the mounted source.
	// They are pointers because the pool agent frequently cannot know them --
	// the account lives in the image and may not exist until boot creates it --
	// and absent has to be expressible. As plain ints, "not given" arrived as 0
	// and boot chowned the primary source tree to root, in exactly the case
	// (no manifest user) where the sandbox is least likely to be running as
	// root (ADR 0033 §3).
	//
	// Absent means boot supplies the identity it resolved, which it has in hand
	// at that point and which is the better answer anyway.
	UID *int64 `json:"uid,omitempty"`
	GID *int64 `json:"gid,omitempty"`
	// BaseCommit is the commit the source was spawned at, from the create
	// request's checkout. The sandbox-agent measures its reported diff stat
	// against it, which is what lets a listing show what a sandbox has changed
	// without running git from outside. Absent when the source has no recorded
	// checkout commit; the diff stat is then simply not reported.
	BaseCommit string `json:"baseCommit,omitempty"`
	// AwaitsDelivery marks a source whose content is not in place when the
	// sandbox's container is created: the client pushes it in afterwards, and
	// pool-agent materializes it on the resume that follows (ADR 0001). Absent
	// means the source was fully materialized before the container existed,
	// which is every clone-delivered source.
	//
	// The sandbox holds its harness launch when any source carries this, until
	// pool-agent reports the sandbox settled (SourcesReadyFileName), so nothing
	// runs against a workspace that has not arrived.
	AwaitsDelivery bool `json:"awaitsDelivery,omitempty"`
	// UpstreamRef is the remote-tracking ref the source would fetch upstream
	// into, derived from the branch it was cloned at. Once the sandbox has
	// fetched, the diff stat's base moves forward to the merge base with this
	// ref, so commits the sandbox pulled rather than wrote stop counting as
	// its changes — the same rule `discobox diff` resolves with. The ref is
	// verified in the repository rather than assumed, so naming one that does
	// not exist (a push-delivered source has no remote at all) costs nothing.
	UpstreamRef string `json:"upstreamRef,omitempty"`
}

// User is the sandbox user identity as the manifest publishes it. Fields the
// request did not give stay unset: the pool agent cannot resolve a sandbox's
// names or invent its ids, so it forwards what it was told and the sandbox
// resolves the rest (ADR 0025 §4). A wholly empty User means the manifest named
// nobody and the image's own account stands (§5).
// User is the identity a sandbox runs as. It is an alias rather than a parallel
// type: the API, the manifest, the pool agent, and the launch path all describe
// identity with one vocabulary, so a field cannot mean one thing here and
// something else one layer in (ADR 0025 §1).
type User = sandboxuser.User

// GitIdentity is the authorship the sandbox commits under. It is deliberately
// not part of User: User is the account a process runs as, one schema shared
// with exec create (ADR 0025 §1), and a single exec has no committer of its own.
// The two are independently absent — a sandbox running as the image's own
// account still commits as the caller — so neither nests in the other.
//
// Runtime-owned and single-writer: no image or project layer contributes to it.
// Either field may be empty; boot writes only what it was given.
type GitIdentity struct {
	UserName  string `json:"userName,omitempty"`
	UserEmail string `json:"userEmail,omitempty"`
}

// Configured reports whether anything was named at all. It is the one test for
// "did the request give a git identity", so adding a field here means teaching
// this function rather than every call site — the same rule sandboxuser.Named
// follows for run identity.
func (g GitIdentity) Configured() bool {
	return strings.TrimSpace(g.UserName) != "" || strings.TrimSpace(g.UserEmail) != ""
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
	// Secrets are the harness's declared credentials. The sandbox needs them
	// for their Delivery alone: a secret delivered by file must not also be
	// exported as an environment variable, and only the declaration says which
	// (harness.SecretDeliveryFile). Image-owned; no other layer may set it.
	Secrets []harness.Secret `json:"secrets,omitempty"`
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

// SandboxGroups is the supplementary set the sandbox's user belongs to.
//
// Two layers can name one: the harness image declares groups as a label, and a
// create request may name its own. They are all-or-nothing rather than unioned
// (ADR 0025 §2), so a request naming any replaces the label's entirely and a
// request naming none inherits them.
//
// It exists as one function because it was two. Boot materialized the image
// label's groups into /etc/group while the exec defaults preferred the
// manifest user's, so a sandbox that declared its own groups had them in every
// exec's credential while the OS account was never added to them. Which list
// is authoritative is one question and must have one answer.
func (c Config) SandboxGroups() []string {
	if len(c.User.AdditionalGroups) > 0 {
		return append([]string(nil), c.User.AdditionalGroups...)
	}
	return append([]string(nil), c.AdditionalGroups...)
}

// SandboxConfigDir is where the sandbox's config volume is bound inside the
// container, and so where every file in this contract is read from. Boot places
// the bind (sandbox-agent/boot); pool-agent writes the files behind it from the
// host, and the mount is read-only so nothing inside can answer itself.
const SandboxConfigDir = "/etc/discobox"

// SourcesReadyFileName is the file pool-agent creates in the config volume once
// every one of the sandbox's sources is materialized and the document beside it
// is final. It is the signal a sandbox whose source arrives by push waits on
// before running anything: until the client's push has landed and been checked
// out, the workspace is an empty repository, and the configuration the project
// itself declares (.discobox/project.json) cannot have been read yet either.
//
// It is a separate signal from the per-source materialized marker deliberately.
// That marker is written the moment a source's checkout completes, which is
// before pool-agent has re-read the project layer and decided whether the
// container has to be rebuilt to honor it — a sandbox gated on it would launch
// its harness against a configuration about to be replaced.
const SourcesReadyFileName = "ready"

// SourcesReadyPath is SourcesReadyFileName as the sandbox sees it.
const SourcesReadyPath = SandboxConfigDir + "/" + SourcesReadyFileName

// SourcesAwaitDelivery reports whether any of these sources is still to be
// delivered by the sandbox's client, which is what makes the readiness signal
// apply. A config that predates the field says no, which is correct for it:
// every source it names was materialized before its container existed.
func SourcesAwaitDelivery(sources []Source) bool {
	for _, source := range sources {
		if source.AwaitsDelivery {
			return true
		}
	}
	return false
}

// AwaitsSourceDelivery reports whether the sandbox is waiting on its client for
// any of its sources.
func (c Config) AwaitsSourceDelivery() bool { return SourcesAwaitDelivery(c.Sources) }
