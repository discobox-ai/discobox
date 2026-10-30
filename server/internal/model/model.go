// Package model defines the combined database and API resource models.
//
// These structs intentionally carry GORM persistence tags together with JSON
// and OpenAPI-facing tags. If the API and database shapes diverge later,
// split only the affected resources into DTOs.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/internal/originkey"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/x/id"
)

const (
	// Pool desired state is DesiredStatePresent / DesiredStateDeleted, shared
	// with every other orchestrated resource (see lifecycle.go).

	PoolStatePending     = "pending"
	PoolStateRegistering = "registering"
	PoolStateActive      = "active"
	PoolStateOffline     = "offline"
	PoolStateFailed      = "failed"
	PoolStateDeleted     = "deleted"

	// SandboxStatePending opens the sandbox existence vocabulary. Only
	// SandboxReconciler writes State; what the container is doing lives on
	// RuntimeState, written only by the pool agent's reports (ADR 0034 §§1–2).
	SandboxStatePending = "pending"
	// SandboxStateAwaitingSource means the sandbox is provisioned and waiting
	// for the client to push its source, because the source is a local
	// directory this server's provider cannot reach. The client pushes into the
	// sandbox's repository, then calls continue to name the commit to check out.
	//
	// It reads like a runtime condition and is not one: the reconciler sets it,
	// parks on it, resumes from it, and derives its give-up deadline from the
	// StateChangedAt it stamps. It is a phase of creation (ADR 0034 §3).
	SandboxStateAwaitingSource = "awaiting_source"
	// SandboxStateReady means the reconciler has converged the sandbox's
	// container against its spec. Whether that container is running is a
	// separate question, answered by RuntimeState.
	SandboxStateReady = "ready"
	// SandboxStateArchived means the container and every disposable resource are
	// gone, and the sandbox's durable tree is retained on its pool host so it can
	// be reinstantiated (ADR 0022 §1). It is the settled observation of desired
	// state `archived`, not a power state: an archived sandbox has nothing to
	// start.
	SandboxStateArchived = "archived"
	SandboxStateDeleted  = "deleted"
	SandboxStateFailed   = "failed"

	// SandboxRuntimeStateStarting opens the sandbox runtime vocabulary: what
	// the container is doing, as observed and reported by the pool agent
	// hosting it (ADR 0017 §10, ADR 0034 §2).
	// Store.ApplySandboxStateReports is the only writer.
	//
	// The empty value is the fifth member of this vocabulary and means *not
	// observed*, which is not the same as SandboxRuntimeStateStopped: it is what
	// a sandbox reads between being recorded and its agent first reporting on
	// it. It is deliberately absent from SandboxRuntimeStates, because the API
	// omits the field entirely rather than serving an empty enum value.
	SandboxRuntimeStateStarting = "starting"
	SandboxRuntimeStateRunning  = "running"
	SandboxRuntimeStateStopping = "stopping"
	SandboxRuntimeStateStopped  = "stopped"
	// SandboxRuntimeStateSuspended is a container frozen whole: its memory and
	// every process kept, nothing in it running (ADR 26-10-16-420 §1). Any
	// start, or any use, thaws it.
	SandboxRuntimeStateSuspended = "suspended"

	// GitSourceDeliveryClone has the sandbox fetch the source itself.
	// GitSourceDeliveryPush has the client push it in, for a local directory the
	// provider cannot reach. Delivery is stated, never inferred from which
	// source fields happen to be set.
	GitSourceDeliveryClone = "clone"
	GitSourceDeliveryPush  = "push"
	// GitSourceDeliveryCache has a client deliver the source into a namespace
	// of the pool's cache for its remote, which the sandbox clones (ADR
	// 26-10-10-545). It is every source created since; the two above are the
	// record of how earlier ones were delivered.
	GitSourceDeliveryCache = "cache"

	// SandboxUpgradePolicyAutomatic and SandboxUpgradePolicyManual are a
	// project's answer to whether its stopped and failed sandboxes follow their
	// harness bundle (ADR 0082 §3, ADR 0121).
	//
	// The empty value is the third member of this vocabulary and means *the
	// server default*, which is automatic: a project that has never chosen
	// tracks the default as it changes rather than being frozen to whatever it
	// was at create. It is deliberately absent from SandboxUpgradePolicies,
	// because the API omits the field rather than serving an empty enum value.
	SandboxUpgradePolicyAutomatic = "automatic"
	SandboxUpgradePolicyManual    = "manual"
)

// Enum registries. Each slice is the canonical set of values a string-typed
// domain field may hold. They exist to be enumerated: the API layer serves
// these values verbatim (see services.Convert), and TestModelEnumsMatchAPISchema
// cross-checks every slice against the generated OpenAPI enum so the two lists
// cannot silently drift. Keep each slice in sync with the consts directly above
// it — a value present in one list but not the other is exactly the bug the test
// catches.
var (
	PoolStates = []string{
		PoolStatePending,
		PoolStateRegistering,
		PoolStateActive,
		PoolStateOffline,
		PoolStateFailed,
		PoolStateDeleted,
	}
	SandboxStates = []string{
		SandboxStatePending,
		SandboxStateAwaitingSource,
		SandboxStateReady,
		SandboxStateArchived,
		SandboxStateDeleted,
		SandboxStateFailed,
	}
	SandboxRuntimeStates = []string{
		SandboxRuntimeStateStarting,
		SandboxRuntimeStateRunning,
		SandboxRuntimeStateStopping,
		SandboxRuntimeStateStopped,
		SandboxRuntimeStateSuspended,
	}
	GitSourceDeliveries = []string{
		GitSourceDeliveryClone,
		GitSourceDeliveryPush,
		GitSourceDeliveryCache,
	}
	GitSourcePurposes = []string{
		GitSourcePurposeWork,
		GitSourcePurposeSkills,
	}
	SandboxUpgradePolicies = []string{
		SandboxUpgradePolicyAutomatic,
		SandboxUpgradePolicyManual,
	}
)

// SandboxUpgradesAutomatically reports whether a project's stopped sandboxes
// are moved onto their harness config's bundle when it changes (ADR 0082).
//
// Empty is automatic, which is what makes the default a server-side answer the
// project follows rather than a value copied into every row at create.
func SandboxUpgradesAutomatically(project *Project) bool {
	if project == nil {
		return false
	}
	return strings.TrimSpace(project.SandboxUpgradePolicy) != SandboxUpgradePolicyManual
}

// SandboxIsLive reports whether a sandbox has a container something is
// currently relying on, and so must not be rebuilt underneath it.
//
// Running is the obvious case. Awaiting-source is the subtle one: it is parked
// mid-create waiting for the client's push, and replacing its container in the
// middle of that hands the push a different sandbox than the one it started
// against. The transitional runtime states count as live because the runtime is
// acting on the container right now, and suspended because its container holds
// the memory of everything that was running in it.
//
// It takes the sandbox rather than a state string because the question spans
// both state axes: `awaiting_source` is a phase of creation and the rest are
// power (ADR 0034 §§1–2). That is also why it is a function rather than a
// comparison at each call site — the wedge described in ADR 0017 §4 came from
// checks spelled State == "stopped", which is really asking "is anything
// relying on this", a question several values answer the same way.
//
// Archived is not live, and the answer is not the interesting part: an archived
// sandbox has no container by intent, so callers that treat "not live" as
// "needs a container built" must check desired state first (ADR 0022 §5).
func SandboxIsLive(sandbox *Sandbox) bool {
	if sandbox == nil {
		return false
	}
	if sandbox.State == SandboxStateAwaitingSource {
		return true
	}
	switch sandbox.RuntimeState {
	case SandboxRuntimeStateStarting, SandboxRuntimeStateRunning, SandboxRuntimeStateStopping, SandboxRuntimeStateSuspended:
		// An archived sandbox is not live whatever its last observation said:
		// the archive removed the container, and reports stop covering it
		// (ADR 0022 §5), so a stale `running` must not read as live.
		return sandbox.State != SandboxStateArchived && sandbox.State != SandboxStateDeleted
	default:
		return false
	}
}

// User represents an authenticated user.
type User struct {
	ID        string    `gorm:"primaryKey;type:text" json:"id" doc:"Stable user ID"`
	Email     string    `gorm:"uniqueIndex;not null;type:text" json:"email" doc:"User email address" format:"email"`
	Name      *string   `gorm:"type:text" json:"name,omitempty" doc:"Display name"`
	AvatarURL *string   `gorm:"column:avatar_url;type:text" json:"avatarUrl,omitempty" doc:"Avatar image URL" format:"uri"`
	Provider  string    `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"provider" doc:"Authentication provider"`
	Subject   string    `gorm:"not null;type:text;uniqueIndex:idx_user_provider_subject" json:"subject" doc:"Provider subject identifier"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
}

func (User) TableName() string { return "users" }

func (u *User) BeforeCreate(_ *gorm.DB) error {
	if u.ID == "" {
		var err error
		u.ID, err = id.New(id.PrefixUser)
		if err != nil {
			return err
		}
	}
	return nil
}

// Project groups sandboxes and provider configuration.
type Project struct {
	ID          string `gorm:"primaryKey;type:text" json:"id" doc:"Stable project ID"`
	OwnerUserID string `gorm:"column:owner_user_id;not null;type:text;index;uniqueIndex:idx_project_owner_name,priority:1" json:"ownerUserId" doc:"Owning user ID"`
	Name        string `gorm:"not null;type:text;uniqueIndex:idx_project_owner_name,priority:2" json:"name" doc:"Project display name" maxLength:"200"`
	// Default is the flag that resolves the "default" project alias. It is the
	// only thing that does: a project is addressed by ID, and by name only as a
	// client-side convenience, so nothing about the default project's name or
	// identity is special.
	Default                bool   `gorm:"column:default_project;not null;default:false;index" json:"default" doc:"Whether this is the user's default project"`
	DefaultPoolID          string `gorm:"column:default_pool_id;type:text;default:''" json:"defaultPoolId,omitempty" doc:"Default pool ID for new sandboxes"`
	DefaultHarnessConfigID string `gorm:"column:default_harness_config_id;type:text;default:''" json:"defaultHarnessConfigId,omitempty" doc:"Default harness config ID"`
	// ArchiveRetentionSeconds is how long this project's archived sandboxes are
	// kept before they are purged (ADR 0022 §4). Zero means the server default:
	// a project that has never chosen gets the default as it changes, rather
	// than being frozen to whatever it was at create time.
	ArchiveRetentionSeconds int64 `gorm:"column:archive_retention_seconds;not null;default:0" json:"archiveRetentionSeconds,omitempty" doc:"How long archived sandboxes are kept before being purged, in seconds. Zero means the server default."`
	// SandboxUpgradePolicy decides whether this project's stopped sandboxes are
	// re-pinned onto their harness config's bundle when that bundle moves
	// (ADR 0082 §3). Empty means the server default, which is `automatic`.
	//
	// It is an opt-out rather than an opt-in because the upgrade is what a user
	// wants almost always; what it buys is the ability to hold a discobox on
	// the bundle it was built with, since a rebuild discards whatever was
	// written to the container's filesystem outside its durable volumes.
	SandboxUpgradePolicy string `gorm:"column:sandbox_upgrade_policy;not null;type:text;default:''" json:"sandboxUpgradePolicy,omitempty" doc:"Whether this project's stopped and failed sandboxes follow their harness config's bundle. Empty means the server default, which is automatic." enum:"automatic,manual"`
	// Welcomed records that this project has shown its introduction, so the
	// launcher shows it once and never again. It is here rather than in client
	// state because a person meets Discobox once, not once per machine they
	// reach the same project from.
	Welcomed bool `gorm:"column:welcomed;not null;default:false" json:"welcomed" doc:"Whether this project has already shown its introduction"`
	// Egress is the project's level of every one of its discoboxes' egress
	// policy (ADR 26-10-01-032). The effective policy is merged whenever it is
	// delivered, never stored, so a change here reaches each discobox by
	// marking its egress push. Written by store.SetProjectEgress alone.
	Egress    egress.Policy `gorm:"column:egress;type:text;serializer:json" json:"egress" doc:"The project's egress policy level"`
	CreatedAt time.Time     `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time     `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Owner                    *User                     `gorm:"-" json:"owner,omitempty" doc:"Project owner"`
	Pools                    []Pool                    `gorm:"foreignKey:ProjectID" json:"pools,omitempty" doc:"Project pools"`
	Members                  []ProjectMember           `gorm:"foreignKey:ProjectID" json:"members,omitempty" doc:"Project members"`
	Sandboxes                []Sandbox                 `gorm:"foreignKey:ProjectID" json:"sandboxes,omitempty" doc:"Project sandboxes"`
	SandboxProviderInstances []SandboxProviderInstance `gorm:"foreignKey:ProjectID" json:"sandboxProviderInstances,omitempty" doc:"Sandbox provider instances"`
	HarnessConfigs           []HarnessConfig           `gorm:"foreignKey:ProjectID" json:"harnessConfigs,omitempty" doc:"Harness configurations"`
}

func (Project) TableName() string { return "projects" }

func (p *Project) BeforeCreate(_ *gorm.DB) error {
	if p.ID == "" {
		var err error
		p.ID, err = id.New(id.PrefixProject)
		if err != nil {
			return err
		}
	}
	return nil
}

// ProjectMember grants a user access to a project.
type ProjectMember struct {
	ProjectID string    `gorm:"column:project_id;primaryKey;type:text" json:"projectId" doc:"Project ID"`
	UserID    string    `gorm:"column:user_id;primaryKey;type:text" json:"userId" doc:"User ID"`
	Role      string    `gorm:"not null;type:text;default:'member'" json:"role" doc:"Project role"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
	User    *User    `gorm:"-" json:"-"`
}

func (ProjectMember) TableName() string { return "project_members" }

// ServerState stores generic server settings and one-time state flags. Delete a
// row to allow its associated initialization to run again.
type ServerState struct {
	Key       string          `gorm:"primaryKey;type:text" json:"key" doc:"State key"`
	Value     json.RawMessage `gorm:"column:value;type:text" json:"value,omitempty" doc:"State value"`
	CreatedAt time.Time       `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time       `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`
}

func (ServerState) TableName() string { return "server_state" }

// SandboxAccessIssuerKey stores per-project, per-user key material used by the
// control plane to sign sandbox access tokens.
type SandboxAccessIssuerKey struct {
	ProjectID           string     `gorm:"column:project_id;primaryKey;type:text" json:"-"`
	UserID              string     `gorm:"column:user_id;primaryKey;type:text" json:"-"`
	PublicKey           string     `gorm:"column:public_key;not null;type:text" json:"-"`
	EncryptedPrivateKey []byte     `gorm:"column:encrypted_private_key;not null" json:"-"`
	KeyType             string     `gorm:"column:key_type;not null;type:text;default:'ed25519'" json:"-"`
	RotatedAt           *time.Time `gorm:"column:rotated_at" json:"-"`
	RevokedAt           *time.Time `gorm:"column:revoked_at;index" json:"-"`
	CreatedAt           time.Time  `gorm:"autoCreateTime" json:"-"`
	UpdatedAt           time.Time  `gorm:"autoUpdateTime" json:"-"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
	User    *User    `gorm:"-" json:"-"`
}

func (SandboxAccessIssuerKey) TableName() string { return "sandbox_access_issuer_keys" }

// ProjectUserKey is an alias of SandboxAccessIssuerKey, and the name the
// sandbox auth package's key store (auth/sandbox) is written against.
type ProjectUserKey = SandboxAccessIssuerKey

