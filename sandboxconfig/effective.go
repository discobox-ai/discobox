package sandboxconfig

import "github.com/obot-platform/discobox/harness"

// APIVersion is the sandbox.json contract version.
const APIVersion = "discobox.dev/sandbox/v1"

// Config is the flat, fully-merged shape sandbox-agent decodes. It is
// Effective(Document)'s result, written verbatim (plus a sibling
// _provenance key) to /etc/discobox/sandbox.json by pool-agent.
type Config struct {
	APIVersion   string       `json:"apiVersion"`
	SandboxID    string       `json:"sandboxId"`
	Provider     Provider     `json:"provider"`
	AgentRuntime AgentRuntime `json:"agentRuntime"`
	Sources      []Source     `json:"sources,omitempty"`

	Harness Harness `json:"harness"`

	Model               string   `json:"model,omitempty"`
	ModelReasoningLevel string   `json:"modelReasoningLevel,omitempty"`
	ModelServiceTier    string   `json:"modelServiceTier,omitempty"`
	Prompt              []string `json:"prompt,omitempty"`
	User                User     `json:"user"`

	Env         map[string]string `json:"env,omitempty"`
	ProxyEnvs   []string          `json:"proxyEnvs,omitempty"`
	HarnessMode string            `json:"harnessMode,omitempty"`

	Files                   []File           `json:"files,omitempty"`
	Volumes                 []harness.Volume `json:"volumes,omitempty"`
	AdditionalGroups        []string         `json:"additionalGroups,omitempty"`
	WorkingDirectorySubpath string           `json:"workingDirectorySubpath,omitempty"`
}

// Harness is the effective, fully-resolved harness contract: exactly one
// harness per sandbox, already selected — there is nothing left to resolve
// at boot.
type Harness struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	RunCommand      []string `json:"runCommand"`
	RelaunchCommand []string `json:"relaunchCommand,omitempty"`
	ConfigCommand   []string `json:"configCommand,omitempty"`
}

// Provenance is diagnostic only: the raw per-layer inputs Effective merged,
// for a human or inspection tool to see which layer contributed a given
// effective value. Its schema is free to change without a version bump, and
// no runtime component decodes it.
type Provenance struct {
	Runtime RuntimeLayer  `json:"runtime"`
	Image   ImageLayer    `json:"image"`
	Project *ProjectLayer `json:"project,omitempty"`
}

// Effective merges a Document's three layers into one Config, per the merge
// rules in docs/adr/0012 §2. Each rule is applied by field category:
// single-writer fields copy from their one owning layer, override-grant
// fields take the image's value unless the project sets a non-empty
// replacement, Files overlays by path, and Env is additive-default (image
// fills only the keys runtime did not set).
func Effective(doc Document) (Config, Provenance) {
	cfg := Config{
		APIVersion:   APIVersion,
		SandboxID:    doc.Runtime.SandboxID,
		Provider:     doc.Runtime.Provider,
		AgentRuntime: doc.Runtime.AgentRuntime,
		Sources:      cloneSources(doc.Runtime.Sources),

		Model:               doc.Runtime.Model,
		ModelReasoningLevel: doc.Runtime.ModelReasoningLevel,
		ModelServiceTier:    doc.Runtime.ModelServiceTier,
		Prompt:              cloneStrings(doc.Runtime.Prompt),
		User:                doc.Runtime.User,

		HarnessMode:      doc.Runtime.HarnessMode,
		Volumes:          cloneVolumes(doc.Image.Volumes),
		AdditionalGroups: cloneStrings(doc.Image.AdditionalGroups),
	}

	cfg.Harness = Harness{
		ID:              doc.Image.HarnessID,
		Name:            doc.Image.HarnessName,
		Description:     doc.Image.HarnessDescription,
		RunCommand:      overrideGrant(doc.Image.RunCommand, projectRunCommand(doc.Project)),
		RelaunchCommand: overrideGrant(doc.Image.RelaunchCommand, projectRelaunchCommand(doc.Project)),
		ConfigCommand:   cloneStrings(doc.Image.ConfigCommand),
	}

	cfg.Env = mergeEnv(doc.Image.Env, doc.Runtime.Env)
	cfg.ProxyEnvs = cloneStrings(doc.Runtime.ProxyEnvs)
	cfg.Files = mergeFiles(doc.Image.Files, doc.Runtime.Files, doc.Project)

	if doc.Project != nil {
		cfg.WorkingDirectorySubpath = doc.Project.WorkingDirectorySubpath
	}

	return cfg, Provenance(doc)
}

func overrideGrant(imageValue, projectValue []string) []string {
	if len(projectValue) > 0 {
		return cloneStrings(projectValue)
	}
	return cloneStrings(imageValue)
}

func projectRunCommand(project *ProjectLayer) []string {
	if project == nil {
		return nil
	}
	return project.RunCommand
}

func projectRelaunchCommand(project *ProjectLayer) []string {
	if project == nil {
		return nil
	}
	return project.RelaunchCommand
}

// mergeEnv is additive-default: it starts from the image's map, then runtime
// entries fill in and override. Image contributes only keys runtime did not
// set.
func mergeEnv(image, runtime map[string]string) map[string]string {
	if len(image) == 0 && len(runtime) == 0 {
		return nil
	}
	out := make(map[string]string, len(image)+len(runtime))
	for k, v := range image {
		out[k] = v
	}
	for k, v := range runtime {
		out[k] = v
	}
	return out
}

// mergeFiles overlays image and runtime entries by path (a later entry with a
// matching path replaces, a new path appends), then appends the project's
// FilesAdd entries whose path is not already present.
func mergeFiles(image, runtime []File, project *ProjectLayer) []File {
	order := make([]string, 0, len(image)+len(runtime))
	byPath := make(map[string]File, len(image)+len(runtime))
	upsert := func(f File) {
		if _, exists := byPath[f.Path]; !exists {
			order = append(order, f.Path)
		}
		byPath[f.Path] = f
	}
	for _, f := range image {
		upsert(f)
	}
	for _, f := range runtime {
		upsert(f)
	}
	if project != nil {
		for _, f := range project.FilesAdd {
			if _, exists := byPath[f.Path]; exists {
				continue
			}
			upsert(f)
		}
	}
	if len(order) == 0 {
		return nil
	}
	out := make([]File, 0, len(order))
	for _, path := range order {
		out = append(out, byPath[path])
	}
	return out
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string{}, in...)
}

func cloneSources(in []Source) []Source {
	if len(in) == 0 {
		return nil
	}
	return append([]Source{}, in...)
}

func cloneVolumes(in []harness.Volume) []harness.Volume {
	if len(in) == 0 {
		return nil
	}
	return append([]harness.Volume{}, in...)
}
