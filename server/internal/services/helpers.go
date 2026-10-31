package services

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/imagecache"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/pkg/sandboxuser"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func Convert[To any](from any) (To, error) {
	var to To
	data, err := json.Marshal(from)
	if err != nil {
		return to, err
	}
	if err := json.Unmarshal(data, &to); err != nil {
		return to, err
	}
	return to, nil
}

func OptStringPtr(value OptString) *string {
	if v, ok := value.Get(); ok {
		return &v
	}
	return nil
}

func OptIntPtr(value OptInt64) *int {
	if v, ok := value.Get(); ok {
		i := int(v)
		return &i
	}
	return nil
}

func HarnessConfigFilesToModel(files []apimodel.HarnessConfigFile) []model.HarnessConfigFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]model.HarnessConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, model.HarnessConfigFile{Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly.Or(false), Template: file.Template.Or(false)})
	}
	return out
}

// EnvVarNamePattern is what an environment variable a credential or a
// harness setting is delivered in may be called, wherever one is named.
var EnvVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SandboxUserFields is the sandbox user as the model stores it. Every field is
// optional and stays nil when the request omitted it: the control plane cannot
// resolve a sandbox's names or ids, so it records intent verbatim and the
// sandbox resolves it (ADR 0025 §4).
type SandboxUserFields struct {
	Name             *string
	UID              *int
	GID              *int
	GroupName        *string
	HomeDirectory    *string
	AdditionalGroups []string
}

func SandboxUserToModel(value OptSandboxUser) SandboxUserFields {
	user, ok := value.Get()
	if !ok {
		return SandboxUserFields{}
	}
	return SandboxUserFields{
		Name:             OptStringPtr(user.Name),
		UID:              OptIntPtr(user.UID),
		GID:              OptIntPtr(user.Gid),
		GroupName:        OptStringPtr(user.GroupName),
		HomeDirectory:    OptStringPtr(user.HomeDirectory),
		AdditionalGroups: append([]string(nil), user.AdditionalGroups...),
	}
}

// ValidateAccount refuses a user no sandbox on goos may be created with: on
// Linux an account named without its uid, or an id outside root and the
// guest's account range (ADR 0141); elsewhere anything but the sandbox's one
// account, by name (ADR 0145 §5). The rule is sandboxuser's; this is only the
// conversion to it.
func (f SandboxUserFields) ValidateAccount(goos string) error {
	user := &sandboxuser.User{AdditionalGroups: f.AdditionalGroups}
	if f.Name != nil {
		user.Name = *f.Name
	}
	if f.GroupName != nil {
		user.GroupName = *f.GroupName
	}
	if f.HomeDirectory != nil {
		user.HomeDirectory = *f.HomeDirectory
	}
	if f.UID != nil {
		user.UID = sandboxuser.ID(int64(*f.UID))
	}
	if f.GID != nil {
		user.GID = sandboxuser.ID(int64(*f.GID))
	}
	if err := user.ValidateAccount(goos); err != nil {
		return apperrors.NewStatusError(http.StatusBadRequest, "sandbox user: "+err.Error())
	}
	return nil
}

func SandboxUserFromModel(sandbox *model.Sandbox) *serverapi.SandboxUser {
	if sandbox == nil || sandbox.UserName == nil && sandbox.UserUID == nil && sandbox.UserGID == nil &&
		sandbox.UserGroupName == nil && sandbox.HomeDirectory == nil && len(sandbox.UserAdditionalGroups) == 0 {
		return nil
	}
	user := &serverapi.SandboxUser{}
	if sandbox.UserName != nil {
		user.SetName(serverapi.NewOptString(*sandbox.UserName))
	}
	if sandbox.UserUID != nil {
		user.SetUID(serverapi.NewOptInt64(int64(*sandbox.UserUID)))
	}
	if sandbox.UserGID != nil {
		user.SetGid(serverapi.NewOptInt64(int64(*sandbox.UserGID)))
	}
	if sandbox.UserGroupName != nil {
		user.SetGroupName(serverapi.NewOptString(*sandbox.UserGroupName))
	}
	if len(sandbox.UserAdditionalGroups) > 0 {
		user.SetAdditionalGroups(append([]string(nil), sandbox.UserAdditionalGroups...))
	}
	if sandbox.HomeDirectory != nil {
		user.SetHomeDirectory(serverapi.NewOptString(*sandbox.HomeDirectory))
	}
	return user
}

// SandboxGitFields is the sandbox's git authorship as the model stores it. Like
// the run user it is recorded verbatim — the control plane has no way to check
// that a name or address is the one its owner meant — but unlike the run user
// nothing about it is ever resolved inside the sandbox: boot writes what it was
// given (ADR 0042).
type SandboxGitFields struct {
	UserName  *string
	UserEmail *string
}