// HarnessConfig stores a project-scoped harness runtime configuration.
//
// A harness config is the single harness concept: the three included harnesses
// are seeded as built-in configs rather than surfaced as separate definitions.
// A config is only selectable once Configured is true, which happens when its
// configure flow completes successfully; Deconfigure reverses that by removing
// the assets the flow created.
type HarnessConfig struct {
	Revision                 int64                      `gorm:"not null;default:1" json:"revision"`
	SaveBack                 *harnesscapture.Definition `gorm:"type:text;serializer:json" json:"saveBack,omitempty"`
	ConfigureRevision        int64                      `gorm:"not null;default:0" json:"-"`
	ConfigureSecretRevisions map[string]string          `gorm:"type:text;serializer:json" json:"-"`
	ID                       string                     `gorm:"primaryKey;type:text" json:"id" doc:"Stable harness config ID"`
	ProjectID                string                     `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_harness_config_project_name,priority:1" json:"projectId" doc:"Project ID"`
	Slug                     string                     `gorm:"column:slug;not null;type:text;default:'';index" json:"slug" doc:"Stable, URL-safe identifier used to select the harness config (e.g. codex). Unique within the project." pattern:"^[a-z0-9][a-z0-9-]*$"`
	BuiltIn                  bool                       `gorm:"column:built_in;not null;default:false" json:"builtIn" doc:"True for the included harnesses seeded by the server. Built-in configs track their bundle and cannot be deleted."`
	Configured               bool                       `gorm:"column:configured;not null;default:false" json:"configured" doc:"True once the configure flow completed successfully. Only configured harnesses can be selected to run."`
	Name                     string                     `gorm:"column:name;not null;type:text;uniqueIndex:idx_harness_config_project_name,priority:2" json:"name" doc:"Harness config name" maxLength:"200"`
	// Bundle is the harness bundle the manifest is read from and every box on
	// the harness applies (ADR 26-10-16-423 §2). Image is a manifest file, the
	// whole manifest of a template with no image (ADR 0145 §3), and empty on a
	// bundle: no harness is an image (§10). A config registered before
	// bundles has neither (internal/database cleared the image it had), and
	// keeps the manifest its image's labels gave it until a bundle is
	// registered for it.
	Bundle       string `gorm:"column:bundle;not null;type:text;default:''" json:"bundle,omitempty" doc:"The harness bundle its manifest is read from and every discobox on it applies. Absent on a harness registered before bundles, or one whose image is a manifest file."`
	BundleDigest string `gorm:"column:bundle_digest;not null;type:text;default:''" json:"bundleDigest,omitempty" doc:"Content digest observed when the harness bundle was registered"`
	// ManifestDigest is the digest of the composed manifest the config
	// snapshots: the harness bundle's labels on the platform's layer (ADR
	// 26-10-16-423 §2). It moves when either moves and with nothing else, so a
	// platform-layer edit, which moves no bundle digest, still reaches the
	// sandboxes on the config (harnessconfigs.snapshotImageMetadata).
	ManifestDigest   string                `gorm:"column:manifest_digest;not null;type:text;default:''" json:"manifestDigest,omitempty" doc:"Digest of the manifest composed from the harness bundle's labels on the platform's layer, which moves when either does"`
	Image            string                `gorm:"column:image;not null;type:text;default:''" json:"image,omitempty" doc:"A manifest file, the whole manifest of a template with no image. Absent on a harness bundle."`
	ImageDigest      string                `gorm:"column:image_digest;not null;type:text;default:''" json:"imageDigest,omitempty" doc:"Content digest observed when the manifest file was registered"`
	RunCommand       []string              `gorm:"column:run_command;type:text;serializer:json" json:"runCommand,omitempty" doc:"Run argv snapshotted from the registered bundle's label."`
	RelaunchCommand  []string              `gorm:"column:relaunch_command;type:text;serializer:json" json:"relaunchCommand,omitempty" doc:"Relaunch argv snapshotted from the registered bundle's label."`
	ConfigCommand    []string              `gorm:"column:config_command;type:text;serializer:json" json:"configCommand,omitempty" doc:"Config-mode argv snapshotted from the registered bundle's label."`
	ConfigReminder   string                `gorm:"column:config_reminder;not null;type:text;default:''" json:"configReminder,omitempty" doc:"Reminder shown outside the interactive configuration terminal, snapshotted from the registered bundle's label."`
	ConfigPorts      []harness.ConfigPort  `gorm:"column:config_ports;type:text;serializer:json" json:"configPorts,omitempty" doc:"Local ports the configure flow forwards into the configure sandbox at the same port number, snapshotted from the registered bundle's label."`
	Files            []HarnessConfigFile   `gorm:"column:files;type:text;serializer:json" json:"files,omitempty" doc:"Non-secret files declared by the bundle's label. Baseline only; the configure flow's files live in ConfiguredFiles."`
	Secrets          []HarnessConfigSecret `gorm:"column:secrets;type:text;serializer:json" json:"secrets,omitempty" doc:"Environment-variable secret declarations snapshotted from the image label."`
	Env              map[string]string     `gorm:"column:env;type:text;serializer:json" json:"env,omitempty" doc:"Default environment variables snapshotted from the image label."`
	Volumes          []harness.Volume      `gorm:"column:volumes;type:text;serializer:json" json:"volumes,omitempty" doc:"Declarative volumes snapshotted from the image label."`
	AdditionalGroups []string              `gorm:"column:additional_groups;type:text;serializer:json" json:"additionalGroups,omitempty" doc:"Supplementary OS groups snapshotted from the image label."`
	// Platforms is what a manifest file's template runs on (ADR 0145 §1): the
	// one platform it declares. A sandbox runs on its pool's platform, which
	// this must name. Empty on a bundle, whose steps select the platform they
	// run on, which rules no pool out.
	Platforms platform.Set `gorm:"column:platforms;type:text;serializer:json" json:"platforms,omitempty" doc:"Platforms a manifest file's template runs on, as os/arch. A discobox on this harness is placed only on a pool that hosts one of them. Absent on a bundle, which rules no pool out."`
	// ImageKind is the kind of box the harness runs on (ADR 26-10-09-106 §4),
	// read when it is inspected: OCI for a bundle, which a Docker pool's
	// sandbox base image applies, or the VM kind a manifest file
	// declares. A sandbox on it is placed only on a pool that runs that kind.
	// The column defaults to OCI; a manifest-file harness from before kinds is
	// left with none (database.unsetManifestFileImageKinds), which places
	// nowhere.
	ImageKind        platform.ImageKind `gorm:"column:image_kind;not null;type:text;default:'oci'" json:"imageKind" doc:"The kind of box the harness runs on: oci for a bundle, or vm/<driver> for a manifest file's VM image built for that driver. A discobox on this harness is placed only on a pool that runs this kind. Empty on a manifest-file harness registered before image kinds, which runs on no pool until its file declares one and it is refreshed."`
	ImageEgressHosts []string           `gorm:"column:image_egress_hosts;type:text;serializer:json" json:"imageEgressHosts,omitempty" doc:"Hosts the registered bundle declares it needs, snapshotted from its label."`
	// Egress is the harness level a person set (ADR 26-10-01-032). It is kept
	// apart from ImageEgressHosts so registering a new bundle replaces what the
	// bundle declares and leaves what a person chose alone.
	Egress egress.Policy `gorm:"column:egress;type:text;serializer:json" json:"egress" doc:"The harness config's egress policy level"`
	// ConfiguredFiles and ConfiguredSecretIDs record what the configure flow
	// produced, kept separate from the image-declared baseline so Deconfigure can
	// remove exactly what it created and leave the baseline intact.
	ConfiguredFiles     []HarnessConfigFile `gorm:"column:configured_files;type:text;serializer:json" json:"configuredFiles,omitempty" doc:"Files written by the configure flow. Overlay the image-declared Files when resolving a sandbox."`
	ConfiguredSecretIDs []string            `gorm:"column:configured_secret_ids;type:text;serializer:json" json:"-" doc:"Secrets created by the configure flow, deleted on deconfigure."`
	// ConfigureSandboxID names the in-flight configure sandbox. It is the durable
	// handle the harness-config reconciler watches, so a configure that is running
	// when the server restarts is still picked up and finished.
	ConfigureSandboxID string `gorm:"column:configure_sandbox_id;type:text;default:''" json:"configureSandboxId,omitempty" doc:"Sandbox running the configure flow, while one is in flight."`
	ConfigureError     string `gorm:"column:configure_error;type:text;default:''" json:"configureError,omitempty" doc:"Why the last configure attempt failed. Cleared when a configure starts or succeeds."`
	// BoundSecrets is not a column: the store counts the config's secret
	// bindings into it on every read (store.withBoundSecrets), so a client can
	// tell a harness that runs with no credentials at all from one that has
	// them without a request per harness.
	BoundSecrets int       `gorm:"column:bound_secrets;->;-:migration" json:"boundSecrets" doc:"How many of the harness's environment variables have a project secret bound. Zero on a harness with a configure command means every discobox on it runs without credentials until it is configured."`
	CreatedAt    time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project   *Project  `gorm:"foreignKey:ProjectID" json:"-"`
	Sandboxes []Sandbox `gorm:"foreignKey:HarnessConfigID" json:"-"`
}

func (HarnessConfig) TableName() string { return "harness_configs" }

func (a *HarnessConfig) BeforeCreate(_ *gorm.DB) error {
	if a.ID == "" {
		var err error
		a.ID, err = id.New(id.PrefixHarnessConfig)
		if err != nil {
			return err
		}
	}
	return nil
}

// HarnessConfigFile is a file to write into a harness's home directory when the
// harness is installed.
type HarnessConfigFile struct {
	Path       string `json:"path" doc:"File path relative to the harness's home directory"`
	Content    string `json:"content" doc:"File content to write"`
	CreateOnly bool   `json:"createOnly,omitempty" doc:"Only create this file if it does not already exist"`
}

// HarnessConfigSecret declares an environment variable the harness expects, and
// whether it is required for the harness to run.
type HarnessConfigSecret struct {
	Name     string `json:"name" doc:"Environment variable name the harness expects to be set" pattern:"^[A-Za-z_][A-Za-z0-9_]*$"`
	Required bool   `json:"required,omitempty" doc:"Whether the secret must be set for the harness to run"`
	// OneOfGroup ties a required secret to a set of alternatives: required secrets
	// sharing a group are satisfied when at least one member is present.
	OneOfGroup string `json:"oneOfGroup,omitempty" doc:"Groups a required secret with alternatives; the requirement is satisfied when at least one member of the group is present"`
	// Delivery says how the sandbox hands the credential to the harness. Empty
	// exports Name into the harness's environment; "file" withholds that export
	// because the harness reads the credential from an installed file instead.
	// See harness.SecretDeliveryFile for why withholding it matters.
	Delivery string `json:"delivery,omitempty" doc:"How the credential reaches the harness: env exports the variable named by name, file withholds it because the harness reads the credential from a file the harness config installs" enum:"env,file"`
	// RejectionScope names the requests that answer for the credential
	// (ADR 26-10-13-595): only what an upstream makes of one of them is taken
	// as a fact about it. Empty is every request.
	RejectionScope []HarnessSecretRejectionRule `json:"rejectionScope,omitempty" doc:"Requests whose refusal or acceptance says whether the credential is alive; empty is every request"`
	// Routes are where the harness itself sends the credential, declared by
	// the image and never by a caller or a configure flow. Configure writes
	// them onto the secret it creates for it (ADR 26-10-02-924).
	Routes []SecretRoute `json:"routes,omitempty" doc:"Where the harness itself sends this credential, declared by its image"`
}

// HarnessSecretRejectionRule is one host of a harness secret's rejection scope,
// and the paths on it. See harness.RejectionRule.
type HarnessSecretRejectionRule struct {
	Host  string   `json:"host" doc:"Host that answers for the credential, with the hosts beneath it"`
	Paths []string `json:"paths,omitempty" doc:"Path prefixes on a segment boundary; empty is every path on the host"`
}

// GitSource describes a Git source to materialize into a sandbox.
type GitSource struct {
	Kind     string `json:"kind" doc:"Source kind. Currently only git is supported." enum:"git"`
	Delivery string `json:"delivery,omitempty" doc:"How the source reaches the sandbox, decided by the server and recorded at create. cache is every source created now. A client delivers it into a Git namespace of the pool's cache for the source's remote, and the sandbox clones it from the pool; the sandbox waits in the awaiting_source phase until the delivery is reported complete. clone and push are how earlier discoboxes were delivered and keep their meaning for them. clone means the sandbox fetched it from url, or from localDirectory through its pool. push means the client pushed it into a repository the pool made for that sandbox." enum:"clone,push,cache"`
	// CacheKey names the pool cache a cache-delivered source is delivered
	// into (ADR 26-10-10-545 §2). It is recorded rather than derived again by
	// each stage that needs it, and it is the server's: whatever a client
	// sends is replaced at create.
	CacheKey       *string `json:"cacheKey,omitempty" doc:"The pool's Git cache this source is delivered into, set by the server for a cache-delivered source. It is the source's remote, normalized to host and path, or a key for the client's own directory when the source has no remote. Sources that share it share one object store on a pool, so a delivery sends only what the pool has not been sent before."`
	Slug           *string `json:"slug,omitempty" doc:"Stable URL-safe source slug used to address the source as a sandbox Git repository"`
	URL            *string `json:"url,omitempty" doc:"Remote Git source URL" format:"uri"`
	LocalDirectory *string `json:"localDirectory,omitempty" doc:"Absolute local Git repository directory accessible to the worker"`
	// NoLocalRepository is a fact about the client's filesystem, not a claim
	// about what this server can reach: the directory the source came from is
	// in no repository, so there is nothing there to clone whatever the
	// provider could see. Delivery stays the server's decision; this is one of
	// its inputs.
	NoLocalRepository bool `json:"noLocalRepository,omitempty" doc:"Whether localDirectory holds no Git repository at all, so nothing can be cloned from it however reachable it is. The client resolved this source from a repository of its own and can only deliver it by push. localDirectory still records the directory the source came from."`
	// NoLocalCommits is the same kind of fact for a repository that exists and
	// has no commits: `git init` and nothing since. A clone of it yields
	// nothing, and the empty base commit the source was resolved against lives
	// only in objects the client wrote, so this source is a push too
	// (ADR 0083). It is deliberately not NoLocalRepository, which additionally
	// means the commits are gone forever with the repository built for that one
	// run; these are in the user's own repository and stay there.
	NoLocalCommits bool `json:"noLocalCommits,omitempty" doc:"Whether the repository at localDirectory has no commits, so a clone of it yields nothing and the base commit this source was resolved against exists only as objects the client holds. The client can only deliver it by push, and can still deliver it later."`
	// NoLocalGitDirectory is the third fact of the kind: the repository has no
	// Git directory in place at localDirectory/.git. A linked worktree or a
	// submodule checkout puts a file there naming one elsewhere, and a
	// clone-delivered source's origin is exactly that directory (ADR 0093), so
	// there is nothing a sandbox can be bound to. Its commits stay in the user's
	// repository, as NoLocalCommits' do.
	NoLocalGitDirectory bool `json:"noLocalGitDirectory,omitempty" doc:"Whether localDirectory has no Git directory of its own at localDirectory/.git — a linked worktree or submodule checkout, whose .git is a file naming a Git directory elsewhere — so there is nothing at that path a sandbox's origin can be bound to. The client can only deliver it by push, and can still deliver it later."`
	// UpstreamURL is a local source's remote as its client reported it: the
	// remote its checked-out branch tracks, else origin, else its only
	// remote. A cache-delivered source has it as origin in the sandbox and is
	// keyed by it on the pool (ADR 26-10-10-545 §§2, 6); a source delivered
	// before that has it as a remote named upstream.
	UpstreamURL *string               `json:"upstreamUrl,omitempty" doc:"Network URL of a local source's remote on the client. It is the remote its checked-out branch tracks, else origin, else its only remote. A cache-delivered source has it as the sandbox's origin remote, and it names the pool cache the source is delivered into. A source delivered before that has it as a remote named upstream."`
	Checkout    *GitSourceCheckout    `json:"checkout,omitempty" doc:"Immutable checkout target and optional user-facing ref identity"`
	Workspace   *GitSourceWorkspace   `json:"workspace,omitempty" doc:"Workspace materialization mode for this source"`
	Destination *GitSourceDestination `json:"destination,omitempty" doc:"Sandbox destination paths for this source"`
	// Purpose is what the source is in the sandbox for (ADR 26-10-14-294 §2).
	// Empty is GitSourcePurposeWork, which is every source created before it,
	// so their manifests fingerprint as they did.
	Purpose string `json:"purpose,omitempty" doc:"What the source is in the sandbox for. work, the default, is a source being worked on. skills is a repository whose skills the sandbox links into the harness's skill directories on its first launch, from the directories skillsSources names; it is never the primary source." enum:"work,skills"`
}

