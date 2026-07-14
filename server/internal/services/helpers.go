package services

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	serverapi "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/server/internal/harnessdefs"
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

func SandboxUserToModel(value OptSandboxUser) (name *string, uid *int, gid *int, homeDirectory *string) {
	user, ok := value.Get()
	if !ok {
		return nil, nil, nil, nil
	}
	return OptStringPtr(user.Name), OptIntPtr(user.UID), OptIntPtr(user.Gid), OptStringPtr(user.HomeDirectory)
}

func SandboxUserFromModel(sandbox *model.Sandbox) *serverapi.SandboxUser {
	if sandbox == nil || sandbox.UserName == nil && sandbox.UserUID == nil && sandbox.UserGID == nil && sandbox.HomeDirectory == nil {
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
	if sandbox.HomeDirectory != nil {
		user.SetHomeDirectory(serverapi.NewOptString(*sandbox.HomeDirectory))
	}
	return user
}

func SandboxToAPI(sandbox *model.Sandbox) (serverapi.Sandbox, error) {
	if sandbox == nil {
		return serverapi.Sandbox{}, nil
	}
	config := map[string]any{
		"cpuVcpus":     sandbox.CPUVCPUs,
		"image":        sandbox.Image,
		"memoryBytes":  sandbox.MemoryBytes,
		"name":         sandbox.Name,
		"storageBytes": sandbox.StorageBytes,
	}
	if sandbox.HarnessConfigID != nil {
		config["harnessConfigId"] = *sandbox.HarnessConfigID
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
		"desiredState":        sandbox.DesiredState,
		"generation":          sandbox.Generation,
		"lastOperationStatus": sandbox.LastOperationStatus,
		"observedGeneration":  sandbox.ObservedGeneration,
		"phase":               sandbox.Phase,
		"restartGeneration":   sandbox.RestartGeneration,
		"restartedGeneration": sandbox.RestartedGeneration,
	}
	if sandbox.ActiveOperation != nil {
		runtime["activeOperation"] = *sandbox.ActiveOperation
	}
	if sandbox.ErrorMessage != nil {
		runtime["errorMessage"] = *sandbox.ErrorMessage
	}
	if sandbox.LastActiveAt != nil {
		runtime["lastActiveAt"] = *sandbox.LastActiveAt
	}
	if sandbox.StatusMessage != nil {
		runtime["statusMessage"] = *sandbox.StatusMessage
	}
	if sandbox.WorkerID != nil {
		runtime["workerId"] = *sandbox.WorkerID
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
	if sandbox.ProviderInstanceID != nil {
		fields["providerInstanceId"] = *sandbox.ProviderInstanceID
	}
	if sandbox.CreatedBy != nil {
		fields["createdBy"] = sandbox.CreatedBy
	}
	if sandbox.ProviderInstance != nil {
		fields["providerInstance"] = sandbox.ProviderInstance
	}
	if sandbox.HarnessConfig != nil {
		// Resolve the sparse stored config against its definition so the embedded
		// harness config carries its effective commands (runCommand is required).
		fields["harnessConfig"] = harnessdefs.Resolve(sandbox.HarnessConfig)
	}
	return Convert[serverapi.Sandbox](fields)
}

func SandboxesToAPI(sandboxes []model.Sandbox) ([]serverapi.Sandbox, error) {
	out := make([]serverapi.Sandbox, 0, len(sandboxes))
	for i := range sandboxes {
		converted, err := SandboxToAPI(&sandboxes[i])
		if err != nil {
			return nil, err
		}
		out = append(out, converted)
	}
	return out, nil
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