func SandboxGitToModel(value OptSandboxGitIdentity) SandboxGitFields {
	git, ok := value.Get()
	if !ok {
		return SandboxGitFields{}
	}
	return SandboxGitFields{
		UserName:  OptStringPtr(git.UserName),
		UserEmail: OptStringPtr(git.UserEmail),
	}
}

// SandboxFeaturesToModel is the request's features, or the refusal a name
// that is not a feature's earns.
func SandboxFeaturesToModel(in serverapi.SandboxCreateConfigFeatures) (map[string]sandboxconfig.Feature, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]sandboxconfig.Feature, len(in))
	for name, feature := range in {
		if !provisionspec.ValidName(name) {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("feature %q is not a feature's name", name))
		}
		if name == provisionspec.Core {
			// Refused here rather than by the box at readiness: core is
			// not optional (ADR 26-10-16-423 §4).
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "core is not a feature a repository's settings can name: it is never off")
		}
		out[name] = sandboxconfig.Feature{Enabled: feature.Enabled}
	}
	return out, nil
}

// SandboxSkillsSourcesToModel is the request's skills sources, in order.
func SandboxSkillsSourcesToModel(in []serverapi.SandboxSkillsSource) []model.SkillsSource {
	if len(in) == 0 {
		return nil
	}
	out := make([]model.SkillsSource, 0, len(in))
	for _, source := range in {
		out = append(out, model.SkillsSource{Source: source.Source, Directory: source.Directory})
	}
	return out
}

// SandboxSkillsToModel is the request's skills as the sandbox keeps them, or
// the refusal they earn: a name that is not one directory, a file that leaves
// its skill, or more content than a bootstrap carries (ADR 26-10-09-395 §1).
func SandboxSkillsToModel(value OptSandboxCreateConfigSkills) (sandboxconfig.Skills, error) {
	in, ok := value.Get()
	if !ok || len(in) == 0 {
		return nil, nil
	}
	skills := make(sandboxconfig.Skills, len(in))
	for name, skill := range in {
		var files []sandboxconfig.SkillFile
		for _, file := range skill.Files {
			files = append(files, sandboxconfig.SkillFile{
				Path:       file.Path,
				Content:    file.Content,
				Executable: file.Executable.Or(false),
			})
		}
		skills[name] = sandboxconfig.Skill{Skill: skill.Skill, Files: files}
	}
	if err := skills.Validate(); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return skills, nil
}

func SandboxGitFromModel(sandbox *model.Sandbox) *serverapi.SandboxGitIdentity {
	if sandbox == nil || sandbox.GitUserName == nil && sandbox.GitUserEmail == nil {
		return nil
	}
	git := &serverapi.SandboxGitIdentity{}
	if sandbox.GitUserName != nil {
		git.SetUserName(serverapi.NewOptString(*sandbox.GitUserName))
	}
	if sandbox.GitUserEmail != nil {
		git.SetUserEmail(serverapi.NewOptString(*sandbox.GitUserEmail))
	}
	return git
}

