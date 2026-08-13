package services

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"regexp"
	"sort"
	"strings"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
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

func OptURIStringPtr(value OptURI) *string {
	if v, ok := value.Get(); ok {
		s := v.String()
		return &s
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

var HarnessConfigEnvVarNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func HarnessConfigSecretsToModel(secrets []apimodel.HarnessConfigSecret) ([]model.HarnessConfigSecret, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	out := make([]model.HarnessConfigSecret, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		name := strings.TrimSpace(secret.Name)
		if !HarnessConfigEnvVarNamePattern.MatchString(name) {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("harness config secret name %q must be a valid environment variable name", secret.Name))
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("harness config secret %q is declared more than once", name))
		}
		seen[name] = struct{}{}
		out = append(out, model.HarnessConfigSecret{Name: name, Required: secret.Required.Or(false), OneOfGroup: strings.TrimSpace(secret.OneOfGroup.Or(""))})
	}
	return out, nil
}

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

func SandboxToAPI(sandbox *model.Sandbox, fallback *model.HarnessConfig) (serverapi.Sandbox, error) {
	if sandbox == nil {
		return serverapi.Sandbox{}, nil
	}
	config := map[string]any{
		"image": sandbox.Image,
		"name":  sandbox.Name,
	}
	if sandbox.ImageDigest != "" {
		config["imageDigest"] = sandbox.ImageDigest
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
	if sandbox.Description != nil {
		config["description"] = *sandbox.Description
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
	}
	if sandbox.LastActiveAt != nil {
		runtime["lastActiveAt"] = *sandbox.LastActiveAt
	}
	if sandbox.StateReportedAt != nil {
		runtime["stateReportedAt"] = *sandbox.StateReportedAt
	}
	if len(sandbox.AppliedCommits) > 0 {
		runtime["appliedCommits"] = sandbox.AppliedCommits
	}
	if upgrade := SandboxUpgrade(sandbox, fallback); upgrade != nil {
		runtime["upgrade"] = upgrade
	}
	if len(sandbox.AgentStatus) > 0 {
		runtime["agentStatus"] = sandbox.AgentStatus
	}
	if sandbox.AgentStatusObservedAt != nil {
		runtime["agentStatusObservedAt"] = *sandbox.AgentStatusObservedAt
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
		"createdAt":       sandbox.CreatedAt,
		"updatedAt":       sandbox.UpdatedAt,
		"config":          config,
		"runtime":         runtime,
	}
	if sandbox.Origin != nil {
		fields["origin"] = sandbox.Origin
	}
	if sandbox.PoolID != "" {
		fields["poolId"] = sandbox.PoolID
	}
	if sandbox.CreatedBy != nil {
		fields["createdBy"] = sandbox.CreatedBy
	}
	if sandbox.Pool != nil {
		fields["pool"] = sandbox.Pool
	}
	if sandbox.HarnessConfig != nil {
		fields["harnessConfig"] = sandbox.HarnessConfig
	}
	return Convert[serverapi.Sandbox](fields)
}

// SandboxImageTarget is an image identity: the reference to run and the digest
// saying which build that reference currently is.
type SandboxImageTarget struct {
	Image  string
	Digest string
}

// SandboxUpgradeTarget reports what an upgrade would move sandbox to, and
// whether that differs from what it runs now. It is the single implementation
// of the rule (ADR 0016 §2), shared by the read path that reports an available
// upgrade and the mutation that applies one — those two answering differently
// is a sandbox lying about itself.
//
// config is the sandbox's harness config, or nil when it has none; defaultImage
// is the server's default sandbox image. A sandbox with no harness config is
// not a sandbox with no image: it runs the default, which carries the sandbox
// agent, and that is what it upgrades to. A config-mode sandbox is running the
// configure command against a deliberately fixed image and upgrades to nothing.
//
// A candidate needs both halves: the reference says what to run, and only the
// digest says whether the sandbox already runs it. Comparing tags would report
// "up to date" for every sandbox on a tag its workflow rebuilds in place, which
// is the failure ADR 0016 §1 exists to prevent. An unpinned sandbox is eligible
// rather than excluded — its tag may no longer resolve anywhere, so adopting
// the current image is its only way forward.
// A zero SandboxImageTarget means there is nothing to move to, which is a
// different answer from "already up to date" and is reported differently.
//
// config is the sandbox's harness config, or the project's fallback `shell`
// config when the sandbox has none yet. A sandbox with no harness config is
// always reported as upgradable, whatever its digest: what the upgrade changes
// for it is adopting the config, and its digest matching already is not the
// same as it being converged (ADR 0025 §4).
func SandboxUpgradeTarget(sandbox *model.Sandbox, config *model.HarnessConfig) (SandboxImageTarget, bool) {
	if sandbox.HarnessMode == "config" || config == nil {
		return SandboxImageTarget{}, false
	}
	image, digest := strings.TrimSpace(config.Image), strings.TrimSpace(config.ImageDigest)
	if image == "" || digest == "" {
		return SandboxImageTarget{}, false
	}
	target := SandboxImageTarget{Image: image, Digest: digest}
	if sandbox.HarnessConfigID == nil {
		return target, true
	}
	return target, digest != strings.TrimSpace(sandbox.ImageDigest)
}

// SandboxUpgrade renders the upgrade field of a sandbox API response, or nil
// when the sandbox has nothing to move to.
//
// Derived on every read from the preloaded harness config, never stored: a
// cached flag would have to be invalidated by every path that writes a harness
// config, and the first one that forgot would leave a sandbox misreporting its
// own state.
func SandboxUpgrade(sandbox *model.Sandbox, fallback *model.HarnessConfig) map[string]any {
	if sandbox == nil {
		return nil
	}
	config := sandbox.HarnessConfig
	if sandbox.HarnessConfigID == nil {
		config = fallback
	}
	target, available := SandboxUpgradeTarget(sandbox, config)
	if target.Digest == "" {
		return nil
	}
	upgrade := map[string]any{
		"available":          available,
		"currentImage":       sandbox.Image,
		"currentImageDigest": sandbox.ImageDigest,
		"targetImage":        target.Image,
		"targetImageDigest":  target.Digest,
	}
	if available {
		upgrade["reason"] = "imageDigestChanged"
	}
	return upgrade
}

// SandboxDisplayState renders the small lifecycle vocabulary presented to API
// users: starting, running, stopping, stopped, archiving, archived, deleting,
// deleted, error.
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

func SandboxesToAPI(sandboxes []model.Sandbox, fallback *model.HarnessConfig) ([]serverapi.Sandbox, error) {
	out := make([]serverapi.Sandbox, 0, len(sandboxes))
	for i := range sandboxes {
		converted, err := SandboxToAPI(&sandboxes[i], fallback)
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
		HostID:      strings.TrimSpace(value.HostId),
		Hostname:    strings.TrimSpace(value.Hostname.Or("")),
		ProjectPath: strings.TrimSpace(value.ProjectPath),
		User:        strings.TrimSpace(value.User.Or("")),
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
		primary.Slug = defaultGitSourceSlug(primary.Slug, "primary", used)
	}

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

func OptStringValue(value OptString) (string, bool) {
	return value.Get()
}

func RawMessage(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}