// GitSourcePurposeWork and GitSourcePurposeSkills are GitSource.Purpose's
// values (ADR 26-10-14-294 §2). Work is every source a sandbox is worked on
// in, and what an absent purpose means. Skills is a repository whose skills
// the sandbox links into the harness's skill directories, and that is never
// the primary source.
const (
	GitSourcePurposeWork   = "work"
	GitSourcePurposeSkills = "skills"
)

// IsSkills reports whether the source is a skills source.
func (s *GitSource) IsSkills() bool {
	return s != nil && s.Purpose == GitSourcePurposeSkills
}

// Root returns the normalized identity of the source repository, independent of
// the checked out ref: the absolute local repository root for local sources, and
// the canonical remote URL for remote ones. Sandboxes are grouped by this value
// so a caller sitting in a Git repository can list the sandboxes that run
// against it.
func (s *GitSource) Root() string {
	if s == nil {
		return ""
	}
	if s.LocalDirectory != nil {
		if root := strings.TrimSpace(*s.LocalDirectory); root != "" {
			return root
		}
	}
	if s.URL != nil {
		return strings.TrimSpace(*s.URL)
	}
	return ""
}

// GitSourceCheckout identifies the immutable checkout and user-facing ref.
type GitSourceCheckout struct {
	Commit  *string `json:"commit,omitempty" doc:"Immutable commit SHA to materialize"`
	RefName *string `json:"refName,omitempty" doc:"User-facing branch, tag, or ref name to recreate in the sandbox"`
	RefType *string `json:"refType,omitempty" doc:"User-facing ref type, such as branch, tag, or commit"`
}

// GitSourceWorkspace describes clean or dirty workspace materialization.
type GitSourceWorkspace struct {
	Mode        string  `json:"mode,omitempty" doc:"Whether to materialize a clean checkout or restore dirty workspace changes" enum:"clean,dirty"`
	SnapshotRef *string `json:"snapshotRef,omitempty" doc:"Discobox-owned hidden Git ref containing dirty workspace snapshot data"`
	BaseCommit  *string `json:"baseCommit,omitempty" doc:"Commit to checkout before applying dirty workspace snapshot changes"`
}

// GitSourceDestination describes where a source is placed inside the sandbox.
type GitSourceDestination struct {
	Directory        *string `json:"directory,omitempty" doc:"Directory where this source should be placed inside the sandbox"`
	WorkingDirectory *string `json:"workingDirectory,omitempty" doc:"Working directory inside the sandbox for this source"`
}

// SourceCodeReferences maps sandbox destination directories to Git sources.
type SourceCodeReferences map[string]GitSource

// SkillsSource is one directory of a skills source whose skills the sandbox
// links in (ADR 26-10-14-294 §3). Source is the SourceCodeReferences key, and
// a sandbox's skills sources are a list because the references are a map: the
// list is what carries the order the client named them in.
type SkillsSource struct {
	Source    string `json:"source" doc:"The sourceCodeReferences key of the skills source"`
	Directory string `json:"directory" doc:"The directory inside the source's repository that holds the skills, or . for the root"`
}

// Origin is the client host a sandbox was created from.
//
// It is client-declared provenance, recorded verbatim and never used to
// materialize source. It names no directory: where a sandbox belongs on its
// client is its origin key, derived from the host and the primary source
// (SandboxOriginKey, ADR 0111).
type Origin struct {
	HostID   string `json:"hostId" doc:"Stable generated identity of the client host, unique per user per machine"`
	Hostname string `json:"hostname,omitempty" doc:"Client hostname, for display only. Not stable and not unique."`
	User     string `json:"user,omitempty" doc:"Client OS username, for display only"`
}

// SandboxOriginKey is where a sandbox belongs on the client that created it
// (ADR 0111): its origin host and its primary source's root, or the host alone
// when it has no source. It is empty for a sandbox created without an origin.
// Clients derive the same value to filter listings, which is why both sides go
// through originkey.For.
func SandboxOriginKey(origin *Origin, source *GitSource) string {
	if origin == nil {
		return ""
	}
	return originkey.For(origin.HostID, source.Root())
}

// PoolManifest is the pool spec, embedded anonymously in Pool on the same terms
// as SandboxManifest (ADR 0017 §11): the fields the pool's host is built from,
// separated from everything the host reports back.
type PoolManifest struct {
	Name               string `gorm:"column:name;not null;type:text;uniqueIndex:idx_pool_project_name,priority:2" json:"name" doc:"Pool display name" maxLength:"200"`
	ProviderInstanceID string `gorm:"column:provider_instance_id;not null;type:text;index" json:"providerInstanceId" doc:"Backing sandbox provider instance ID. Immutable after create."`
	// The pool's size. A provider that runs one VM per pool sizes that VM from
	// these; a provider declares which it acts on (ProviderDefinition.
	// PoolSizeFields), and the pool service refuses the rest. Zero is unset.
	CPUVCPUs     float64 `gorm:"column:cpu_vcpus;not null;default:0" json:"cpuVcpus" doc:"vCPUs for the pool's VM. Only a provider that sizes a VM per pool accepts it; zero leaves the size to the provider's configuration, then the host."`
	MemoryBytes  int64   `gorm:"column:memory_bytes;not null;default:0" json:"memoryBytes" doc:"Memory for the pool's VM in bytes. Only a provider that sizes a VM per pool accepts it; zero leaves the size to the provider's configuration, then the host."`
	StorageBytes int64   `gorm:"column:storage_bytes;not null;default:0" json:"storageBytes" doc:"Storage for the pool in bytes. Only a provider that acts on it accepts it, and none does today; zero is unset."`
	// DisableAgentEndpointTLS has the pool reach its sandboxes' agents over
	// plain HTTP rather than TLS its CA issues (ADR 26-10-29-481 §3). It is
	// off for a pool made now, and an upgrade turned it on for every pool made
	// before, whose sandboxes were made with plain HTTP. It is in the manifest:
	// the pool's agent is told it in its bootstrap, so a change places the
	// pool's host again.
	DisableAgentEndpointTLS bool `gorm:"column:disable_agent_endpoint_tls;not null;default:false" json:"disableAgentEndpointTls" doc:"Reach the pool's sandboxes' agents over plain HTTP instead of TLS the pool's CA issues. Off for a pool made now; on for every pool made before TLS was the default. A change reaches a sandbox when its box is next made - a VM sandbox at its next start, a container when it is replaced."`
}

// PoolHostCredential is a credential of its provider's that a pool's host
// holds, as the pool shows it: never its secret.
type PoolHostCredential struct {
	// Key is the provisioning key the host holds: PoolHostKeyIssued, or
	// PoolHostKeyProvider. Empty is none.
	Key string `json:"key,omitempty" enum:"issued,provider"`
	// Scope is what the key reaches, as the driver says (account, org,
	// project); empty where it cannot tell.
	Scope string `json:"scope,omitempty"`
	// KeyID is the driver's ID for an issued key.
	KeyID     string     `json:"keyId,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// MachineToken is the host also carrying a credential its driver gives
	// every machine that is not isolated (boxd's in-VM token, which acts on
	// the org as the provider's user). discobox sets no expiry on it and
	// never renews or revokes it.
	MachineToken bool `json:"machineToken,omitempty"`
}

const (
	// PoolHostKeyIssued is a key the provider's driver issued for the pool,
	// which expires and is renewed and revoked by discobox.
	PoolHostKeyIssued = "issued"
	// PoolHostKeyProvider is the provider's own key, delivered because its
	// configuration opts in: it has no expiry discobox sets, and is never
	// renewed or revoked by discobox.
	PoolHostKeyProvider = "provider"
)

// Pool is the user-visible sharing boundary sandboxes are scheduled into,
// and its own runtime host (ADR-0006).
//
// Sandboxes in the same pool share a cache volume, the pool's CPU and memory,
// and a weaker isolation boundary (same kernel/host); cross-tenant or mutually
// untrusted work belongs in different pools. A pool binds to exactly one
// provider instance at create time, immutably: the provider instance is
// backend identity (type, credentials, connection config), while everything
// about capacity and sharing lives here.
//
// One host per pool is an invariant: the pool row carries the runtime
// lifecycle directly — registration identity, scheduling flags, reported
// capacity, heartbeat, and provider runtime state. Recovery replaces the
// runtime in place under the same pool identity; pool-local state survives in
// named volumes.
type Pool struct {
	ID           string `gorm:"primaryKey;type:text" json:"id" doc:"Stable pool ID"`
	ProjectID    string `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_pool_project_name,priority:1" json:"projectId" doc:"Project ID"`
	PoolManifest `gorm:"embedded"`
	// Egress is the pool's level of its discoboxes' egress policy
	// (ADR 26-10-01-032). It is not in the manifest: a policy change converges
	// at the proxy and must not rebuild the pool host.
	Egress egress.Policy `gorm:"column:egress;type:text;serializer:json" json:"egress" doc:"The pool's egress policy level"`

	// Runtime host state, reported by the pool agent and the provider.
	//
	// Platform is the one platform the pool hosts (ADR 0145 §1), declared by
	// its agent at registration and on every status report. It is empty until
	// the agent first declares it.
	Platform platform.Platform `gorm:"column:platform;not null;type:text;default:''" json:"platform,omitzero" doc:"The one platform the pool hosts, as os/arch, declared by its agent. Empty until the agent first reports."`
	// ImageKind is the kind of image the pool runs (ADR 26-10-09-106 §4):
	// OCI for a Docker pool, a VM driver's for a vm pool. Its agent
	// declares it beside the platform; an agent from before kinds declares
	// none and runs OCI images, which is what is recorded for it. Empty until
	// the agent first reports.
	ImageKind             platform.ImageKind `gorm:"column:image_kind;not null;type:text;default:''" json:"imageKind,omitzero" doc:"The kind of image the pool runs: oci, or vm/<driver> for VM images of that driver. Declared by its agent; empty until the agent first reports."`
	PublicKey             string             `gorm:"column:public_key;type:text" json:"publicKey,omitempty" doc:"Pool agent public key"`
	KeyType               string             `gorm:"column:key_type;type:text;default:'ed25519'" json:"keyType,omitempty" doc:"Pool agent key type"`
	Ready                 bool               `gorm:"column:ready;not null;default:false;index" json:"ready" doc:"Whether the pool host is alive and healthy"`
	Schedulable           bool               `gorm:"column:schedulable;not null;default:false;index" json:"schedulable" doc:"Whether the pool accepts new sandboxes"`
	Degraded              bool               `gorm:"column:degraded;not null;default:false;index" json:"degraded" doc:"Whether the pool should be used only as fallback capacity"`
	AvailableCPUVCPUs     float64            `gorm:"column:available_cpu_vcpus;not null;default:0" json:"availableCpuVcpus" doc:"Agent-reported available CPU capacity in vCPUs"`
	AvailableMemoryBytes  int64              `gorm:"column:available_memory_bytes;not null;default:0" json:"availableMemoryBytes" doc:"Agent-reported available memory capacity in bytes"`
	AvailableStorageBytes int64              `gorm:"column:available_storage_bytes;not null;default:0" json:"availableStorageBytes" doc:"Agent-reported available storage capacity in bytes"`
	Conditions            json.RawMessage    `gorm:"column:conditions;type:text" json:"conditions,omitempty" doc:"Opaque agent-reported condition details for display"`
	// ProvisionProgress is what the provider driver is doing to bring this host
	// up, for a client whose sandbox is waiting for a pool to take it. Unlike
	// Conditions it is not agent-reported: the phases it names — fetching a VM
	// image, booting the machine, pulling the pool's image — all happen
	// before there is an agent to report anything.
	ProvisionProgress   json.RawMessage `gorm:"column:provision_progress;type:text" json:"provisionProgress,omitempty" doc:"Latest provisioning progress reported by the provider driver, such as a VM image fetch or an image pull in flight"`
	ProvisionProgressAt *time.Time      `gorm:"column:provision_progress_at" json:"provisionProgressAt,omitempty" doc:"When ProvisionProgress was observed" format:"date-time"`
	// Resources is what this pool is consuming, reported by its agent every
	// resource-report interval (ADR 0071, resource accounting). It is the pool's own totals plus its
	// disk, and it is telemetry rather than a scheduling input: what placement
	// reads is the Available* fields above.
	//
	// Overhead — the load that is not any sandbox — is `cpu.vcpus` here minus
	// the sum of the sandboxes' own, and is derived by whoever reads it rather
	// than stored, so it can never disagree with its operands.
	Resources           json.RawMessage `gorm:"column:resources;type:text" json:"resources,omitempty" doc:"Latest pool-wide CPU, memory and disk consumption reported by the pool agent. Telemetry, not a scheduling input."`
	ResourcesReportedAt *time.Time      `gorm:"column:resources_reported_at" json:"resourcesReportedAt,omitempty" doc:"When Resources was reported" format:"date-time"`
	RuntimeState        json.RawMessage `gorm:"column:runtime_state;type:text" json:"-" doc:"Internal provider runtime state; may contain boot material and must not be serialized"`
	// HostCredential is the provider credential the pool's host holds, which
	// its provider writes when it places one (ADR 26-10-23-551 §4, 0126's
	// Consequence): a pool whose host can reach the provider's account says
	// so, and how far. Nil is a host that holds none.
	HostCredential    *PoolHostCredential `gorm:"column:host_credential;type:text;serializer:json" json:"hostCredential,omitempty" doc:"The provider credential the pool's host holds, and what it reaches. Absent when it holds none."`
	ResourceLifecycle `gorm:"embedded"`
	RegisteredAt      *time.Time `gorm:"column:registered_at" json:"registeredAt,omitempty" doc:"Registration timestamp" format:"date-time"`
	LastSeenAt        *time.Time `gorm:"column:last_seen_at;index" json:"lastSeenAt,omitempty" doc:"Last heartbeat timestamp" format:"date-time"`

	// Startup invalidates health without discarding identity or the last report.
	HealthCheckStartedAt *time.Time `gorm:"column:health_check_started_at" json:"-"`
	// Only a status heartbeat stamps this; registration only stamps LastSeenAt.
	StatusReportedAt *time.Time `gorm:"column:status_reported_at" json:"-"`
	// When the runtime reconciler last settled an attempt, independent of telemetry.
	ReconciledAt *time.Time `gorm:"column:reconciled_at" json:"-"`

	RevokedAt *time.Time `gorm:"column:revoked_at;index" json:"revokedAt,omitempty" doc:"Revocation timestamp" format:"date-time"`
	CreatedAt time.Time  `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time  `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project          *Project                 `gorm:"foreignKey:ProjectID" json:"-"`
	ProviderInstance *SandboxProviderInstance `gorm:"foreignKey:ProviderInstanceID" json:"providerInstance,omitempty" doc:"Backing sandbox provider instance"`
	Sandboxes        []Sandbox                `gorm:"foreignKey:PoolID" json:"-"`
}