func SandboxToAPI(sandbox *model.Sandbox, basis SandboxUpgradeBasis) (serverapi.Sandbox, error) {
	if sandbox == nil {
		return serverapi.Sandbox{}, nil
	}
	config := map[string]any{"name": sandbox.Name}
	if sandbox.Image != "" {
		config["image"] = sandbox.Image
	}
	if sandbox.ImageDigest != "" {
		config["imageDigest"] = sandbox.ImageDigest
	}
	if harness := sandbox.HarnessPin(); harness.Digest != "" {
		config["bundle"] = harness.Reference
		config["bundleDigest"] = harness.Digest
	}
	if sandbox.HarnessManifestDigest != "" {
		config["harnessManifestDigest"] = sandbox.HarnessManifestDigest
	}
	if platform := sandbox.PlatformPin(); platform.Digest != "" {
		config["platformBundle"] = platform.Reference
		config["platformBundleDigest"] = platform.Digest
	}
	if sandbox.HarnessConfigID != nil {
		config["harnessConfigId"] = *sandbox.HarnessConfigID
	}
	if sandbox.HarnessMode != "" {
		config["harnessMode"] = sandbox.HarnessMode
	}
	if sandbox.Model != nil {
		config["model"] = *sandbox.Model
	}
	if sandbox.ModelServiceTier != nil {
		config["modelServiceTier"] = *sandbox.ModelServiceTier
	}
	if sandbox.ModelReasoningLevel != nil {
		config["modelReasoningLevel"] = *sandbox.ModelReasoningLevel
	}
	if sandbox.Env != nil {
		config["env"] = sandbox.Env
	}
	if len(sandbox.Prompt) > 0 {
		config["prompt"] = sandbox.Prompt
	}
	if sandbox.Source != nil {
		config["source"] = sandbox.Source
	}
	if sandbox.SourceCodeReferences != nil {
		config["sourceCodeReferences"] = sandbox.SourceCodeReferences
	}
	if user := SandboxUserFromModel(sandbox); user != nil {
		config["user"] = user
	}
	if git := SandboxGitFromModel(sandbox); git != nil {
		config["git"] = git
	}
	// The names, never the content: a listing would otherwise carry every
	// skill of every sandbox in it (ADR 26-10-09-395 §2).
	if len(sandbox.SkillNames) > 0 {
		config["skillNames"] = sandbox.SkillNames
	}
	if len(sandbox.SkillsSources) > 0 {
		config["skillsSources"] = sandbox.SkillsSources
	}
	if len(sandbox.Features) > 0 {
		config["features"] = sandbox.Features
	}
	runtime := map[string]any{
		"desiredState":       sandbox.DesiredState,
		"displayState":       SandboxDisplayState(sandbox),
		"generation":         sandbox.Generation,
		"observedGeneration": sandbox.ObservedGeneration,
		"state":              sandbox.State,
		"stateChangedAt":     sandbox.StateChangedAt,
	}
	// Omitted rather than sent empty: empty means no agent has reported on this
	// sandbox yet, and an empty string is not a member of the runtimeState enum
	// (ADR 0034 §2).
	if sandbox.RuntimeState != "" {
		runtime["runtimeState"] = sandbox.RuntimeState
	}
	if sandbox.RuntimeStateChangedAt != nil {
		runtime["runtimeStateChangedAt"] = *sandbox.RuntimeStateChangedAt
	}
	if sandbox.ErrorMessage != nil {
		runtime["errorMessage"] = *sandbox.ErrorMessage
		if sandbox.ErrorReason != "" {
			runtime["errorReason"] = sandbox.ErrorReason
		}
	}
	if sandbox.LastActiveAt != nil {
		runtime["lastActiveAt"] = *sandbox.LastActiveAt
	}
	if sandbox.StateReportedAt != nil {
		runtime["stateReportedAt"] = *sandbox.StateReportedAt
	}
	if len(sandbox.SourceDeliveries) > 0 {
		runtime["sourceDeliveries"] = sandbox.SourceDeliveries
	}
	if sandbox.SourceDeliveredAt != nil {
		runtime["sourceDeliveredAt"] = *sandbox.SourceDeliveredAt
	}
	if upgrade := SandboxUpgrade(sandbox, basis); upgrade != nil {
		runtime["upgrade"] = upgrade
	}
	if sandbox.AutomaticRepair != nil {
		runtime["automaticRepair"] = sandbox.AutomaticRepair
	}
	if len(sandbox.AgentStatus) > 0 {
		status := sandbox.AgentStatus
		if sandbox.HarnessChangeID != "" && sandbox.HarnessConfigID != nil {
			var err error
			status, err = sandboxAgentStatusCurrentHarness(status, *sandbox.HarnessConfigID)
			if err != nil {
				return serverapi.Sandbox{}, err
			}
		}
		runtime["agentStatus"] = status
	}
	if sandbox.AgentStatusObservedAt != nil {
		runtime["agentStatusObservedAt"] = *sandbox.AgentStatusObservedAt
	}
	if len(sandbox.Resources) > 0 {
		runtime["resources"] = sandbox.Resources
	}
	if sandbox.ResourcesObservedAt != nil {
		runtime["resourcesObservedAt"] = *sandbox.ResourcesObservedAt
	}
	if len(sandbox.ProvisionProgress) > 0 {
		runtime["provisionProgress"] = sandbox.ProvisionProgress
	}
	if sandbox.ProvisionProgressAt != nil {
		runtime["provisionProgressAt"] = *sandbox.ProvisionProgressAt
	}
	fields := map[string]any{
		"id":              sandbox.ID,
		"projectId":       sandbox.ProjectID,
		"createdByUserId": sandbox.CreatedByUserID,
		"displayName":     SandboxDisplayName(sandbox),
		"createdAt":       sandbox.CreatedAt,
		"updatedAt":       sandbox.UpdatedAt,
		"config":          config,
		"runtime":         runtime,
	}
	if sandbox.Origin != nil {
		fields["origin"] = sandbox.Origin
	}
	if sandbox.OriginKey != nil {
		fields["originKey"] = *sandbox.OriginKey
	}
	if sandbox.PoolID != "" {
		fields["poolId"] = sandbox.PoolID
	}
	if !sandbox.Platform.IsZero() {
		fields["platform"] = sandbox.Platform.String()
	}
	if sandbox.CreatedBy != nil {
		fields["createdBy"] = sandbox.CreatedBy
	}
	if sandbox.CreatedBySandboxID != nil {
		fields["createdBySandboxId"] = *sandbox.CreatedBySandboxID
	}
	// The discobox's own egress level (ADR 26-10-01-032), not its effective
	// policy: what a person set here, and what `box update` keeps when it is
	// given only one of its two flags.
	fields["egress"] = sandbox.Egress
	if sandbox.DiscoboxAPI {
		fields["discoboxApi"] = true
	}
	if sandbox.HarnessConfig != nil {
		fields["harnessConfig"] = sandbox.HarnessConfig
	}
	// The copy of what the sandbox holds (ADR 0136). Before it has reported,
	// that is the description it was created with and nothing else;
	// metaObservedAt is absent until then, which is what says so. The name
	// and the description are there only when set (ADR 26-10-12-026 §1). The
	// tags are there always, empty or not: the schema no longer requires
	// them, but a client generated from before it stopped does, and would
	// fail to read every sandbox that has none.
	meta := map[string]any{"tags": sandbox.Tags}
	if sandbox.Tags == nil {
		meta["tags"] = map[string]string{}
	}
	if name := sandboxMetaName(sandbox); name != "" {
		meta["name"] = name
	}
	if sandbox.Description != nil && *sandbox.Description != "" {
		meta["description"] = *sandbox.Description
	}
	fields["meta"] = meta
	if sandbox.MetaObservedAt != nil {
		fields["metaObservedAt"] = *sandbox.MetaObservedAt
	}
	result, err := Convert[serverapi.Sandbox](fields)
	if err != nil {
		return result, err
	}
	if sandbox.Pool != nil {
		pool, err := PoolToAPI(sandbox.Pool)
		if err != nil {
			return result, err
		}
		result.Pool = serverapi.NewOptPool(pool)
	}
	return result, nil
}

