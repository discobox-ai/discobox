package agent

import (
	"context"
	"strings"
)

// Kind identifies a coding agent host that prompter can drive.
type Kind string

const (
	KindUnknown    Kind = "unknown"
	KindDiscobot   Kind = "discobot"
	KindClaudeCode Kind = "claude-code"
	KindCodex      Kind = "codex"
	KindOpenCode   Kind = "opencode"
	KindGeminiCLI  Kind = "gemini-cli"
)

// Detected describes the current agent host inferred from local process state.
type Detected struct {
	Kind   Kind
	Source string
}

// Process describes a sanitized ancestor process. It intentionally stores only
// executable identity, not full command-line arguments or environment values.
type Process struct {
	PID  int
	PPID int
	Comm string
	Exe  string
}

// RunRequest is the normalized command contract prompter adapters receive.
type RunRequest struct {
	// SessionID is an optional caller-owned persistent session key. Empty means
	// the run is ephemeral and should not be saved for later resume.
	SessionID   string
	Prompt      string
	Agent       string
	Model       string
	Reasoning   string
	ServiceTier string
	Workdir     string
}

// RunResult is the normalized JSON response emitted by prompter.
type RunResult struct {
	Text      string `json:"text"`
	SessionID string `json:"sessionID,omitempty"`
}

// Runner starts a new coding-agent session in the requested working directory.
type Runner interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

// Detector contains one agent-specific detection implementation.
type Detector interface {
	Kind() Kind
	NeedsEnvironment() bool
	NeedsProcessAncestry() bool
	Detect(Context) (Detected, bool)
}

// Context is the input selected for one detector. Fields are populated only
// when the detector asks for them through its Needs* methods.
type Context struct {
	Environment     map[string]string
	ProcessAncestry []Process
}

// Sources lazily collects reusable detection inputs.
type Sources struct {
	EnvironmentProvider     func() map[string]string
	ProcessAncestryProvider func() []Process

	environmentLoaded     bool
	processAncestryLoaded bool
	environment           map[string]string
	processAncestry       []Process
}

func (s *Sources) environmentContext() map[string]string {
	if !s.environmentLoaded {
		s.environmentLoaded = true
		if s.EnvironmentProvider != nil {
			s.environment = s.EnvironmentProvider()
		}
		if s.environment == nil {
			s.environment = map[string]string{}
		}
	}
	return s.environment
}

func (s *Sources) processAncestryContext() []Process {
	if !s.processAncestryLoaded {
		s.processAncestryLoaded = true
		if s.ProcessAncestryProvider != nil {
			s.processAncestry = s.ProcessAncestryProvider()
		}
	}
	return s.processAncestry
}

// Environ converts a process environment slice into a lookup map.
func Environ(values []string) map[string]string {
	env := make(map[string]string, len(values))
	for _, value := range values {
		name, val, ok := strings.Cut(value, "=")
		if ok {
			env[name] = val
		}
	}
	return env
}

// Detect preserves the test-friendly environment-only entrypoint.
func Detect(env map[string]string) Detected {
	return DetectWith(DefaultDetectors(), &Sources{
		EnvironmentProvider: func() map[string]string {
			return env
		},
	})
}

// DetectFrom preserves the explicit environment/process entrypoint.
func DetectFrom(env map[string]string, ancestry []Process) Detected {
	return DetectWith(DefaultDetectors(), &Sources{
		EnvironmentProvider: func() map[string]string {
			return env
		},
		ProcessAncestryProvider: func() []Process {
			return ancestry
		},
	})
}

// DetectWith loops through detectors in order and returns the first match.
func DetectWith(detectors []Detector, sources *Sources) Detected {
	if sources == nil {
		sources = &Sources{}
	}
	if detected, ok := detectWithFilter(detectors, sources, false); ok {
		return detected
	}
	if detected, ok := detectWithFilter(detectors, sources, true); ok {
		return detected
	}
	return Detected{Kind: KindUnknown}
}