func (Pool) TableName() string { return "pools" }

func (p *Pool) BeforeCreate(_ *gorm.DB) error {
	if p.ID == "" {
		var err error
		p.ID, err = id.New(id.PrefixPool)
		if err != nil {
			return err
		}
	}
	p.SetDefaults(DesiredStatePresent, PoolStatePending)
	if p.KeyType == "" {
		p.KeyType = "ed25519"
	}
	return nil
}

// EverCreated reports whether the pool's runtime completed its initial create
// and registered at least once. Such pools are stateful (they own runtime
// volumes and assigned sandboxes), so a failed reconcile must be driven back
// to health rather than latched to a terminal failure. Only a pool whose
// runtime never registered may fail terminally.
func (p *Pool) EverCreated() bool {
	return p != nil && p.RegisteredAt != nil
}

// SandboxManifest is the sandbox spec: everything the container is built from.
// It is embedded anonymously in Sandbox, so it is flat in the database and flat
// on the wire — the split is a statement about ownership, not a nesting change
// (ADR 0017 §11).
//
// Membership has one test: **does changing this field require rebuilding the
// container?** Everything here answers yes, which is what makes the fingerprint
// over this struct a sound drift check. Fields that answer no — Name,
// Description, Origin, the derived index columns, and every
// observed or runtime field — belong on Sandbox instead.
//
// Model, ModelServiceTier, ModelReasoningLevel, and Prompt are here because
// they are handed to the runtime at create and baked into the container the
// harness launches from, so a change to any of them only takes effect on a
// rebuild. That they read as session parameters rather than infrastructure does
// not change where they are consumed.
//
// Adding a field here puts it in the fingerprint automatically. That is the
// entire reason the struct exists: a hand-maintained list of spec fields rots
// silently, and the symptom is a container that stops being rebuilt for a
// change that should rebuild it.
type SandboxManifest struct {
	HarnessConfigID     *string  `gorm:"column:harness_config_id;type:text;index" json:"harnessConfigId,omitempty" doc:"Harness config ID"`
	HarnessMode         string   `gorm:"column:harness_mode;not null;type:text;default:'run'" json:"harnessMode,omitempty" doc:"Harness startup mode: run or config"`
	Model               *string  `gorm:"column:model;type:text" json:"model,omitempty" doc:"Model the harness should use"`
	ModelServiceTier    *string  `gorm:"column:model_service_tier;type:text" json:"modelServiceTier,omitempty" doc:"Model service tier the harness should use"`
	ModelReasoningLevel *string  `gorm:"column:model_reasoning_level;type:text" json:"modelReasoningLevel,omitempty" doc:"Model reasoning level the harness should use"`
	Prompt              []string `gorm:"column:prompt;type:text;serializer:json" json:"prompt,omitempty" doc:"Prompt the harness should run, passed as argv to preserve the caller's exact tokens"`
	// Image and ImageDigest are the manifest file of the template with no
	// image a sandbox on such a harness runs (ADR 0145 §3), and empty on a
	// sandbox on a harness bundle, which runs its provider's image with its
	// bundles applied (ADR 26-10-16-423 §10). A sandbox from before bundles
	// carried its runnable harness image here, until it was pinned to its
	// bundles (sandboxes.Service.PinUndeliveredSandboxes).
	Image       string `gorm:"column:image;type:text" json:"image,omitempty" doc:"Manifest file of the template with no image this sandbox runs, when its harness is one"`
	ImageDigest string `gorm:"column:image_digest;not null;type:text;default:''" json:"imageDigest,omitempty" doc:"Digest of the manifest file this sandbox is pinned to. Written at create and by an upgrade; the pool host rebuilds any container whose spec fingerprint does not match."`
	// The bundles a box applies (ADR 26-10-16-423 §§7, 13): its harness
	// config's, and the platform its server's release names, each pinned by
	// digest — at create, by an upgrade or a repair, and for a stopped sandbox
	// when what they were taken from moves — and are what the pool lets the
	// box fetch. Omitted when empty, which keeps the fingerprint of a sandbox
	// on a manifest file what it was before bundles.
	Bundle       string `gorm:"column:bundle;not null;type:text;default:''" json:"bundle,omitempty" doc:"Harness bundle this sandbox is pinned to"`
	BundleDigest string `gorm:"column:bundle_digest;not null;type:text;default:''" json:"bundleDigest,omitempty" doc:"Manifest digest of the harness bundle this sandbox is pinned to"`
	// HarnessManifestDigest is the harness config's ManifestDigest the
	// sandbox was pinned with: the manifest its container is built with,
	// composed from the harness bundle's labels on the platform's layer. It is
	// in both fingerprints, so a platform-layer edit, which moves no bundle
	// digest, rebuilds the container as a moved harness bundle does.
	HarnessManifestDigest string `gorm:"column:harness_manifest_digest;not null;type:text;default:''" json:"harnessManifestDigest,omitempty" doc:"Digest of the composed harness manifest this sandbox's container is built with"`
	PlatformBundle        string `gorm:"column:platform_bundle;not null;type:text;default:''" json:"platformBundle,omitempty" doc:"Platform bundle this sandbox is pinned to"`
	PlatformBundleDigest  string `gorm:"column:platform_bundle_digest;not null;type:text;default:''" json:"platformBundleDigest,omitempty" doc:"Manifest digest of the platform bundle this sandbox is pinned to"`
	// Bundles is the ordered list of bundles a sandbox pinned since ADR
	// 26-10-24-953 applies (§1): the platform, then its harness's when it has
	// one, the order the server composes them in and the box applies them in.
	// Such a sandbox leaves Bundle and PlatformBundle empty; one pinned before
	// keeps them, and has none here, so its fingerprint is what it was. Read
	// and write them through PlatformPin, HarnessPin, BundlePins and SetPins,
	// never by form. Omitted when empty.
	Bundles []BundlePin `gorm:"column:bundles;type:text;serializer:json" json:"bundles,omitempty" doc:"Bundles this sandbox applies, in order"`
	// AgentBundle and AgentBundleDigest are the agent bundle this sandbox's
	// pool places it with (ADR 26-10-24-953 §2), pinned with the platform.
	// Neither fingerprint includes them: the agent is placed at every start,
	// and what it applies is the bundles.
	AgentBundle          string               `gorm:"column:agent_bundle;not null;type:text;default:''" json:"agentBundle,omitempty" doc:"Agent bundle this sandbox's pool places it with"`
	AgentBundleDigest    string               `gorm:"column:agent_bundle_digest;not null;type:text;default:''" json:"agentBundleDigest,omitempty" doc:"Manifest digest of the agent bundle this sandbox is pinned to"`
	Env                  map[string]string    `gorm:"column:env;type:text;serializer:json" json:"env,omitempty" doc:"Environment variables available to sandbox-agent terminals and execs by default"`
	Source               *GitSource           `gorm:"column:source;type:text;serializer:json" json:"source,omitempty" doc:"Primary Git source to materialize in the sandbox"`
	SourceCodeReferences SourceCodeReferences `gorm:"column:source_code_references;type:text;serializer:json" json:"sourceCodeReferences,omitempty" doc:"Additional Git sources to materialize in the sandbox"`
	UserName             *string              `gorm:"column:user_name;type:text" json:"userName,omitempty" doc:"Username to use inside the sandbox"`
	UserUID              *int                 `gorm:"column:user_uid" json:"userUid,omitempty" doc:"UID to use inside the sandbox"`
	UserGID              *int                 `gorm:"column:user_gid" json:"userGid,omitempty" doc:"Primary group ID to use inside the sandbox"`
	UserGroupName        *string              `gorm:"column:user_group_name;type:text" json:"userGroupName,omitempty" doc:"Primary group name to use inside the sandbox, resolved inside it"`
	UserAdditionalGroups []string             `gorm:"column:user_additional_groups;type:text;serializer:json" json:"userAdditionalGroups,omitempty" doc:"Supplementary groups, each a group name or a numeric GID"`
	HomeDirectory        *string              `gorm:"column:home_directory;type:text" json:"homeDirectory,omitempty" doc:"User home directory to use inside the sandbox"`
	GitUserName          *string              `gorm:"column:git_user_name;type:text" json:"gitUserName,omitempty" doc:"Value for git's user.name inside the sandbox"`
	GitUserEmail         *string              `gorm:"column:git_user_email;type:text" json:"gitUserEmail,omitempty" doc:"Value for git's user.email inside the sandbox"`
	// Skills ride the sandbox's bootstrap, so they are spec: fixed at create
	// and part of the fingerprint. Omitted when empty, which keeps the
	// fingerprint of every sandbox created without them what it was before
	// they existed (ADR 26-10-09-395 §2).
	Skills sandboxconfig.Skills `gorm:"column:skills;type:text;serializer:json" json:"skills,omitempty" doc:"Skills installed into the harness's skill directories on the sandbox's first launch"`
	// SkillsSources are spec for the same reason, and omitted when empty for
	// the same reason (ADR 26-10-14-294 §2).
	SkillsSources []SkillsSource `gorm:"column:skills_sources;type:text;serializer:json" json:"skillsSources,omitempty" doc:"Directories of skills sources whose skills are linked into the harness's skill directories on the sandbox's first launch, in order"`
	// Features ride the bootstrap too, and are omitted when empty for the
	// same reason (ADR 26-10-16-423 §§4, 11).
	Features map[string]sandboxconfig.Feature `gorm:"column:features;type:text;serializer:json" json:"features,omitempty" doc:"The repository's features by name, as its settings.json set them at create; the sandbox provisions every one but those turned off"`
}