// HarnessPins is what a harness config pins a sandbox to (ADR 26-10-16-423
// §2): its bundle, which the box applies, or, for a template with no image,
// its manifest file — one of the two — and the digest of the manifest
// composed from it, which its container is built with.
type HarnessPins struct {
	Bundle         model.BundlePin
	Image          string
	ImageDigest    string
	ManifestDigest string
}

// HarnessConfigPins is what config pins a sandbox to now, and false when it
// pins nothing: a config registered before bundles, which has no bundle, or
// one whose bundle has no content digest yet — a development bundle no pool
// has built — which no box can fetch.
func HarnessConfigPins(config *model.HarnessConfig) (HarnessPins, bool) {
	if config == nil {
		return HarnessPins{}, false
	}
	manifest := strings.TrimSpace(config.ManifestDigest)
	if bundle := strings.TrimSpace(config.Bundle); bundle != "" {
		digest := strings.TrimSpace(config.BundleDigest)
		return HarnessPins{Bundle: model.BundlePin{Reference: bundle, Digest: digest}, ManifestDigest: manifest}, imagecache.IsDigest(digest)
	}
	// A config's image is a manifest file's or nothing: no harness is an
	// image (ADR 26-10-16-423 §10).
	image, digest := strings.TrimSpace(config.Image), strings.TrimSpace(config.ImageDigest)
	return HarnessPins{Image: image, ImageDigest: digest, ManifestDigest: manifest},
		strings.HasPrefix(image, ManifestFileScheme) && digest != ""
}

// ManifestFileScheme prefixes a harness config's image when it names a
// manifest file, the whole manifest of a template with no image.
const ManifestFileScheme = "file://"

// ReleasePins are the bundles the release names that every delivered box is
// pinned to: the platform it applies first, and the agent its pool places it
// with (ADR 26-10-24-953 §2). Each is zero when the release's reference has
// not resolved.
type ReleasePins struct {
	Platform model.BundlePin
	Agent    model.BundlePin
}

// SandboxPinTarget is what a sandbox is pinned to: its harness config's
// pins, and the release's.
type SandboxPinTarget struct {
	// Harness is zero when the harness config offers nothing to move to,
	// which leaves the sandbox's own in place.
	Harness HarnessPins
	// Platform and Agent are the release's bundles as its references
	// resolve; a zero one is not known, which leaves the sandbox's own in
	// place.
	Platform model.BundlePin
	Agent    model.BundlePin
}

// Empty reports whether the target has nothing to move a sandbox to.
func (t SandboxPinTarget) Empty() bool { return t == SandboxPinTarget{} }

// Apply writes the target's pins onto sandbox. A harness, a platform or an
// agent the target does not name leaves the sandbox's as it is. A harness it
// names replaces all three of its pins, so a sandbox from before bundles
// leaves the runnable image it carried behind once it is pinned to its
// bundle.
func (t SandboxPinTarget) Apply(sandbox *model.Sandbox) {
	platform, harness := sandbox.PlatformPin(), sandbox.HarnessPin()
	if t.Harness != (HarnessPins{}) {
		sandbox.Image, sandbox.ImageDigest = t.Harness.Image, t.Harness.ImageDigest
		harness = t.Harness.Bundle
		sandbox.HarnessManifestDigest = t.Harness.ManifestDigest
	}
	if t.Platform.Digest != "" {
		platform = t.Platform
	}
	sandbox.SetPins(platform, harness)
	if t.Agent.Digest != "" {
		sandbox.SetAgentPin(t.Agent)
	}
}

