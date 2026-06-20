package hooks

import "strings"

// HookType identifies when a hook is eligible to run.
type HookType string

const (
	HookTypeSession   HookType = "session"
	HookTypeFile      HookType = "file"
	HookTypePreCommit HookType = "pre-commit"
)

// Valid reports whether t is a supported hook type.
func (t HookType) Valid() bool {
	switch t {
	case HookTypeSession, HookTypeFile, HookTypePreCommit:
		return true
	default:
		return false
	}
}

// HookEngine identifies how a hook definition is intended to be executed.
type HookEngine string

const (
	HookEngineScript  HookEngine = "script"
	HookEngineAI      HookEngine = "ai"
	HookEngineLSP     HookEngine = "lsp"
	HookEngineBuiltin HookEngine = "builtin"
)

// Valid reports whether e is a known hook engine. The initial runner executes
// script hooks only; ai is parsed for compatibility and builtin is reserved.
func (e HookEngine) Valid() bool {
	switch e {
	case HookEngineScript, HookEngineAI, HookEngineLSP, HookEngineBuiltin:
		return true
	default:
		return false
	}
}

// RunAs is an execution user hint for script/session hooks.
type RunAs string

const (
	RunAsUser RunAs = "user"
	RunAsRoot RunAs = "root"
)

// Valid reports whether r is a supported execution user hint.
func (r RunAs) Valid() bool {
	switch r {
	case "", RunAsUser, RunAsRoot:
		return true
	default:
		return false
	}
}

// Hook is the public model for a discovered hook definition. It is intentionally
// independent from daemon, runner, and persistence-specific state.
type Hook struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Type        HookType       `json:"type"`
	Engine      HookEngine     `json:"engine"`
	RunAs       RunAs          `json:"run_as,omitempty"`
	Blocking    bool           `json:"blocking,omitempty"`
	Pattern     string         `json:"pattern,omitempty"`
	Ignore      []string       `json:"ignore,omitempty"`
	Phase       string         `json:"phase,omitempty"`
	Subagent    string         `json:"subagent,omitempty"`
	LanguageID  string         `json:"language_id,omitempty"`
	MinSeverity string         `json:"min_severity,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	AbsPath     string         `json:"abs_path"`
	RelPath     string         `json:"rel_path"`
	HasShebang  bool           `json:"has_shebang,omitempty"`
	Executable  bool           `json:"executable,omitempty"`
	Extensions  map[string]any `json:"extensions,omitempty"`
}

// IsScript reports whether h is a script hook.
func (h Hook) IsScript() bool { return h.Engine == HookEngineScript }

// IsAI reports whether h is an AI compatibility prompt hook.
func (h Hook) IsAI() bool { return h.Engine == HookEngineAI }

// IsLSP reports whether h starts a language server for diagnostics.
func (h Hook) IsLSP() bool { return h.Engine == HookEngineLSP }

// AppliesToFiles reports whether h is triggered by changed files.
func (h Hook) AppliesToFiles() bool { return h.Type == HookTypeFile }

// NormalizedPhase returns the phase with surrounding whitespace removed.
func (h Hook) NormalizedPhase() string { return strings.TrimSpace(h.Phase) }