// Fingerprint is the spec digest the runtime compares a container against
// (ADR 0017 §5). It is recorded as a container label at build time; a container
// whose label no longer matches was built from a different spec and is rebuilt.
//
// Canonical JSON of the manifest is the input, so the digest is stable across
// processes and changes exactly when a spec field changes.
//
// Sandbox secret assignments are intentionally not part of the fingerprint:
// they are sentinels injected at launch, not spec, and folding them in would
// rebuild the container whenever an assignment changes. The corollary is that
// a sandbox must never be launched before its assignments are readable —
// create commits them atomically with the sandbox row and the dirty mark
// (createSandboxIntent), and the reconciler fails rather than launching when
// it cannot read them.
func (m SandboxManifest) Fingerprint() string {
	m.AgentBundle, m.AgentBundleDigest = "", ""
	encoded, err := json.Marshal(m)
	if err != nil {
		// Marshaling a manifest cannot fail for the types it holds. If it ever
		// does, a fingerprint that matches nothing is the safe answer: it forces
		// a rebuild rather than silently accepting a stale container.
		return "unfingerprintable"
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// DeliveredFingerprint is the spec digest of a sandbox its pool delivers the
// platform to (ADR 26-10-16-423 §7, as amended): Fingerprint without the
// pins such a container is not built from. It runs the provider's sandbox
// base image and applies its bundles at every start, so a moved pin restarts
// it rather than rebuilding it, and it keeps its content store and what it
// provisioned (ADR 26-10-24-953 §1). What its container is built with is the
// composed harness manifest, whose digest stays in, so a manifest-layer edit
// rebuilds it. A sandbox pinned before that ADR keeps its harness bundle pin
// in, as it was, so a moved one rebuilds its container still: dropping it
// would rebuild every such container once. The image pair is left out too,
// which a sandbox from before bundles still carries until it is pinned to
// them.
//
// A field added to SandboxManifest is in it, as in Fingerprint, unless it is
// another pin a box applies at its start.
func (m SandboxManifest) DeliveredFingerprint() string {
	m.Image, m.ImageDigest = "", ""
	m.PlatformBundle, m.PlatformBundleDigest = "", ""
	m.Bundles = nil
	m.AgentBundle, m.AgentBundleDigest = "", ""
	encoded, err := json.Marshal(struct {
		Delivered SandboxManifest `json:"delivered"`
	}{m})
	if err != nil {
		return "unfingerprintable"
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// BundlePin is a bundle by reference and the manifest digest it is pinned to
// (ADR 26-10-16-423 §§7, 13).
type BundlePin struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// Name is the name the asset API addresses the bundle by.
func (p BundlePin) Name() string { return BundleName(p.Reference) }

// BundleName is the name a bundle is addressed by: the last component of its
// reference's repository, so ghcr.io/discobox-ai/discobox-platform:v1 and a
// development build's discobox-platform:dev-<digest> are both
// discobox-platform.
// Empty for a reference that does not parse.
func BundleName(ref string) string {
	parsed, err := name.ParseReference(strings.TrimSpace(ref))
	if err != nil {
		return ""
	}
	return path.Base(parsed.Context().RepositoryStr())
}

// pinnedBefore reports a sandbox pinned before ADR 26-10-24-953, which keeps
// its pins in Bundle and PlatformBundle.
func (m SandboxManifest) pinnedBefore() bool {
	return len(m.Bundles) == 0 && (m.PlatformBundleDigest != "" || m.BundleDigest != "")
}

// PlatformPin is the platform bundle the sandbox applies, whichever form it
// is pinned in; zero when it is pinned to none.
func (m SandboxManifest) PlatformPin() BundlePin {
	if m.pinnedBefore() {
		return BundlePin{Reference: m.PlatformBundle, Digest: m.PlatformBundleDigest}
	}
	if len(m.Bundles) > 0 {
		return m.Bundles[0]
	}
	return BundlePin{}
}

// HarnessPin is the harness bundle the sandbox applies after the platform;
// zero for a sandbox on a harness with no bundle.
func (m SandboxManifest) HarnessPin() BundlePin {
	if m.pinnedBefore() {
		return BundlePin{Reference: m.Bundle, Digest: m.BundleDigest}
	}
	if len(m.Bundles) > 1 {
		return m.Bundles[1]
	}
	return BundlePin{}
}

// BundlePins is every bundle the sandbox applies, in order: what its pool is
// sent, and what keeps their blobs in the server's store.
func (m SandboxManifest) BundlePins() []BundlePin {
	var pins []BundlePin
	for _, pin := range []BundlePin{m.PlatformPin(), m.HarnessPin()} {
		if strings.TrimSpace(pin.Reference) != "" {
			pins = append(pins, pin)
		}
	}
	return pins
}

// AgentPin is the agent bundle the sandbox's pool places it with; zero when
// it is pinned to none.
func (m SandboxManifest) AgentPin() BundlePin {
	return BundlePin{Reference: m.AgentBundle, Digest: m.AgentBundleDigest}
}

// SetPins pins the sandbox to platform and then harness, a zero harness
// being none. A sandbox pinned before ADR 26-10-24-953 keeps its form, so its
// fingerprint changes only as its pins do; any other is pinned as the list.
// A harness with no platform — one re-pinned while the release's platform
// cannot be resolved, which no box can be delivered — is kept in the form
// from before the list too, since the list's first pin is the platform.
func (m *SandboxManifest) SetPins(platform, harness BundlePin) {
	if m.pinnedBefore() || (platform.Reference == "" && harness.Reference != "") {
		m.PlatformBundle, m.PlatformBundleDigest = platform.Reference, platform.Digest
		m.Bundle, m.BundleDigest = harness.Reference, harness.Digest
		return
	}
	m.Bundle, m.BundleDigest, m.PlatformBundle, m.PlatformBundleDigest = "", "", "", ""
	m.Bundles = nil
	if platform.Reference != "" {
		m.Bundles = append(m.Bundles, platform)
	}
	if harness.Reference != "" {
		m.Bundles = append(m.Bundles, harness)
	}
}

// SetAgentPin pins the sandbox to the agent bundle pin.
func (m *SandboxManifest) SetAgentPin(pin BundlePin) {
	m.AgentBundle, m.AgentBundleDigest = pin.Reference, pin.Digest
}

// SandboxHarnessChange is a proposed spec, kept separate until the pool has
// reserved the stopped runtime. It survives a server restart (ADR 26-10-24-650).
type SandboxHarnessChange struct {
	ID       string
	Manifest SandboxManifest
}

// Sandbox is the managed runtime/session unit.
type Sandbox struct {
	PendingHarnessConfigID *string               `gorm:"type:text;index" json:"-"`
	PendingHarnessConfig   *HarnessConfig        `gorm:"foreignKey:PendingHarnessConfigID" json:"-"`
	HarnessChangeID        string                `gorm:"type:text;not null;default:''" json:"-"`
	PendingHarnessChange   *SandboxHarnessChange `gorm:"type:text;serializer:json" json:"-"`
	HarnessChangeError     string                `gorm:"type:text;not null;default:''" json:"-"`

	ID              string `gorm:"primaryKey;type:text" json:"id" doc:"Stable sandbox ID"`
	ProjectID       string `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_sandbox_project_name,priority:1" json:"projectId" doc:"Project ID"`
	CreatedByUserID string `gorm:"column:created_by_user_id;not null;type:text;index" json:"createdByUserId" doc:"Creating user ID"`
	// CreatedBySandboxID is the sandbox that created this one, when one did
	// (ADR 26-09-24-630 §1). The sandbox role delivers source only into a sandbox the
	// caller created; nothing else reads it for authority.
	CreatedBySandboxID *string `gorm:"column:created_by_sandbox_id;type:text;index" json:"createdBySandboxId,omitempty" doc:"Sandbox that created this one, when a sandbox did (ADR 26-09-24-630). Immutable after create."`
	PoolID             string  `gorm:"column:pool_id;not null;type:text;index" json:"poolId" doc:"Pool the sandbox is scheduled into. Resolved at create, immutable after."`
	// Platform is what the sandbox runs on (ADR 0145 §1): its pool's, which
	// its harness's image must be published for. A sandbox created before its
	// pool's agent declared a platform, or before platforms were recorded, has
	// none until it is next placed, which gives it the pool's. It is outside SandboxManifest because it is not a spec that
	// can change: a sandbox's tree belongs to its platform, so it is placed only
	// on a pool of the same one, and is never moved off it.
	Platform platform.Platform `gorm:"column:platform;not null;type:text;default:''" json:"platform,omitzero" doc:"Platform the sandbox runs on, as os/arch: its pool's. Absent until its pool has declared one; immutable after."`
	Name     string            `gorm:"column:name;not null;type:text;uniqueIndex:idx_sandbox_project_name,priority:2" json:"name" doc:"Sandbox name, unique within its project" maxLength:"200"`
	// Description is a copy of the sandbox's description, which lives with its
	// tags in the meta file inside the sandbox (ADR 0136). Until the sandbox
	// first reports (MetaObservedAt nil) it holds the description the sandbox
	// was created with, which is what seeds that file.
	Description     *string `gorm:"type:text" json:"description,omitempty" doc:"The sandbox's description as it last reported it, or the one it was created with until it has"`
	SandboxManifest `gorm:"embedded"`
	// SkillNames are the names of the manifest's Skills, written beside them
	// when the row is inserted and never after (skills are fixed at create).
	// Listings read these instead of the skills, which they leave unloaded:
	// up to a megabyte a sandbox is too much to hydrate for a name
	// (ADR 26-10-09-395 §2). Outside the manifest, so no fingerprint moves.
	SkillNames        []string `gorm:"column:skill_names;type:text;serializer:json" json:"skillNames,omitempty" doc:"Names of the skills the sandbox was created with"`
	ResourceLifecycle `gorm:"embedded"`
	// Egress is the discobox's own level of its egress policy
	// (ADR 26-10-01-032). It is not in the manifest, whose fingerprint rebuilds
	// the container: a policy change converges at the pool proxy.
	Egress egress.Policy `gorm:"column:egress;type:text;serializer:json" json:"egress" doc:"The discobox's own egress policy level"`
	// DiscoboxAPI lets the discobox call the discobox API through its pool,
	// in the sandbox role, on the discoboxes it created (ADR 26-10-27-880 §9).
	// Like Egress it has its own setter (store.SetSandboxDiscoboxAPI) and is
	// never written by a whole-row save: a person changes it while the
	// reconciler holds a copy of the row.
	DiscoboxAPI bool `gorm:"column:discobox_api;not null;default:false" json:"discoboxApi,omitempty" doc:"The discobox may call the discobox API through its pool, acting only on the discoboxes it created"`
	// AttachedHarnessConfigIDs are the harness configs, beside its own, whose
	// credentials the discobox was given to run a second harness in it
	// (POST .../harness-secrets): their bindings apply to it, after its own
	// harness config's and before the project's (ADR 26-10-27-880 §2), so a
	// change to them reaches it. Written only by its own setter
	// (store.AttachSandboxHarnessConfig), never by a whole-row save.
	AttachedHarnessConfigIDs []string                        `gorm:"column:attached_harness_config_ids;type:text;serializer:json" json:"-"`
	SourceRoot               *string                         `gorm:"column:source_root;type:text;index" json:"sourceRoot,omitempty" doc:"Normalized repository identity of the primary source: local repository root path, or remote URL. Derived from Source; used to list the sandboxes belonging to a repository."`
	Origin                   *Origin                         `gorm:"column:origin;type:text;serializer:json" json:"origin,omitempty" doc:"Client host the sandbox was created from. Immutable after create."`
	SourceDeliveries         map[string]sourcecache.Delivery `gorm:"column:source_deliveries;type:text;serializer:json" json:"sourceDeliveries,omitempty"`
	SourceDeliveredAt        *time.Time                      `gorm:"column:source_delivered_at" json:"sourceDeliveredAt,omitempty" doc:"When the client reported its push complete for a push-delivered source. Empty while the sandbox is still awaiting it. The commit to check out is the source's Checkout.Commit, fixed at create." format:"date-time"`
	OriginKey                *string                         `gorm:"column:origin_key;type:text;index" json:"-" doc:"Where the sandbox belongs on the client that created it: SandboxOriginKey of its origin and primary source. Indexed; what listings filter on."`
	ProviderState            json.RawMessage                 `gorm:"column:provider_state;type:text" json:"providerState,omitempty" doc:"Non-secret provider state"`
	// RepairGeneration marks one generation as a repair (ADR 0035): when it
	// equals Generation, ensure tears the runtime down (provider Archive:
	// container and disposable state dropped, durable tree kept) before the
	// ordinary create rebuilds it. Naming exactly one generation is what makes
	// repair one-shot: retries within the generation re-run an idempotent
	// teardown, later generations never tear down again.
	//
	// "Retries within the generation" means the repair has not been observed
	// yet. Once ObservedGeneration reaches it the repair has landed, and the
	// ensures that still arrive on that generation are observation-driven --
	// which must not tear anything down.
	RepairGeneration int64 `gorm:"column:repair_generation;not null;default:0" json:"-" doc:"Generation whose ensure rebuilds from a teardown"`
	// WakeAt is the sandbox's one scheduled start: the wake its last confirmed
	// power-down carried, which the sandbox power reconcile sends a start at,
	// once (ADR 26-10-16-337 §6). It is not desired state — nothing retries
	// it — and any power instruction from outside clears it. Null is no wake.
	WakeAt *time.Time `gorm:"column:wake_at" json:"-" doc:"When the sandbox asked to be started again after the power-down it asked for"`
	// WakeEpoch guards WakeAt against a power instruction from outside that
	// lands while the power reconcile is between reading the sandbox and
	// writing it. Every such instruction moves it, wake or no wake; the
	// reconcile's writes are conditional on the value it read.
	WakeEpoch int64 `gorm:"column:wake_epoch;not null;default:0" json:"-" doc:"Moved by every outside power instruction; guards WakeAt"`
	// AutomaticRepair is the last repair nobody typed: the one recorded when an
	// action found this sandbox failed with no container (ADR 26-10-12-711).
	// Its generation is what bounds automatic repair to one. A sandbox that
	// failed on that generation has had its repair, so a second is never
	// recorded for it, and any explicit intent moves the generation on. It stays after the repair lands, because landing clears the failure
	// that explains why the container was rebuilt, and is replaced by the next.
	AutomaticRepair *AutomaticRepair `gorm:"column:automatic_repair;type:text;serializer:json" json:"automaticRepair,omitempty" doc:"The last repair recorded automatically for this sandbox, when one has been"`
	SecretState     []byte           `gorm:"column:secret_state" json:"-"`
	LastActiveAt    *time.Time       `gorm:"column:last_active_at;index" json:"lastActiveAt,omitempty" doc:"Last observed activity timestamp" format:"date-time"`
	// RuntimeState is the power axis: what the container is doing, as observed
	// by the pool agent hosting it. Store.ApplySandboxStateReports is its only
	// writer, and Store.UpdateSandbox omits it so no other path can carry a
	// stale value back over an observation (ADR 0034 §2). Empty means no agent
	// has reported on this sandbox since it last ran: not yet, or not since it
	// was archived, whose next complete sync puts the state down
	// (ADR 26-10-29-316). Neither is the same as `stopped`.
	RuntimeState          string     `gorm:"column:runtime_state;not null;type:text;default:''" json:"runtimeState,omitempty" doc:"Observed power state, reported by the pool agent hosting the sandbox. Empty until one has reported, and again once an archived sandbox's pool has synced without it." enum:"starting,running,stopping,stopped,suspended"`
	RuntimeStateChangedAt *time.Time `gorm:"column:runtime_state_changed_at" json:"runtimeStateChangedAt,omitempty" doc:"When RuntimeState last changed to its current value" format:"date-time"`
	// StateReportedAt and StateReportSeq order the runtime's state reports
	// (ADR 0017 §10). A report older than what is already recorded is ignored,
	// so a delayed transition cannot overwrite a newer complete sync.
	StateReportedAt *time.Time `gorm:"column:state_reported_at" json:"stateReportedAt,omitempty" doc:"When the hosting pool agent last reported this sandbox's state" format:"date-time"`
	StateReportBoot string     `gorm:"column:state_report_boot;not null;type:text;default:''" json:"-" doc:"Boot ID of the pool agent that produced the recorded state report"`
	StateReportSeq  int64      `gorm:"column:state_report_seq;not null;default:0" json:"-" doc:"Sequence number of the recorded state report within its reporting agent's boot"`
	// AgentStatus and AgentStatusObservedAt are the sandbox-agent-computed git
	// status, harness session state, and connection counts pushed periodically
	// by the hosting pool-agent (ADR 0030) — a distinct channel from
	// StateReportedAt above, which is pool-agent's own observed container power
	// state (ADR 0017 §10).
	AgentStatus           json.RawMessage `gorm:"column:agent_status;type:text" json:"agentStatus,omitempty" doc:"Latest sandbox-agent-reported git/session/connection status, pushed periodically by the hosting pool-agent"`
	AgentStatusObservedAt *time.Time      `gorm:"column:agent_status_observed_at" json:"agentStatusObservedAt,omitempty" doc:"When AgentStatus was observed by sandbox-agent" format:"date-time"`
	// ProvisionProgress is work underway on a sandbox that has no state
	// transition to announce it — an image pull, above all (ADR 0039). It is an
	// observation like the two channels above, reported by the hosting
	// pool-agent, and it exists to be read by a client waiting to attach.
	//
	// It is transient by nature: a pull that finished is history, and the
	// record keeps only the last report rather than a series.
	ProvisionProgress   json.RawMessage `gorm:"column:provision_progress;type:text" json:"provisionProgress,omitempty" doc:"Latest provisioning progress reported by the hosting pool-agent, such as an image pull in flight"`
	ProvisionProgressAt *time.Time      `gorm:"column:provision_progress_at" json:"provisionProgressAt,omitempty" doc:"When ProvisionProgress was observed" format:"date-time"`
	// Resources is this sandbox's CPU, memory and disk consumption, computed by
	// the hosting pool agent and pushed on its own channel (ADR 0071, resource accounting) — a third
	// observation channel alongside AgentStatus above and RuntimeState.
	//
	// The rates in it are comparable with every other sandbox in the same pool
	// and only with those: one agent polls all of its own sandboxes on one tick
	// and differences them over that same window, while another pool's agent
	// ticks on its own schedule.
	Resources           json.RawMessage `gorm:"column:resources;type:text" json:"resources,omitempty" doc:"Latest CPU, memory and disk consumption for this sandbox, computed by the hosting pool agent"`
	ResourcesObservedAt *time.Time      `gorm:"column:resources_observed_at" json:"resourcesObservedAt,omitempty" doc:"When Resources was observed" format:"date-time"`
	// Tags are a copy of the sandbox's tags, which live with its description
	// in the meta file inside the sandbox (ADR 0136). The copy is what a
	// listing filters on and what a stopped sandbox is read as; the file is
	// what is true. Two paths write the copy — the status report, and a meta
	// write the sandbox has answered — and MetaObservedAt orders them: both are
	// stamped on the sandbox's own clock, and only a newer observation replaces
	// an older one, so a status poll read before a write cannot land after it
	// and undo it.
	Tags map[string]string `gorm:"column:tags;type:text;serializer:json" json:"tags,omitempty" doc:"The sandbox's tags as it last reported them"`
	// MetaName is a copy of the name in that same file (ADR 26-10-12-026):
	// what a person or the agent calls the sandbox, and what a listing shows
	// for it when set. It is not Name, the generated handle the sandbox was
	// created with, which a rename leaves alone. Nil on a sandbox nobody has
	// named, which is every row from before the column.
	MetaName       *string    `gorm:"column:meta_name;type:text" json:"metaName,omitempty" doc:"The name in the sandbox's meta as it last reported it"`
	MetaObservedAt *time.Time `gorm:"column:meta_observed_at" json:"metaObservedAt,omitempty" doc:"When the sandbox read or wrote MetaName, Description and Tags, on its own clock" format:"date-time"`
	// EventSeq is the Seq of the sandbox's latest SandboxEvent, which the next
	// change is compared with and retention always keeps (ADR 26-10-29-316
	// §§2, 4). Zero is a sandbox with no event yet. Only the store's event
	// recorder writes it, and UpdateSandbox omits it.
	EventSeq  int64     `gorm:"column:event_seq;not null;default:0;index" json:"-"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project       *Project       `gorm:"foreignKey:ProjectID" json:"-"`
	CreatedBy     *User          `gorm:"-" json:"createdBy,omitempty" doc:"Creating user"`
	Pool          *Pool          `gorm:"foreignKey:PoolID" json:"pool,omitempty" doc:"Pool the sandbox is scheduled into"`
	HarnessConfig *HarnessConfig `gorm:"foreignKey:HarnessConfigID" json:"harnessConfig,omitempty" doc:"Harness config"`
}

func (Sandbox) TableName() string { return "sandboxes" }

// AutomaticRepair records one automatic repair of a sandbox
// (ADR 26-10-12-711 §4).
type AutomaticRepair struct {
	// Generation is the repair's own: the generation its intent recorded.
	Generation int64     `json:"generation" doc:"Generation the repair was recorded as"`
	StartedAt  time.Time `json:"startedAt" doc:"When the repair was recorded" format:"date-time"`
	// Trigger names the use that found the sandbox without a container, in
	// words a listing can show: "exec attach", "ssh", "start".
	Trigger string `json:"trigger" doc:"The use of the sandbox that triggered the repair"`
	// Failure is the failure the repair answers: what the sandbox had settled
	// on when it was used.
	Failure string `json:"failure,omitempty" doc:"The failure the repair answers"`
	// LandedAt is when the repair's rebuild converged, and is nil for one that
	// is still running or that failed. It is what tells a failure the repair
	// produced from one that came after it: the record outlives the repair,
	// and a sandbox can fail again on the same generation, when a container
	// lost later cannot be rebuilt.
	LandedAt *time.Time `json:"landedAt,omitempty" doc:"When the repair landed. Absent while it runs, and when it failed." format:"date-time"`
}

// AutomaticRepairAllowed reports whether using this sandbox, found to have no
// container, may record an automatic repair: it is meant to exist, it has
// settled as failed, and that failure is not an automatic repair's own
// (ADR 26-10-12-711 §4). Whether it has no container is the pool's to say and
// is not read here.
func (s *Sandbox) AutomaticRepairAllowed() bool {
	if s.DesiredState != DesiredStatePresent || !s.Converged() {
		return false
	}
	if s.State != SandboxStateFailed && s.ErrorMessage == nil {
		return false
	}
	return !s.AutomaticRepairSpent()
}

// AutomaticRepairSpent reports whether the sandbox's current generation is an
// automatic repair that has not landed: one in flight, or one that failed. A
// repair that landed is spent on nothing — a failure after it is a new one,
// and gets its own.
func (s *Sandbox) AutomaticRepairSpent() bool {
	return s.AutomaticRepair != nil && s.AutomaticRepair.Generation == s.Generation && s.AutomaticRepair.LandedAt == nil
}

// AutomaticRepairLandedSince reports whether an automatic repair of the sandbox
// landed at or after t: the repair a use that began at t caused, or waited on.
func (s *Sandbox) AutomaticRepairLandedSince(t time.Time) bool {
	repair := s.AutomaticRepair
	return repair != nil && repair.LandedAt != nil && !repair.LandedAt.Before(t)
}

// Repairing reports whether a repair of this sandbox, typed or automatic, has
// been recorded and has not landed yet.
func (s *Sandbox) Repairing() bool {
	return s.DesiredState == DesiredStatePresent && s.RepairGeneration != 0 &&
		s.RepairGeneration == s.Generation && !s.Converged()
}

// SetRuntimeState records an observation of what the container is doing,
// stamping RuntimeStateChangedAt only on an actual change.
//
// It mirrors SetState on the other axis, and for the same reason: a complete
// sync repeats what we already knew every 60 seconds, and restamping the anchor
// each time would make it useless for "how long has this been running".
func (s *Sandbox) SetRuntimeState(state string) {
	if s.RuntimeState == state {
		return
	}
	s.RuntimeState = state
	now := time.Now().UTC()
	s.RuntimeStateChangedAt = &now
}

func (s *Sandbox) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		s.ID, err = id.New(id.PrefixSandbox)
		if err != nil {
			return err
		}
	}
	s.SetDefaults(DesiredStatePresent, SandboxStatePending)
	return nil
}

// SandboxProviderInstance stores configured sandbox provider instances.
//
// Config is for non-secret settings only. Secret material should be encrypted
// into EncryptedConfig or another secret store before this model is persisted.
type SandboxProviderInstance struct {
	ID              string          `gorm:"primaryKey;type:text" json:"id" doc:"Stable provider instance ID"`
	ProjectID       string          `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	Type            string          `gorm:"column:type;not null;type:text;index" json:"type" doc:"Provider type"`
	Name            string          `gorm:"column:name;not null;type:text" json:"name" doc:"Provider display name" maxLength:"200"`
	Config          json.RawMessage `gorm:"column:config;type:text" json:"config,omitempty" doc:"Non-secret provider configuration"`
	EncryptedConfig []byte          `gorm:"column:encrypted_config" json:"-"`
	Disabled        bool            `gorm:"column:disabled;not null;default:false" json:"disabled" doc:"Whether this provider is disabled"`
	CreatedAt       time.Time       `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt       time.Time       `gorm:"autoUpdateTime" json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
	Pools   []Pool   `gorm:"foreignKey:ProviderInstanceID" json:"pools,omitempty" doc:"Pools backed by this provider instance"`
}

func (SandboxProviderInstance) TableName() string { return "sandbox_provider_instances" }

func (p *SandboxProviderInstance) BeforeCreate(_ *gorm.DB) error {
	if p.ID == "" {
		var err error
		p.ID, err = id.New(id.PrefixSandboxProvider)
		if err != nil {
			return err
		}
	}
	return nil
}

const (
	// SecretTypeToken is one opaque string. Not "bearer": the proxy swaps the
	// value into whatever header the sandbox put it in — x-api-key,
	// PRIVATE-TOKEN, Authorization — so naming it after one HTTP scheme names a
	// requirement that does not exist.
	SecretTypeToken = "token"
	// SecretTypeOAuth is a token that rotates: the current access
	// token lives in SecretValue.Token (so the proxy swap is identical to a
	// bearer), while the refresh token, token endpoint, client ID, and access
	// token expiry ride alongside it and never leave the control plane. The
	// server refreshes the access token on resolve when it is near expiry.
	SecretTypeOAuth = "oauth"
	// SecretTypeExchange is fields a person stores, such as an API key, that
	// the server trades at the token endpoint its recipe names for a
	// short-lived token (ADR 26-10-08-452). The token lives in
	// SecretValue.Token and is renewed like an OAuth access token; the stored
	// fields never leave the control plane.
	SecretTypeExchange = "exchange"

	SecretRequestStatusPending  = "pending"
	SecretRequestStatusApproved = "approved"
	SecretRequestStatusDenied   = "denied"

	// SecretRefreshStatusPending and its siblings are where a refresh request
	// stands: asked, answered with a value, or dismissed by a person.
	SecretRefreshStatusPending   = "pending"
	SecretRefreshStatusAnswered  = "answered"
	SecretRefreshStatusDismissed = "dismissed"
	// SecretRefreshCauseStale and SecretRefreshCauseRejected are what opened a
	// refresh request: a resolve of a value past, or nearly past, its
	// lifetime, or an upstream refusing it.
	SecretRefreshCauseStale    = "stale"
	SecretRefreshCauseRejected = "rejected"
	// SecretRefreshViaCommand and its siblings are how a refresh's value was
	// produced, as the answer records it.
	SecretRefreshViaCommand = "command"
	SecretRefreshViaEntered = "entered"
	SecretRefreshViaUpdate  = "update"

	// SecretBindingTargetSandbox and its siblings are what a binding is for
	// (ADR 26-10-27-880 §2): one discobox, every discobox on a harness config,
	// or every discobox in the project. Per variable the narrowest applies.
	SecretBindingTargetSandbox       = "sandbox"
	SecretBindingTargetHarnessConfig = "harnessConfig"
	SecretBindingTargetProject       = "project"

	// SecretPurposeUse and SecretPurposeDelegate are what an agent's request
	// asks a credential for: to use it, delivered in a variable, or to give it
	// to the discoboxes it creates (ADR 26-10-27-880 §3).
	SecretPurposeUse      = "use"
	SecretPurposeDelegate = "delegate"

	// SecretBindingEventCreated and its siblings are what happened to a
	// binding, as its append-only history records it (ADR 26-10-27-880 §6).
	SecretBindingEventCreated = "created"
	SecretBindingEventUpdated = "updated"
	SecretBindingEventDeleted = "deleted"
	SecretBindingEventExpired = "expired"
	// SecretBindingEventDelivered is a raw binding's value handed to the
	// pool of a discobox it covers, to deliver there (ADR 26-10-27-840 §5):
	// once per value and discobox, Cause naming the discobox.
	SecretBindingEventDelivered = "delivered"
	// SecretBindingActorSystem is the actor of a change nobody made directly:
	// a cascade from a removed parent, an expiry, or the upgrade.
	SecretBindingActorSystem = "system"

	// SecretRejectionReasonUnrefreshable and its siblings are why a credential
	// an upstream refused needs a person rather than another attempt
	// (ADR 0132 §3). They are the control plane's judgment, not the proxy's
	// observation: the proxy reports a rejection, and what is recorded is what
	// this side could or could not do about it.
	//
	// Unrefreshable is a credential with nothing to renew — an API key, or an
	// OAuth secret holding no refresh material. Whatever was stored is what was
	// refused.
	SecretRejectionReasonUnrefreshable = "unrefreshable"
	// SecretRejectionReasonRefreshFailed is an OAuth credential whose renewal
	// was attempted and refused: the refresh token is spent, revoked, or
	// belongs to a session somebody ended elsewhere.
	SecretRejectionReasonRefreshFailed = "refresh-failed"
	// SecretRejectionReasonRejectedAfterRefresh is the one that costs a person
	// the most certainty: the credential renewed, and what came back was
	// refused too.
	SecretRejectionReasonRejectedAfterRefresh = "rejected-after-refresh"
)

// SecretRejectionReasons is every reason a rejection can be recorded under.
var SecretRejectionReasons = []string{
	SecretRejectionReasonUnrefreshable,
	SecretRejectionReasonRefreshFailed,
	SecretRejectionReasonRejectedAfterRefresh,
}

// OAuth token request encodings (SecretValue.TokenRequestEncoding). JSON is the
// empty value because it is what every OAuth secret stored before the field
// existed was refreshed with.
const (
	OAuthTokenRequestJSON = ""
	OAuthTokenRequestForm = "form"
)

// Secret is a project-scoped encrypted credential that can be requested by sandboxes.
type Secret struct {
	ID        string `gorm:"primaryKey;type:text" json:"id" doc:"Stable secret ID"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_secret_project_name,priority:1" json:"projectId" doc:"Project ID"`
	// Name is what a person, a request and a binding name the secret by, so
	// it is unique in its project.
	Name      string `gorm:"column:name;not null;type:text;uniqueIndex:idx_secret_project_name,priority:2" json:"name" doc:"Secret name"`
	Type      string `gorm:"column:type;not null;type:text" json:"type" doc:"Secret type" enum:"token,oauth,exchange"`
	Anonymous bool   `gorm:"column:anonymous;not null;default:false;index" json:"anonymous,omitempty" doc:"Sandbox-managed secret created from an inline value; referenced only by ID"`
	Format    string `gorm:"column:format;not null;type:text;default:''" json:"format,omitempty" doc:"Generative format template describing the credential shape; used to mint sentinel placeholders"`
	// FormatSet says Format was written by a person rather than read from the
	// value. A value write re-reads the shape only when it is false, so an
	// override survives the value being replaced — by a person, a refresh, or
	// a harness reconfigure.
	FormatSet bool `gorm:"column:format_set;not null;default:false" json:"formatSet,omitempty" doc:"Format was set explicitly rather than read from the value"`
	// OAuth is what an oauth credential is, without being it: where it renews,
	// which client it belongs to, what it may do, and when the access token
	// goes stale. Never the access token, never the refresh token.
	//
	// It is not a column. The fields live inside the encrypted value, because
	// they were captured with it; this is filled in on read, so a caller can
	// see what a credential is good for without the credential.
	OAuth *SecretOAuth `gorm:"-" json:"oauth,omitempty" doc:"What an OAuth credential is; never the tokens themselves"`
	// ExchangeRecipe is how an exchange credential's stored fields are traded
	// for its token. It says where the key is sent, never what the key is, so
	// it is a column rather than part of the sealed value; it is shown on
	// read through Exchange.
	ExchangeRecipe *ExchangeRecipe `gorm:"column:exchange_recipe;type:text;serializer:json" json:"-"`
	// Exchange is the same for an exchange credential: its recipe and when
	// the token goes stale. Filled in on read.
	Exchange *SecretExchange `gorm:"-" json:"exchange,omitempty" doc:"What an exchange credential is; never the stored fields or the token"`
	// MaxBindingTTL is the longest a binding of this credential may last, and
	// the lifetime a binding takes when nobody names one. It is a ceiling
	// rather than a suggestion: a longer binding, or one that never lapses, is
	// refused while it stands. Zero lifts it — bindings may then never lapse,
	// which is what an unlimited credential says out loud.
	//
	// It carries no column default, unlike the rest of the not-null scalars
	// here: GORM omits a zero-valued field from an INSERT when the column has
	// one, so a default would quietly turn "no limit" into an hour. The
	// service owns the default a creator who names none receives.
	MaxBindingTTL  int64  `gorm:"column:max_binding_ttl_seconds;not null" json:"maxBindingTTLSeconds" doc:"Longest a binding of this secret may last, in seconds; 0 lets bindings never lapse"`
	EncryptedValue []byte `gorm:"column:encrypted_value" json:"-"`
	// Revision advances on every write that moves updated_at, in the
	// statement itself. It, not updated_at, is the secret's generation: a
	// coarse clock (Windows) gives two writes in one tick the same
	// timestamp, and a check comparing them would miss the second.
	Revision int64 `gorm:"column:revision;not null;default:1" json:"-"`
	// TTL is how long a token's value is good for after it is written, in
	// seconds (ADR 26-09-25-122 §1). Zero never goes stale, which is every
	// token written before the field existed.
	TTL int64 `gorm:"column:ttl_seconds;not null;default:0" json:"ttlSeconds,omitempty" doc:"How long a value is good for after it is written, in seconds; 0 never goes stale"`
	// RefreshCommand is the argument vector a person's client may run to
	// produce a new value. It is advice to that client: nothing on this side
	// runs it, or checks that a value came from it.
	RefreshCommand []string `gorm:"column:refresh_command;type:text;serializer:json" json:"refreshCommand,omitempty" doc:"Command a person's client may run to produce a new value"`
	// Routes are where the credential may be sent, whoever holds it (ADR
	// 26-10-27-880 §4): every request a sentinel for it is swapped into must
	// be allowed by them, and by its binding's and parent's routes. None sends
	// it nowhere; `*` sends it anywhere (ADR 26-10-27-939).
	Routes []SecretRoute `gorm:"column:routes;type:text;serializer:json" json:"routes,omitempty" doc:"Where the credential may be sent; none sends it nowhere"`
	// AllowRaw says the value itself may ever be handed to a discobox, by a
	// binding a person makes raw (ADR 26-10-27-840 §1). It is part of the
	// furthest the secret may go: no binding is raw unless it is set, and
	// clearing it deletes the raw bindings that stand.
	AllowRaw bool `gorm:"column:allow_raw;not null;default:false" json:"allowRaw,omitempty" doc:"The value itself may be handed to a discobox by a raw binding a person makes; clearing it deletes those bindings"`
	// ValueUpdatedAt is when the current value was written, set by the store
	// on create and on every replaced value.
	ValueUpdatedAt *time.Time `gorm:"column:value_updated_at" json:"valueUpdatedAt,omitempty" doc:"When the current value was written" format:"date-time"`
	// ValueExpiresAt is when the current value goes stale, when that is not
	// TTL after it was written: a value that knew its own lifetime, or one an
	// upstream refused, which is stale from the moment it was refused.
	ValueExpiresAt *time.Time `gorm:"column:value_expires_at" json:"-"`
	// StaleAt is when the current value stops being trusted, derived on read
	// (StaleTime). It is not a column.
	StaleAt   *time.Time `gorm:"-" json:"staleAt,omitempty" doc:"When the current value stops being trusted" format:"date-time"`
	CreatedAt time.Time  `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time  `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (Secret) TableName() string { return "secrets" }

// SecretRoute is one place a secret may be sent: the route shape of
// pkg/secretroute (ADR 26-10-27-880 §4), stored as that package encodes it.
// Hosts are exact names, `*.suffix` for the hosts strictly beneath a suffix,
// or `*` for every host (ADR 26-10-27-939); methods, path globs and a GraphQL
// or MCP block narrow it. A blocked route refuses every request it is the
// most specific route for within its list.
type SecretRoute = secretroute.Route

// ReplaceRoutes sets the secret's routes to routes, in order, normalized.
// Repeats collapse. A route secretroute.Normalize refuses is an error.
// changed reports whether anything moved.
func (s *Secret) ReplaceRoutes(routes []SecretRoute) (changed bool, err error) {
	out, err := NormalizeRoutes(routes)
	if err != nil {
		return false, err
	}
	changed = !SameRoutes(out, s.Routes)
	s.Routes = out
	return changed, nil
}

// NormalizeRoutes is a route list as it is stored: each route normalized
// (secretroute.Normalize), in order, repeats collapsed. A route Normalize
// refuses is an error rather than dropped: whoever wrote it meant somewhere.
func NormalizeRoutes(routes []SecretRoute) ([]SecretRoute, error) {
	var out []SecretRoute
	seen := map[string]bool{}
	for _, route := range routes {
		next, err := secretroute.Normalize(route)
		if err != nil {
			return nil, err
		}
		// A normalized route's canonical form is the route: Format of a
		// normalized route round-trips through Parse.
		key := secretroute.Format(next)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, next)
	}
	return out, nil
}

// OpenRouteHosts are the host patterns the open routes of a list name, in
// order, each once: where a credential held to the list may be sent, as the
// agent credentials protocol speaks of it.
func OpenRouteHosts(routes []SecretRoute) []string {
	var out []string
	for _, route := range routes {
		if route.Blocked {
			continue
		}
		for _, host := range route.Hosts {
			if !slices.Contains(out, host) {
				out = append(out, host)
			}
		}
	}
	return out
}

// SameRouteSet reports whether two normalized route lists hold the same
// routes, in any order.
func SameRouteSet(a, b []SecretRoute) bool {
	key := func(routes []SecretRoute) []string {
		out := make([]string, 0, len(routes))
		for _, route := range routes {
			out = append(out, secretroute.Format(route))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(key(a), key(b))
}

// SameRoutes reports whether two normalized route lists are the same routes
// in the same order.
func SameRoutes(a, b []SecretRoute) bool {
	return slices.EqualFunc(a, b, func(x, y SecretRoute) bool { return secretroute.Format(x) == secretroute.Format(y) })
}

// DefaultRefreshTTL is the lifetime a token takes when it names a refresh
// command and no lifetime: a value worth fetching by command is short-lived by
// assumption (ADR 26-09-25-122 §1).
const DefaultRefreshTTL = 300

// ValueWrittenAt is when the current value was written. Every row has one:
// the store stamps it on create and on each replaced value, and the migration
// backfilled the rows written before it existed. A row somehow without one
// reads its creation, which never moves, rather than its last update, which
// moves on every rename.
func (s *Secret) ValueWrittenAt() time.Time {
	if s.ValueUpdatedAt != nil {
		return *s.ValueUpdatedAt
	}
	return s.CreatedAt
}

// StaleTime is when the current value stops being trusted, and false for one
// that never does. An expiry the value carries wins over the secret's TTL.
func (s *Secret) StaleTime() (time.Time, bool) {
	if s.ValueExpiresAt != nil {
		return *s.ValueExpiresAt, true
	}
	if s.TTL > 0 {
		return s.ValueWrittenAt().Add(time.Duration(s.TTL) * time.Second), true
	}
	return time.Time{}, false
}

// Renewable reports whether a person's client is asked for a new value when
// this one goes stale or is refused: a token that has a lifetime or a command
// to renew it by.
func (s *Secret) Renewable() bool {
	return s.Type == SecretTypeToken && (s.TTL > 0 || len(s.RefreshCommand) > 0)
}

// ValueWritten records that a new value was written at now. expiresAt is the
// value's own expiry, or nil to go stale TTL after now.
func (s *Secret) ValueWritten(now time.Time, expiresAt *time.Time) {
	s.ValueUpdatedAt = &now
	s.ValueExpiresAt = expiresAt
}

func (s *Secret) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		s.ID, err = id.New(id.PrefixSecret)
		if err != nil {
			return err
		}
	}
	return nil
}

// SecretOAuth is the non-secret half of an OAuth credential: what it is, what
// it may do, and when it goes stale. It says nothing that could be used as the
// credential.
type SecretOAuth struct {
	TokenURL             string   `json:"tokenUrl,omitempty" doc:"Where the access token is renewed"`
	ClientID             string   `json:"clientId,omitempty" doc:"OAuth client the grant belongs to"`
	Scopes               []string `json:"scopes,omitempty" doc:"What the grant may do, as captured at login"`
	SubscriptionType     string   `json:"subscriptionType,omitempty" doc:"The plan or account kind the grant belongs to"`
	AccessTokenExpiresAt int64    `json:"accessTokenExpiresAt,omitempty" doc:"When the access token goes stale, unix milliseconds"`
	Refreshable          bool     `json:"refreshable,omitempty" doc:"Whether it carries what it needs to renew itself"`
}

// SecretExchange is the non-secret half of an exchange credential.
type SecretExchange struct {
	Recipe         *ExchangeRecipe `json:"recipe,omitempty" doc:"How the stored fields are exchanged for a token"`
	TokenExpiresAt int64           `json:"tokenExpiresAt,omitempty" doc:"When the current token goes stale, unix milliseconds"`
}

// ExchangeRecipe is a templated token request and a reading of its answer
// (ADR 26-10-08-452): a POST to URL whose body and headers name the stored
// fields as {name}, and the paths to the token and its expiry in the JSON that
// comes back.
type ExchangeRecipe struct {
	URL           string            `json:"url"`
	Form          bool              `json:"form,omitempty"`
	Fields        []string          `json:"fields"`
	Body          map[string]string `json:"body,omitempty"`
	Header        map[string]string `json:"header,omitempty"`
	TokenPath     string            `json:"tokenPath"`
	ExpiresAtPath string            `json:"expiresAtPath,omitempty"`
	ExpiresInPath string            `json:"expiresInPath,omitempty"`
}

// SecretValue holds the type-specific plaintext credential fields.
// Only fields relevant to the secret type will be populated.
type SecretValue struct {
	// Token is the credential: one opaque string, whatever header it ends up
	// in.
	Token string `json:"token,omitempty"`
	// oauth. Token above holds the current access token, so the proxy swap is
	// identical either way; these are used only server-side to refresh it and
	// are never emitted to the pool (the resolve handler sends Token alone).
	RefreshToken         string `json:"refreshToken,omitempty"`
	TokenURL             string `json:"tokenUrl,omitempty"`
	ClientID             string `json:"clientId,omitempty"`
	AccessTokenExpiresAt int64  `json:"accessTokenExpiresAt,omitempty"` // unix milliseconds; 0 means unknown
	// ClientSecret is a confidential client's secret, which must authenticate
	// each refresh as it did the code exchange (RFC 6749 §6). Empty for a
	// public client, which refreshes with its client ID alone. Whoever ran the
	// sign-in supplies it (ADR 26-10-07-005 §4); it is sealed with the rest of
	// the value and, like the refresh token, never leaves the server.
	ClientSecret string `json:"clientSecret,omitempty"`
	// TokenRequestEncoding is how the refresh request's body is encoded:
	// empty for JSON, OAuthTokenRequestForm for application/x-www-form-urlencoded.
	// It is a property of the authorization server, not of the grant, and it is
	// not negotiable after the fact — a refresh token rotates on use, so a
	// request an endpoint rejects for its encoding cannot be retried in the
	// other one without risking a spent token.
	TokenRequestEncoding string `json:"tokenRequestEncoding,omitempty" enum:"form"`
	// Scopes and SubscriptionType describe what the grant is, not what it is —
	// non-secret metadata the authorization server returned alongside the token.
	// They are recorded because a client may gate features on them locally: Claude
	// Code refuses Remote Control unless the credential it reads carries the
	// `user:profile` scope, so a sandbox handed only a token is limited to
	// inference regardless of what the token is actually good for.
	//
	// They are captured at login rather than assumed. Which scopes a login yields
	// depends on the account and the flow, and claiming one the token does not
	// have trades a clear client-side refusal for an opaque upstream 401. A
	// rotation carries them forward untouched — a refresh returns a new token for
	// the same grant, not a new grant.
	Scopes           []string `json:"scopes,omitempty"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	// Exchange is what an exchange secret trades for Token, by the field
	// names its recipe declares. It never leaves the server.
	Exchange map[string]string `json:"exchange,omitempty"`
}

// SecretRequest is a sandbox asking for a binding (ADR 26-10-27-880 §1): the
// approval-inbox item an agent files through the agent credentials protocol,
// naming the credential, the hosts it will send it to, the variable it wants
// it in for a use, and why. The decision is recorded on it, fixed once made;
// approving one makes a binding on the sandbox that asked, which is what was
// approved, since the approver may edit the ask.
type SecretRequest struct {
	ID          string `gorm:"primaryKey;type:text" json:"id" doc:"Stable request ID"`
	ProjectID   string `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	RequestedBy string `gorm:"column:requested_by;not null;type:text" json:"requestedBy" doc:"Principal ID of the requestor"`
	SandboxID   string `gorm:"column:sandbox_id;not null;type:text;default:'';index" json:"sandboxId,omitempty" doc:"The discobox that asked, and the binding's target if approved"`
	// SecretID is the secret approved, set when the request is; Name is the
	// credential the agent asked for, which the approver answers with one.
	SecretID      string        `gorm:"column:secret_id;not null;type:text;default:''" json:"secretId,omitempty" doc:"The secret approved; set when approved"`
	Name          string        `gorm:"column:name;not null;type:text;default:''" json:"name,omitempty" doc:"Credential name the agent asked for"`
	EnvName       string        `gorm:"column:env_name;not null;type:text;default:''" json:"envName,omitempty" doc:"Environment variable the credential is wanted in, for a use"`
	Justification string        `gorm:"column:justification;not null;type:text;default:''" json:"justification,omitempty" doc:"Why the agent says it needs the credential"`
	Routes        []SecretRoute `gorm:"column:routes;type:text;serializer:json" json:"routes,omitempty" doc:"Where the credential is asked to be sent"`
	// TTL is how long the agent asked the binding to last, in seconds. It is
	// what an approval starts from, never what it is held to: the approver
	// chooses the lifetime. Zero is "nothing in particular", not forever.
	TTL int64 `gorm:"column:ttl_seconds;not null;default:0" json:"ttlSeconds,omitempty" doc:"How long the requester asks the binding to last, in seconds"`
	// Purpose is what the agent asked the credential for: to use it, or to
	// give it to the discoboxes it creates (ADR 26-10-27-880 §3).
	Purpose   string     `gorm:"column:purpose;not null;type:text;default:'use'" json:"purpose,omitempty" doc:"What the agent asked the credential for: to use it, or to give it to the discoboxes it creates" enum:"use,delegate"`
	Status    string     `gorm:"column:status;not null;type:text;default:'pending'" json:"status" doc:"Request status" enum:"pending,approved,denied"`
	BindingID string     `gorm:"column:binding_id;not null;type:text;default:''" json:"bindingId,omitempty" doc:"The binding approving the request made"`
	DecidedBy string     `gorm:"column:decided_by;not null;type:text;default:''" json:"decidedBy,omitempty" doc:"Who answered the request"`
	DecidedAt *time.Time `gorm:"column:decided_at" json:"decidedAt,omitempty" doc:"When it was answered" format:"date-time"`
	CreatedAt time.Time  `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time  `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

// Delegates reports whether the request asks to give the credential on rather
// than to use it.
func (r *SecretRequest) Delegates() bool { return r.Purpose == SecretPurposeDelegate }

func (SecretRequest) TableName() string { return "secret_requests" }

func (r *SecretRequest) BeforeCreate(_ *gorm.DB) error {
	if r.ID == "" {
		var err error
		r.ID, err = id.New(id.PrefixSecretRequest)
		if err != nil {
			return err
		}
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	return nil
}

// PrefixSecretRefreshRequest identifies a refresh request. It sits here rather
// than in github.com/discobox-ai/x/id, which carries no prefix for it.
const PrefixSecretRefreshRequest = "sref"

// SecretRefreshRequest asks for a new value of a token the project holds,
// which went stale or was refused (ADR 26-09-25-122 §4). It is not a request
// for access: it is answered by writing a value, or dismissed, and never
// approved. One is pending per secret, which the database holds.
type SecretRefreshRequest struct {
	ID        string `gorm:"primaryKey;type:text" json:"id" doc:"Refresh request ID"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index;uniqueIndex:idx_secret_refresh_open,priority:1,where:status = 'pending'" json:"projectId" doc:"Project ID"`
	SecretID  string `gorm:"column:secret_id;not null;type:text;index;uniqueIndex:idx_secret_refresh_open,priority:2,where:status = 'pending'" json:"secretId" doc:"The secret a new value is asked for"`
	// SecretName is filled in on read, while the secret exists.
	SecretName string               `gorm:"-" json:"secretName,omitempty" doc:"Its name"`
	SandboxID  string               `gorm:"column:sandbox_id;not null;type:text;default:''" json:"sandboxId,omitempty" doc:"The discobox that most recently needed the value"`
	Cause      string               `gorm:"column:cause;not null;type:text" json:"cause" doc:"What opened the request" enum:"stale,rejected"`
	Status     string               `gorm:"column:status;not null;type:text;default:'pending'" json:"status" doc:"Whether it is still asked" enum:"pending,answered,dismissed"`
	Answer     *SecretRefreshAnswer `gorm:"column:answer;type:text;serializer:json" json:"answer,omitempty" doc:"How it was answered, once it was"`
	// CreatedAt and ClosedAt are stamped in UTC: the refresh trail bounds its
	// reads by them, and SQLite compares a time as text carrying its offset.
	CreatedAt time.Time  `gorm:"column:created_at;index" json:"createdAt" doc:"When it was asked" format:"date-time"`
	ClosedAt  *time.Time `gorm:"column:closed_at;index" json:"closedAt,omitempty" doc:"When it was answered or dismissed" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (SecretRefreshRequest) TableName() string { return "secret_refresh_requests" }

func (r *SecretRefreshRequest) BeforeCreate(_ *gorm.DB) error {
	if r.ID == "" {
		var err error
		if r.ID, err = id.New(PrefixSecretRefreshRequest); err != nil {
			return err
		}
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	return nil
}

// SecretRefreshAnswer is how a refresh request was answered: who wrote the
// value, from where, and how the client says it produced it. Never the value.
// Everything but the principal and the time is the client's own account
// (ADR 26-09-25-122 §6).
type SecretRefreshAnswer struct {
	AnsweredAt    time.Time `json:"answeredAt" doc:"When the new value was written" format:"date-time"`
	AnsweredBy    string    `json:"answeredBy,omitempty" doc:"Principal that wrote it"`
	Via           string    `json:"via" doc:"How the value was produced" enum:"command,entered,update"`
	Command       []string  `json:"command,omitempty" doc:"The command the client says it ran"`
	CommandDigest string    `json:"commandDigest,omitempty" doc:"Digest of that command"`
	Session       bool      `json:"session,omitempty" doc:"Whether a session permission answered it"`
	ClientHost    string    `json:"clientHost,omitempty" doc:"Host ID of the client that wrote it"`
}

// SecretRefreshEvent is one thing that happened to a refresh request, as the
// refresh audit trail serves it (ADR 26-09-25-122 §6): it was asked for, and
// later answered with a value or dismissed. A request is two events rather
// than one record because the trail is read forward by time, and a record that
// changed after a reader passed it would never be read again.
type SecretRefreshEvent struct {
	ID           string               `json:"id" doc:"The refresh request's ID"`
	Event        string               `json:"event" doc:"What happened" enum:"asked,answered,dismissed"`
	At           time.Time            `json:"at" doc:"When it happened" format:"date-time"`
	ProjectID    string               `json:"projectId" doc:"Project ID"`
	SecretID     string               `json:"secretId" doc:"Secret a new value was asked for"`
	SecretName   string               `json:"secretName,omitempty" doc:"Its name, while it exists"`
	SandboxID    string               `json:"sandboxId,omitempty" doc:"The discobox that most recently needed the value"`
	RefreshCause string               `json:"refreshCause,omitempty" doc:"What opened the request" enum:"stale,rejected"`
	Answer       *SecretRefreshAnswer `json:"answer,omitempty" doc:"How it was answered, on an answered event"`
}

const (
	SecretRefreshEventAsked     = "asked"
	SecretRefreshEventAnswered  = "answered"
	SecretRefreshEventDismissed = "dismissed"
)

// PrefixSecretBinding identifies a binding. The x/id package names the same
// string for the harness-config binding this replaced; the name here is what
// it is now.
const PrefixSecretBinding = "bind"

// SecretBinding is a secret made available to a target (ADR 26-10-27-880 §1):
// existing and unexpired, it is the whole of the permission. Use delivers the
// credential's sentinel in EnvName to every discobox the target covers;
// Delegate lets those discoboxes give it, to use, to the discoboxes they
// create (§3). Routes narrow the secret's for this binding; a binding handed
// on under a delegation records it as ParentID, is held to its routes and its
// expiry too, and goes with it.
type SecretBinding struct {
	ID         string `gorm:"primaryKey;type:text" json:"id" doc:"Stable binding ID"`
	ProjectID  string `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	SecretID   string `gorm:"column:secret_id;not null;type:text;index;uniqueIndex:idx_secret_binding_delegate,priority:3,where:delegate" json:"secretId" doc:"Bound secret ID"`
	SecretName string `gorm:"-" json:"secretName,omitempty" doc:"Bound secret's name"`
	TargetKind string `gorm:"column:target_kind;not null;type:text;index:idx_secret_binding_target,priority:1;uniqueIndex:idx_secret_binding_use,priority:1,where:use;uniqueIndex:idx_secret_binding_delegate,priority:1,where:delegate" json:"targetKind" doc:"What the binding is for" enum:"project,harnessConfig,sandbox"`
	TargetID   string `gorm:"column:target_id;not null;type:text;index:idx_secret_binding_target,priority:2;uniqueIndex:idx_secret_binding_use,priority:2,where:use;uniqueIndex:idx_secret_binding_delegate,priority:2,where:delegate" json:"targetId" doc:"The project, harness config or discobox ID the binding is for"`
	Use        bool   `gorm:"column:use;not null;default:false" json:"use" doc:"The target may send the credential: it is delivered in envVar"`
	Delegate   bool   `gorm:"column:delegate;not null;default:false" json:"delegate" doc:"A discobox the binding covers may give the credential, to use, to the discoboxes it creates"`
	EnvName    string `gorm:"column:env_name;not null;type:text;default:'';uniqueIndex:idx_secret_binding_use,priority:3,where:use" json:"envVar,omitempty" doc:"Environment variable the credential's sentinel is delivered in; set when use is"`
	// Routes narrow the secret's for this binding; none inherits them.
	Routes []SecretRoute `gorm:"column:routes;type:text;serializer:json" json:"routes,omitempty" doc:"Routes that narrow the secret's for this binding; empty inherits the secret's"`
	// Raw hands the discobox the value itself rather than a sentinel (ADR
	// 26-10-27-840): sandboxconfig.RawDeliveryEnv or RawDeliveryFile, empty
	// for a sentinel. Only a person makes a binding raw, only for a use of a
	// secret that allows it, and never one handed on under a delegation.
	Raw       string     `gorm:"column:raw;not null;type:text;default:''" json:"raw,omitempty" doc:"Hand the discobox the value itself: env puts it in envVar, file puts the path of a file holding it there; empty delivers a sentinel" enum:"env,file"`
	ExpiresAt *time.Time `gorm:"column:expires_at;index" json:"expiresAt,omitempty" doc:"When the binding lapses; absent never" format:"date-time"`
	ParentID  string     `gorm:"column:parent_id;not null;type:text;default:'';index" json:"parentId,omitempty" doc:"The delegate binding this one was handed on under"`
	RequestID string     `gorm:"column:request_id;not null;type:text;default:''" json:"requestId,omitempty" doc:"The request this binding answered, when one did"`
	CreatedBy string     `gorm:"column:created_by;not null;type:text;default:''" json:"createdBy,omitempty" doc:"Principal that made the binding"`
	CreatedAt time.Time  `json:"createdAt" doc:"Creation timestamp" format:"date-time"`
	UpdatedAt time.Time  `json:"updatedAt" doc:"Last update timestamp" format:"date-time"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (SecretBinding) TableName() string { return "secret_bindings" }

func (b *SecretBinding) BeforeCreate(_ *gorm.DB) error {
	if b.ID == "" {
		var err error
		if b.ID, err = id.New(PrefixSecretBinding); err != nil {
			return err
		}
	}
	return nil
}

// Live reports whether the binding stands at now: present and unexpired. A
// binding handed on is live only while its parent is too, which the caller
// checks with the parent.
func (b *SecretBinding) Live(now time.Time) bool {
	return b.ExpiresAt == nil || now.Before(*b.ExpiresAt)
}

// PrefixSecretBindingEvent identifies one change in a binding's history.
const PrefixSecretBindingEvent = "bev"

// SecretBindingEvent is one change to a binding, as the append-only history
// records it (ADR 26-10-27-880 §6): written in the transaction that made the
// change, with who made it, why, and the binding as it then stood. Bindings
// are deleted outright, so this is what says, later, who could send a
// credential where and who allowed it.
type SecretBindingEvent struct {
	ID        string        `gorm:"primaryKey;type:text" json:"id" doc:"Event ID"`
	ProjectID string        `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	BindingID string        `gorm:"column:binding_id;not null;type:text;index" json:"bindingId" doc:"The binding changed"`
	SecretID  string        `gorm:"column:secret_id;not null;type:text;index" json:"-"`
	Event     string        `gorm:"column:event;not null;type:text" json:"event" doc:"What happened; delivered is a raw binding's value reaching the discobox it covers, once per value" enum:"created,updated,deleted,expired,delivered"`
	Actor     string        `gorm:"column:actor;not null;type:text" json:"actor" doc:"Who did it, or system"`
	Cause     string        `gorm:"column:cause;not null;type:text;default:''" json:"cause,omitempty" doc:"Why, when something caused it"`
	Binding   SecretBinding `gorm:"column:binding;type:text;serializer:json" json:"binding" doc:"The binding after the change; before it, for a delete or an expiry"`
	// Seq orders a binding's events among themselves, where At ties.
	Seq int64     `gorm:"column:seq;not null;default:0" json:"-"`
	At  time.Time `gorm:"column:at;not null;index" json:"at" doc:"When it happened" format:"date-time"`
}

func (SecretBindingEvent) TableName() string { return "secret_binding_events" }

func (e *SecretBindingEvent) BeforeCreate(_ *gorm.DB) error {
	if e.ID == "" {
		var err error
		if e.ID, err = id.New(PrefixSecretBindingEvent); err != nil {
			return err
		}
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	return nil
}

// SandboxSecretResolution is the result of resolving a sandbox sentinel: either a
// live grant (value populated) or a pending request awaiting approval.
type SandboxSecretResolution struct {
	Status    string
	Value     *SecretValue
	ExpiresAt *time.Time
	// SecretID is the secret an approved value is, which the pool's proxy
	// records on the request it swapped it into (ADR 26-10-01-240 §1).
	SecretID string
	// BindingID is, for a raw binding's value, the binding it was handed out
	// under (ADR 26-10-27-840).
	BindingID string
	// Revision is the secret's revision the value is of.
	Revision int64
	// ReportScope is the rejection scope the sandbox's harness declares for
	// the variable the sentinel is bound to, which the pool's proxy holds its
	// reports to (ADR 26-10-13-595). Empty is every request.
	ReportScope []HarnessSecretRejectionRule
}

// SandboxInventory is what a sandbox's list reads last answered — its execs,
// services, tools, and each terminal's visible screen — as its sandbox agent
// posted it (ADR 26-10-11-657). The control plane answers those reads from it
// while the sandbox is not running.
//
// It is a row of its own rather than a column on Sandbox because it is large
// and read only for one sandbox at a time: on the sandbox row it would be
// loaded and returned by every listing. It is deleted with its sandbox.
type SandboxInventory struct {
	SandboxID string `gorm:"primaryKey;column:sandbox_id;type:text" json:"sandboxId" doc:"Sandbox the inventory is of"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	// Digest is the sandbox agent's hash of the inventory's parts. The pool
	// agent reports an unchanged inventory by it alone.
	Digest string `gorm:"column:digest;not null;type:text" json:"digest" doc:"Sandbox-agent digest of the inventory's parts"`
	// Document is the inventory as the sandbox agent sent it, sealed: it holds
	// terminal content and the environment exec records carry.
	Document []byte `gorm:"column:document" json:"-"`
	// ObservedAt is when the sandbox last had this inventory, on its own
	// clock: when it was taken, or the last poll that found it unchanged.
	ObservedAt time.Time `gorm:"column:observed_at;not null" json:"observedAt" doc:"When the sandbox last had this inventory" format:"date-time"`
}

// SandboxSecret is one sentinel a sandbox is delivered: the materialized form
// of a use binding that applies to it, never edited by a person. The sandbox is provisioned with the sentinel value; the
// proxy swaps it for the real value resolved from the referenced secret. The
// sentinel is non-secret but is not exposed through the API.
type SandboxSecret struct {
	ID        string `gorm:"primaryKey;type:text" json:"id" doc:"Stable assignment ID"`
	ProjectID string `gorm:"column:project_id;not null;type:text;index" json:"projectId" doc:"Project ID"`
	SandboxID string `gorm:"column:sandbox_id;not null;type:text;index;uniqueIndex:idx_sandbox_secret_env,priority:1" json:"sandboxId" doc:"Sandbox ID"`
	// BindingID is the binding the row delivers: the narrowest use binding
	// for EnvName that covers the sandbox (ADR 26-10-27-880 §2).
	BindingID string `gorm:"column:binding_id;not null;type:text;default:'';index" json:"bindingId" doc:"The binding delivered"`
	SecretID  string `gorm:"column:secret_id;not null;type:text;index" json:"secretId" doc:"Assigned secret ID"`
	EnvName   string `gorm:"column:env_name;not null;type:text;uniqueIndex:idx_sandbox_secret_env,priority:2" json:"envName" doc:"Environment variable name injected into the sandbox"`
	Sentinel  string `gorm:"column:sentinel;not null;type:text;uniqueIndex" json:"-"`
	// Format is the template the sentinel was minted from. A rebind compares it
	// against the replacement secret's format to decide whether the sentinel can
	// be kept, which it must be able to do after the old secret row is gone.
	Format string `gorm:"column:format;type:text" json:"-"`
	// Raw is the delivery of a raw binding's row, copied from the binding
	// (ADR 26-10-27-840): its sentinel is then only the pool's handle for the
	// value and is never delivered into the sandbox.
	Raw string `gorm:"column:raw;not null;type:text;default:''" json:"-"`
	// RawDeliveredRevision is the secret revision whose value this row last
	// handed its pool, so the delivered event is recorded once per value.
	RawDeliveredRevision int64     `gorm:"column:raw_delivered_revision;not null;default:0" json:"-"`
	CreatedAt            time.Time `gorm:"autoCreateTime" json:"createdAt" doc:"Creation timestamp" format:"date-time"`
}

func (SandboxSecret) TableName() string { return "sandbox_secrets" }

func (s *SandboxSecret) BeforeCreate(_ *gorm.DB) error {
	if s.ID == "" {
		var err error
		s.ID, err = id.New(id.PrefixSandboxSecret)
		if err != nil {
			return err
		}
	}
	return nil
}

// SecretRejection records a credential an upstream refused, which the proxy's
// retry could not save and this side cannot renew (ADR 0132 §4). It is live
// state rather than history: one row per secret and destination, cleared when
// the credential is replaced or starts working again.
//
// Its key is that pair rather than a generated ID. Nothing addresses a
// rejection — the window reads them as a list and acts on the secret — and a
// surrogate key would let the same credential be condemned twice for the same
// host, which is the one thing the table must not hold.
type SecretRejection struct {
	ProjectID string `gorm:"column:project_id;primaryKey;type:text" json:"projectId" doc:"Project ID"`
	SecretID  string `gorm:"column:secret_id;primaryKey;type:text;index" json:"secretId" doc:"Secret whose value was refused"`
	// Host is the destination that refused it, normalized the way every other
	// host on this model is. A credential good for one host and dead at another
	// is two different facts, and the row says which one this is.
	Host string `gorm:"column:host;primaryKey;type:text" json:"host" doc:"Destination host that refused the credential"`
	// Reason is why a person is needed: see SecretRejectionReasons.
	Reason string `gorm:"column:reason;not null;type:text" json:"reason" doc:"Why the rejection needs a person rather than another attempt" enum:"unrefreshable,refresh-failed,rejected-after-refresh"`
	// SandboxID is where it was last seen, kept for the report rather than
	// for matching: a harness credential fails in every sandbox on that
	// harness, and naming the most recent one is how somebody finds their way
	// to it.
	SandboxID string `gorm:"column:sandbox_id;not null;type:text;default:'';index" json:"sandboxId,omitempty" doc:"Sandbox that most recently saw the rejection"`
	// Count is how many times this has been reported. The proxy coalesces its
	// reports, so this counts minutes of failure rather than requests.
	Count       int64     `gorm:"column:count;not null;default:0" json:"count" doc:"How many times the rejection has been reported"`
	FirstSeenAt time.Time `gorm:"column:first_seen_at;not null" json:"firstSeenAt" doc:"When the credential was first refused" format:"date-time"`
	LastSeenAt  time.Time `gorm:"column:last_seen_at;not null" json:"lastSeenAt" doc:"When it was most recently refused" format:"date-time"`

	// Secret, HarnessConfigID and the rest are filled in on read: what the
	// window needs to name the credential and offer the remedy that fits it.
	// They are derived rather than stored for the reason the harness is not a
	// column — the answer belongs to the secret and its bindings, and a copy
	// here would be a second writer of it.
	SecretName        string `gorm:"-" json:"secretName,omitempty" doc:"Name of the refused secret"`
	SecretType        string `gorm:"-" json:"secretType,omitempty" doc:"Type of the refused secret" enum:"token,oauth,exchange"`
	EnvName           string `gorm:"-" json:"envName,omitempty" doc:"Environment variable the credential is delivered in, when one names it"`
	HarnessConfigID   string `gorm:"-" json:"harnessConfigId,omitempty" doc:"Harness config whose configure flow owns this credential, when one does"`
	HarnessConfigName string `gorm:"-" json:"harnessConfigName,omitempty" doc:"Display name of that harness config"`

	Project *Project `gorm:"foreignKey:ProjectID" json:"-"`
}

func (SecretRejection) TableName() string { return "secret_rejections" }

// AllModels returns all persisted model types.
func AllModels() []any {
	return []any{
		&User{},
		&Project{},
		&ProjectMember{},
		&ServerState{},
		&SandboxAccessIssuerKey{},
		&HarnessConfig{},
		&HarnessCaptureReceipt{},
		&Sandbox{},
		&SandboxProviderInstance{},
		&Pool{},
		&Secret{},
		&SecretRequest{},
		&SecretRefreshRequest{},
		&SecretBinding{},
		&SecretBindingEvent{},
		&SandboxSecret{},
		&SandboxInventory{},
		&SSHKey{},
		&ProjectSkillsSource{},
		&ProjectGateway{},
		&Peer{},
		&SecretRejection{},
		&HostTrustRequest{},
		&HostTrust{},
		&SandboxEvent{},
		&SandboxEventLog{},
	}
}