// upgradeReason is why an upgrade is available: the first pin that moved.
func (t SandboxPinTarget) upgradeReason(sandbox *model.Sandbox) string {
	harness := t.Harness != (HarnessPins{})
	switch {
	case harness && t.Harness.Bundle.Digest != sandbox.HarnessPin().Digest:
		return "bundleDigestChanged"
	case harness && t.Harness.ImageDigest != strings.TrimSpace(sandbox.ImageDigest):
		return "imageDigestChanged"
	case harness && t.Harness.ManifestDigest != sandbox.HarnessManifestDigest:
		return "manifestDigestChanged"
	case t.Platform.Digest != "" && t.Platform.Digest != sandbox.PlatformPin().Digest:
		return "platformBundleDigestChanged"
	case t.Agent.Digest != "" && t.Agent.Digest != sandbox.AgentBundleDigest:
		return "agentBundleDigestChanged"
	}
	return "bundleDigestChanged"
}

// SandboxUpgradeTarget reports what an upgrade would move sandbox to, and
// whether that differs from what it runs now. It is the single implementation
// of the rule (ADR 0016 §2), shared by the read path that reports an available
// upgrade and the mutation that applies one — those two answering differently
// is a sandbox lying about itself.
//
// The harness's pins are its config's now (HarnessConfigPins), and the
// platform and agent the bundles the release's references resolve to now,
// each zero when that is not known (ADR 26-10-16-423 §7, ADR 26-10-24-953
// §2). A pin that moved makes an upgrade available, and it is the digest that
// says so: comparing tags would report "up to date" for every sandbox on a tag
// its workflow rebuilds in place, which is the failure ADR 0016 §1 exists to
// prevent. The release's bundles are every box's, so they are a target of
// their own even for a sandbox whose harness has nothing to move to; only a
// config-mode sandbox, which runs the configure flow and is thrown away,
// follows nothing.
//
// A sandbox from before bundles, pinned to a runnable harness image, has no
// bundle pin, so its harness's bundle is always a move for it.
//
// A zero SandboxPinTarget means there is nothing to move to, which is a
// different answer from "already up to date" and is reported differently.
//
// config is the sandbox's harness config, or the project's fallback `shell`
// config when the sandbox has none yet. A sandbox with no harness config is
// always reported as upgradable, whatever its pins: what the upgrade changes
// for it is adopting the config, and its pins matching already is not the
// same as it being converged (ADR 0032 §4).
//
// A config whose manifest file's template does not run on the sandbox's
// platform is nothing to move to: a sandbox never leaves its own (ADR 0145
// §1). A sandbox whose pool has not yet declared one is not held back by it.
func SandboxUpgradeTarget(sandbox *model.Sandbox, config *model.HarnessConfig, release ReleasePins) (SandboxPinTarget, bool) {
	if sandbox.HarnessMode == "config" {
		return SandboxPinTarget{}, false
	}
	var target SandboxPinTarget
	available := false
	if pins, ok := harnessTarget(sandbox, config); ok {
		target.Harness = pins
		available = sandbox.HarnessConfigID == nil || pins.Bundle.Digest != sandbox.HarnessPin().Digest ||
			pins.ImageDigest != strings.TrimSpace(sandbox.ImageDigest) || pins.ManifestDigest != sandbox.HarnessManifestDigest
	}
	if release.Platform.Digest != "" {
		target.Platform = release.Platform
		available = available || release.Platform.Digest != sandbox.PlatformPin().Digest
	}
	if release.Agent.Digest != "" {
		target.Agent = release.Agent
		available = available || release.Agent.Digest != sandbox.AgentBundleDigest
	}
	return target, available
}

// harnessTarget is what config offers sandbox, when it offers anything: its
// pins, on the sandbox's platform.
func harnessTarget(sandbox *model.Sandbox, config *model.HarnessConfig) (HarnessPins, bool) {
	pins, ok := HarnessConfigPins(config)
	if !ok {
		return HarnessPins{}, false
	}
	if !sandbox.Platform.IsZero() && config.Platforms.Publishes(sandbox.Platform) != nil {
		return HarnessPins{}, false
	}
	return pins, true
}

// SandboxUpgradeBasis is what a sandbox's available upgrade is read against
// besides its own harness config: the project's reserved `shell` config,
// which a legacy sandbox with no harness config of its own upgrades to (ADR
// 0032 §4; nil when seeding has not created it), and the release's bundles as
// its references were last resolved (each zero when it has not been).
type SandboxUpgradeBasis struct {
	Fallback *model.HarnessConfig
	Release  ReleasePins
}