func detectWithFilter(detectors []Detector, sources *Sources, wantsProcess bool) (Detected, bool) {
	for _, detector := range detectors {
		if detector.NeedsProcessAncestry() != wantsProcess {
			continue
		}
		var ctx Context
		if detector.NeedsEnvironment() {
			ctx.Environment = sources.environmentContext()
		}
		if detector.NeedsProcessAncestry() {
			ctx.ProcessAncestry = sources.processAncestryContext()
		}
		if detected, ok := detector.Detect(ctx); ok {
			return detected, true
		}
	}
	return Detected{}, false
}

// StaticDetector is a small helper for agent packages whose rules are simple
// environment or process-name matches.
type StaticDetector struct {
	AgentKind    Kind
	EnvKeys      []string
	EnvAllKeys   []string
	EnvEquals    map[string]string
	EnvContains  map[string]string
	ProcessNames []string
}

func (d StaticDetector) Kind() Kind {
	return d.AgentKind
}

func (d StaticDetector) NeedsEnvironment() bool {
	return len(d.EnvKeys) > 0 || len(d.EnvAllKeys) > 0 || len(d.EnvEquals) > 0 || len(d.EnvContains) > 0
}

func (d StaticDetector) NeedsProcessAncestry() bool {
	return len(d.ProcessNames) > 0
}

func (d StaticDetector) Detect(ctx Context) (Detected, bool) {
	if len(d.EnvAllKeys) > 0 {
		if hasAllEnvKeys(ctx.Environment, d.EnvAllKeys) && envEqualsAll(ctx.Environment, d.EnvEquals) && envContainsAll(ctx.Environment, d.EnvContains) {
			return Detected{Kind: d.AgentKind, Source: "env:" + d.EnvAllKeys[0]}, true
		}
		return d.detectProcess(ctx)
	}
	for _, key := range d.EnvKeys {
		if _, ok := ctx.Environment[key]; ok {
			return Detected{Kind: d.AgentKind, Source: "env:" + key}, true
		}
	}
	for key, want := range d.EnvEquals {
		if ctx.Environment[key] == want {
			return Detected{Kind: d.AgentKind, Source: "env:" + key}, true
		}
	}
	for key, want := range d.EnvContains {
		if strings.Contains(ctx.Environment[key], want) {
			return Detected{Kind: d.AgentKind, Source: "env:" + key}, true
		}
	}
	return d.detectProcess(ctx)
}

func (d StaticDetector) detectProcess(ctx Context) (Detected, bool) {
	for _, process := range ctx.ProcessAncestry {
		for _, name := range ProcessNames(process) {
			if stringIn(name, d.ProcessNames) {
				return Detected{Kind: d.AgentKind, Source: "process:" + name}, true
			}
		}
	}
	return Detected{}, false
}

// ProcessNames returns normalized executable names for a process.
func ProcessNames(process Process) []string {
	names := make([]string, 0, 2)
	if process.Comm != "" {
		names = append(names, NormalizeProcessName(process.Comm))
	}
	if process.Exe != "" {
		names = append(names, NormalizeProcessName(process.Exe))
	}
	return names
}

// NormalizeProcessName extracts a lowercase executable basename.
func NormalizeProcessName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimSuffix(value, ".exe")
	if index := strings.LastIndexAny(value, `/\`); index >= 0 {
		value = value[index+1:]
	}
	return value
}

func stringIn(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func hasAllEnvKeys(env map[string]string, keys []string) bool {
	for _, key := range keys {
		if _, ok := env[key]; !ok {
			return false
		}
	}
	return true
}

func envEqualsAll(env map[string]string, equals map[string]string) bool {
	for key, want := range equals {
		if env[key] != want {
			return false
		}
	}
	return true
}

func envContainsAll(env map[string]string, contains map[string]string) bool {
	for key, want := range contains {
		if !strings.Contains(env[key], want) {
			return false
		}
	}
	return true
}

// RunnerFor resolves the detected host to a CLI adapter.
func RunnerFor(detected Detected) (Runner, bool) {
	if _, ok := PromptDriverFor(detected.Kind); !ok {
		return nil, false
	}
	return NewCLIRunner(detected.Kind), true
}