// SandboxUpgrade renders the upgrade field of a sandbox API response, or nil
// when the sandbox has nothing to move to.
//
// Derived on every read from the preloaded harness config, never stored: a
// cached flag would have to be invalidated by every path that writes a harness
// config, and the first one that forgot would leave a sandbox misreporting its
// own state.
func SandboxUpgrade(sandbox *model.Sandbox, basis SandboxUpgradeBasis) map[string]any {
	if sandbox == nil {
		return nil
	}
	config := sandbox.HarnessConfig
	if sandbox.HarnessConfigID == nil {
		config = basis.Fallback
	}
	target, available := SandboxUpgradeTarget(sandbox, config, basis.Release)
	if target.Empty() {
		return nil
	}
	upgrade := map[string]any{"available": available}
	if sandbox.ImageDigest != "" {
		upgrade["currentImage"], upgrade["currentImageDigest"] = sandbox.Image, sandbox.ImageDigest
	}
	if target.Harness.ImageDigest != "" {
		upgrade["targetImage"], upgrade["targetImageDigest"] = target.Harness.Image, target.Harness.ImageDigest
	}
	if digest := sandbox.HarnessPin().Digest; digest != "" {
		upgrade["currentBundleDigest"] = digest
	}
	if target.Harness.Bundle.Digest != "" {
		upgrade["targetBundleDigest"] = target.Harness.Bundle.Digest
	}
	if digest := sandbox.PlatformPin().Digest; digest != "" {
		upgrade["currentPlatformBundleDigest"] = digest
	}
	if target.Platform.Digest != "" {
		upgrade["targetPlatformBundleDigest"] = target.Platform.Digest
	}
	if sandbox.AgentBundleDigest != "" {
		upgrade["currentAgentBundleDigest"] = sandbox.AgentBundleDigest
	}
	if target.Agent.Digest != "" {
		upgrade["targetAgentBundleDigest"] = target.Agent.Digest
	}
	if available {
		upgrade["reason"] = target.upgradeReason(sandbox)
	}
	return upgrade
}

// SandboxDisplayState renders the small lifecycle vocabulary presented to API
// users: starting, running, stopping, stopped, suspended, repairing, archiving,
// archived, deleting, deleted, error.
//
// It is the one place the two state axes are combined, and it is what clients
// should read: existence is the reconciler's `State`, power is the pool agent's
// `RuntimeState`, and a caller asking "what is this sandbox doing" wants one
// answer composed from both (ADR 0034 §5).
//
// Existence is consulted first and wins, because a sandbox that is being
// archived or deleted is not described by whatever its container was last seen
// doing. Only once existence is settled at `ready` does the runtime axis
// answer.
func SandboxDisplayState(sandbox *model.Sandbox) string {
	if sandbox == nil {
		return "error"
	}
	// A repair that has not landed answers before the failure it is repairing:
	// recording it cleared the error, the row still reads `failed` until the
	// reconciler writes, and "error" would name the thing being fixed
	// (ADR 26-10-12-711 §8).
	if sandbox.Repairing() {
		return "repairing"
	}
	if sandbox.ErrorMessage != nil || sandbox.State == model.SandboxStateFailed {
		return "error"
	}
	if sandbox.DesiredState == model.DesiredStateDeleted && sandbox.State != model.SandboxStateDeleted {
		return "deleting"
	}
	if sandbox.DesiredState == model.DesiredStateArchived && sandbox.State != model.SandboxStateArchived {
		return "archiving"
	}
	switch sandbox.State {
	case model.SandboxStatePending, model.SandboxStateAwaitingSource:
		// Both are a sandbox on its way up for the first time, whatever its
		// container is doing: the agent may already have reported `running`
		// while the reconciler is still finishing the create, and the sandbox
		// the caller asked for is not ready until both are true.
		//
		// Awaiting-source is parked rather than working, but from the caller's
		// side it is the same "not ready yet", and the client that owes it a
		// push is the one that already knows.
		return "starting"
	case model.SandboxStateReady:
		if sandbox.RuntimeState == "" {
			// Converged, and no agent has reported on it yet. That window is
			// brief — the create publishes what it observed before returning —
			// and "starting" is the honest reading of it: the container exists
			// and nobody has seen it settle.
			return "starting"
		}
		return sandbox.RuntimeState
	case model.SandboxStateArchived, model.SandboxStateDeleted:
		return sandbox.State
	default:
		// Includes the empty state, which is a row that was never given one.
		return "error"
	}
}

// SandboxDisplayName is the name a listing shows for a sandbox: the name in
// its meta when one is set (ADR 26-10-12-026 §3), followed by what the program
// in its primary terminal says — the msg it last reported over OSC 7501 while
// the sandbox is running and it has one, else the window title the terminal
// last set — as "name: words" (sandboxmeta.Title; ADR 26-10-31-618). Either
// alone when there is only one, then the sandbox's own configured name, and the
// sandbox ID when it has no name either. The msg says what the work is doing
// and the title what it is about; both tell two sandboxes apart better than two
// generated names do. A set name is what a person or the agent chose to call
// it, which outranks what the harness happened to write, and leads it.
//
// It is computed here, next to the display state it sits beside on the wire, so
// every client names a sandbox the same way rather than each rediscovering the
// rule.
func SandboxDisplayName(sandbox *model.Sandbox) string {
	if sandbox == nil {
		return ""
	}
	title, msg := sandboxPrimaryTerminalTitleAndMsg(sandbox)
	if sandbox.RuntimeState != model.SandboxRuntimeStateRunning {
		// A stopped sandbox's row keeps the last status its agent posted,
		// and nothing is doing what that msg says in a box that is off.
		msg = ""
	}
	if name := sandboxmeta.Title(sandboxMetaName(sandbox), msg, title); name != "" {
		return name
	}
	if name := strings.TrimSpace(sandbox.Name); name != "" {
		return name
	}
	return sandbox.ID
}

// sandboxMetaName is the name the sandbox's meta holds, as the row last
// recorded it: empty when nobody has named the sandbox.
func sandboxMetaName(sandbox *model.Sandbox) string {
	if sandbox.MetaName == nil {
		return ""
	}
	return strings.TrimSpace(*sandbox.MetaName)
}

// sandboxPrimaryTerminalTitleAndMsg is the window title the sandbox's primary
// terminal last set (OSC 0/2), and the msg its program last reported with its
// status (OSC 7501), as sandbox-agent reported them with the rest of its status
// (ADR 0030, ADR 26-10-16-424) — already on the sandbox row, so naming a
// listing wakes nothing. Each is empty when no primary session has set one,
// which is the caller's cue to fall back. Sessions live and ended alike are on
// the record; the newest primary that set either is the one whose words the
// user last saw, and both are read from that one session.
func sandboxPrimaryTerminalTitleAndMsg(sandbox *model.Sandbox) (title, msg string) {
	if len(sandbox.AgentStatus) == 0 {
		return "", ""
	}
	// Decoded through a local struct rather than the generated schema type:
	// the column is whatever sandbox-agent last wrote, and rows written by an
	// older or newer agent carry fields this server does not know. The
	// generated type forbids those (additionalProperties: false), so decoding
	// through it would fail the whole payload over a field nothing here reads
	// — and a stopped sandbox's row is never rewritten, so that loss would be
	// permanent. Only the fields below are the contract.
	var status struct {
		Sessions []struct {
			Primary       bool      `json:"primary"`
			Title         string    `json:"title"`
			StartedAt     time.Time `json:"startedAt"`
			ProgramStatus *struct {
				Msg string `json:"msg"`
			} `json:"programStatus"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(sandbox.AgentStatus, &status); err != nil {
		return "", ""
	}
	found, startedAt := false, time.Time{}
	for _, session := range status.Sessions {
		if !session.Primary {
			continue
		}
		sessionTitle, sessionMsg := strings.TrimSpace(session.Title), ""
		if session.ProgramStatus != nil {
			sessionMsg = strings.TrimSpace(session.ProgramStatus.Msg)
		}
		if sessionTitle == "" && sessionMsg == "" {
			continue
		}
		if !found || session.StartedAt.After(startedAt) {
			title, msg, startedAt, found = sessionTitle, sessionMsg, session.StartedAt, true
		}
	}
	return title, msg
}

func SandboxesToAPI(sandboxes []model.Sandbox, basis SandboxUpgradeBasis) ([]serverapi.Sandbox, error) {
	out := make([]serverapi.Sandbox, 0, len(sandboxes))
	for i := range sandboxes {
		converted, err := SandboxToAPI(&sandboxes[i], basis)
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
}

// OriginToModel converts a client-declared origin, or returns nil when the
// client sent none. Origin is recorded verbatim: it is provenance, not intent,
// and nothing here interprets or normalizes what the client reported.
func OriginToModel(input serverapi.OptOrigin) *model.Origin {
	value, ok := input.Get()
	if !ok {
		return nil
	}
	return &model.Origin{
		HostID:   strings.TrimSpace(value.HostId),
		Hostname: strings.TrimSpace(value.Hostname.Or("")),
		User:     strings.TrimSpace(value.User.Or("")),
	}
}

func GitSourceToModel(input serverapi.GitSource) model.GitSource {
	out := model.GitSource{Kind: string(input.Kind)}
	if value, ok := input.Slug.Get(); ok {
		out.Slug = &value
	}
	if urlValue, ok := input.URL.Get(); ok {
		value := urlValue.String()
		out.URL = &value
	}
	if value, ok := input.LocalDirectory.Get(); ok {
		out.LocalDirectory = &value
	}
	out.NoLocalRepository = input.NoLocalRepository.Or(false)
	out.NoLocalCommits = input.NoLocalCommits.Or(false)
	out.NoLocalGitDirectory = input.NoLocalGitDirectory.Or(false)
	if value, ok := input.UpstreamUrl.Get(); ok {
		out.UpstreamURL = &value
	}
	if checkout, ok := input.Checkout.Get(); ok {
		out.Checkout = &model.GitSourceCheckout{}
		if value, ok := checkout.Commit.Get(); ok {
			out.Checkout.Commit = &value
		}
		if value, ok := checkout.RefName.Get(); ok {
			out.Checkout.RefName = &value
		}
		if value, ok := checkout.RefType.Get(); ok {
			out.Checkout.RefType = &value
		}
	}
	if workspace, ok := input.Workspace.Get(); ok {
		out.Workspace = &model.GitSourceWorkspace{}
		if value, ok := workspace.Mode.Get(); ok {
			mode := string(value)
			out.Workspace.Mode = mode
		}
		if value, ok := workspace.SnapshotRef.Get(); ok {
			out.Workspace.SnapshotRef = &value
		}
		if value, ok := workspace.BaseCommit.Get(); ok {
			out.Workspace.BaseCommit = &value
		}
	}
	if destination, ok := input.Destination.Get(); ok {
		out.Destination = &model.GitSourceDestination{}
		if value, ok := destination.Directory.Get(); ok {
			out.Destination.Directory = &value
		}
		if value, ok := destination.WorkingDirectory.Get(); ok {
			out.Destination.WorkingDirectory = &value
		}
	}
	if value, ok := input.Purpose.Get(); ok {
		out.Purpose = string(value)
	}
	return out
}

func SourceCodeReferencesToModel(input serverapi.SandboxCreateConfigSourceCodeReferences) model.SourceCodeReferences {
	out := make(model.SourceCodeReferences, len(input))
	for key, source := range input {
		out[key] = GitSourceToModel(source)
	}
	return out
}

func DefaultGitSourceSlugs(primary *model.GitSource, refs model.SourceCodeReferences) {
	used := map[string]struct{}{}
	if primary != nil {
		primary.Slug = defaultGitSourceSlug(primary.Slug, sandboxconfig.PrimarySourceSlug, used)
	}
	// The primary's source data is mounted under this name whatever the
	// primary's own slug, and in a sandbox with no primary at all, so it is
	// never a reference's.
	used[sandboxconfig.PrimarySourceSlug] = struct{}{}

	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		source := refs[key]
		source.Slug = defaultGitSourceSlug(source.Slug, key, used)
		refs[key] = source
	}
}

func defaultGitSourceSlug(existing *string, seed string, used map[string]struct{}) *string {
	base := slugify(seed)
	if existing != nil && strings.TrimSpace(*existing) != "" {
		base = slugify(*existing)
	}
	base = trimSlugBase(base, 63)
	if base == "" {
		base = "source"
	}
	slug := base
	if _, ok := used[slug]; ok {
		slug = fmt.Sprintf("%s-%08x", trimSlugBase(base, 54), stableSlugHash(seed))
		for i := 2; ; i++ {
			if _, ok := used[slug]; !ok {
				break
			}
			slug = fmt.Sprintf("%s-%d", trimSlugBase(base, 61), i)
		}
	}
	used[slug] = struct{}{}
	return &slug
}

func slugify(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func stableSlugHash(value string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(value))
	return h.Sum32()
}

func trimSlugBase(value string, maxLen int) string {
	value = strings.Trim(value, "-")
	if len(value) <= maxLen {
		return value
	}
	return strings.Trim(value[:maxLen], "-")
}

func RawMessage(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

// AgentCredentialRequestStatus maps a request's approval state onto the
// protocol's vocabulary. The protocol says "granted", the control plane says
// "approved", and only the control plane knows that an approval whose binding
// has since been removed or lapsed no longer holds.
func AgentCredentialRequestStatus(req *model.SecretRequest, binding *model.SecretBinding) string {
	switch req.Status {
	case model.SecretRequestStatusApproved:
		if binding == nil {
			return model.SecretRequestStatusDenied
		}
		return "granted"
	case model.SecretRequestStatusDenied:
		return model.SecretRequestStatusDenied
	default:
		return model.SecretRequestStatusPending
	}
}

// EgressPolicyToModel reads an egress policy level off the API, refusing one
// the proxy could not enforce as a bad request naming the entry.
func EgressPolicyToModel(in apimodel.EgressPolicy) (egress.Policy, error) {
	allow, _ := in.Allow.Get()
	level := egress.Policy{Allow: egress.Normalize(allow), DenyByDefault: in.DenyByDefault.Or(false)}
	if err := level.Validate(); err != nil {
		return egress.Policy{}, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return level, nil
}
