// Package parser discovers, parses, normalizes, and validates hook definition
// files under .discobox/hooks.
package parser

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	hooks "github.com/obot-platform/discobox/hooks"
	"gopkg.in/yaml.v3"
)

const (
	HooksDirName     = ".discobox/hooks"
	GlobalIgnoreName = "ignore"
)

var commonHookExts = map[string]struct{}{
	".sh": {}, ".bash": {}, ".zsh": {}, ".fish": {},
	".py": {}, ".rb": {}, ".pl": {},
	".js": {}, ".mjs": {}, ".cjs": {}, ".ts": {},
	".md": {}, ".markdown": {}, ".txt": {},
}

var hookIDNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
var hookIDOrderPrefix = regexp.MustCompile(`^[0-9]+-+`)
var phaseNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Discovery is the result of discovering a repository's hook directory.
type Discovery struct {
	Hooks               []hooks.Hook `json:"hooks"`
	GlobalIgnore        []string     `json:"global_ignore,omitempty"`
	HooksDir            string       `json:"hooks_dir"`
	GlobalIgnoreAbsPath string       `json:"global_ignore_abs_path,omitempty"`
}

// ValidationError identifies a hook validation problem and, when known, the
// offending field and path.
type ValidationError struct {
	Path  string
	Field string
	Err   error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if e.Path != "" {
		parts = append(parts, e.Path)
	}
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *ValidationError) Unwrap() error { return e.Err }

// ValidationErrors collects more than one hook validation failure.
type ValidationErrors []error

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return ""
	}
	msgs := make([]string, len(e))
	for i := range e {
		msgs[i] = e[i].Error()
	}
	return strings.Join(msgs, "; ")
}

func (e ValidationErrors) Unwrap() []error { return []error(e) }

func fieldError(path, field string, format string, args ...any) error {
	return &ValidationError{Path: path, Field: field, Err: fmt.Errorf(format, args...)}
}

// Discover reads direct hook files from repoRoot/.discobox/hooks. An absent hooks
// directory is not an error.
func Discover(repoRoot string) (*Discovery, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	hooksDir := filepath.Join(repoRoot, HooksDirName)
	out := &Discovery{HooksDir: hooksDir}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}

	var errs ValidationErrors
	for _, entry := range entries {
		name := entry.Name()
		if name == GlobalIgnoreName && !entry.IsDir() {
			ignorePath := filepath.Join(hooksDir, name)
			patterns, err := ParseGlobalIgnoreFile(ignorePath)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out.GlobalIgnore = patterns
			out.GlobalIgnoreAbsPath = ignorePath
			continue
		}
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(hooksDir, name)
		hook, err := ParseFile(repoRoot, path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out.Hooks = append(out.Hooks, hook)
	}

	sort.Slice(out.Hooks, func(i, j int) bool {
		if out.Hooks[i].Name == out.Hooks[j].Name {
			return out.Hooks[i].ID < out.Hooks[j].ID
		}
		return out.Hooks[i].Name < out.Hooks[j].Name
	})
	if len(errs) > 0 {
		return out, errs
	}
	return out, nil
}

// ParseGlobalIgnoreFile parses .discobox/hooks/ignore.
func ParseGlobalIgnoreFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, filepath.ToSlash(line))
	}
	return patterns, nil
}

// ParseFile parses and validates a single hook file.
func ParseFile(repoRoot, path string) (hooks.Hook, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return hooks.Hook{}, err
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return hooks.Hook{}, err
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return hooks.Hook{}, err
	}
	mode := fs.FileMode(0)
	if info, err := os.Stat(absPath); err == nil {
		mode = info.Mode()
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	parsed, err := parseFrontMatter(data)
	if err != nil {
		return hooks.Hook{}, &ValidationError{Path: rel, Field: "front_matter", Err: err}
	}
	meta, err := decodeMetadata(parsed.meta)
	if err != nil {
		return hooks.Hook{}, &ValidationError{Path: rel, Field: "front_matter", Err: err}
	}
	return buildHook(rel, absPath, filepath.Base(path), data, mode, parsed, meta)
}

type parsedFrontMatter struct {
	meta       []byte
	body       string
	hasShebang bool
	delimiter  string
}

func parseFrontMatter(data []byte) (parsedFrontMatter, error) {
	text := string(data)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return parsedFrontMatter{}, errors.New("missing front matter")
	}
	start := 0
	hasShebang := strings.HasPrefix(strings.TrimSuffix(lines[0], "\n"), "#!")
	if hasShebang {
		start = 1
	}
	if start >= len(lines) {
		return parsedFrontMatter{}, errors.New("missing opening delimiter")
	}
	delim := delimiterKind(lines[start])
	if delim == "" {
		return parsedFrontMatter{}, errors.New("missing opening delimiter")
	}

	var meta bytes.Buffer
	closeIndex := -1
	for i := start + 1; i < len(lines); i++ {
		if delimiterKind(lines[i]) == delim {
			closeIndex = i
			break
		}
		line := strings.TrimSuffix(lines[i], "\n")
		line = strings.TrimSuffix(line, "\r")
		switch delim {
		case "#---":
			line = stripCommentPrefix(line, "#")
		case "//---":
			line = stripCommentPrefix(line, "//")
		}
		meta.WriteString(line)
		meta.WriteByte('\n')
	}
	if closeIndex == -1 {
		return parsedFrontMatter{}, errors.New("missing closing delimiter")
	}
	body := strings.Join(lines[closeIndex+1:], "")
	return parsedFrontMatter{meta: meta.Bytes(), body: body, hasShebang: hasShebang, delimiter: delim}, nil
}

func delimiterKind(line string) string {
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
	switch trimmed {
	case "---", "#---", "//---":
		return trimmed
	default:
		return ""
	}
}

func stripCommentPrefix(line, prefix string) string {
	trimmed := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(trimmed, prefix) {
		trimmed = strings.TrimPrefix(trimmed, prefix)
		if strings.HasPrefix(trimmed, " ") || strings.HasPrefix(trimmed, "\t") {
			trimmed = strings.TrimLeft(trimmed, " \t")
		}
		return trimmed
	}
	return line
}

type metadata struct {
	Name        string
	Type        string
	Description string
	Engine      string
	RunAs       string
	Blocking    bool
	Pattern     string
	Ignore      []string
	Phase       string
	Subagent    string
	LanguageID  string
	MinSeverity string
	Extensions  map[string]any
}

func decodeMetadata(data []byte) (metadata, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return metadata{}, err
	}
	if raw == nil {
		raw = map[string]any{}
	}
	m := metadata{Extensions: map[string]any{}}
	for k, v := range raw {
		key := normalizeKey(k)
		switch key {
		case "name":
			m.Name = asString(v)
		case "type":
			m.Type = strings.ToLower(asString(v))
		case "description":
			m.Description = asString(v)
		case "engine":
			m.Engine = strings.ToLower(asString(v))
		case "run_as":
			m.RunAs = strings.ToLower(asString(v))
		case "blocking":
			m.Blocking = asBool(v)
		case "pattern":
			m.Pattern = filepath.ToSlash(asString(v))
		case "ignore", "exclude":
			m.Ignore = append(m.Ignore, asStringSlice(v)...)
		case "phase":
			m.Phase = strings.ToLower(strings.TrimSpace(asString(v)))
		case "subagent":
			m.Subagent = asString(v)
		case "language_id", "language":
			m.LanguageID = asString(v)
		case "min_severity", "minimum_severity":
			m.MinSeverity = strings.ToLower(asString(v))
		default:
			m.Extensions[key] = v
		}
	}
	for i := range m.Ignore {
		m.Ignore[i] = filepath.ToSlash(strings.TrimSpace(m.Ignore[i]))
	}
	m.Ignore = compactStrings(m.Ignore)
	return m, nil
}

func normalizeKey(k string) string {
	k = strings.TrimSpace(strings.ToLower(k))
	k = strings.ReplaceAll(k, "-", "_")
	return k
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return strings.TrimSpace(t.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "yes", "y", "on", "1":
			return true
		}
	case int, int64, uint64:
		return fmt.Sprint(t) != "0"
	}
	return false
}

func asStringSlice(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, asString(item))
		}
		return out
	case []string:
		return t
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []string{t}
	default:
		if s := asString(t); s != "" {
			return []string{s}
		}
		return nil
	}
}

func compactStrings(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func buildHook(rel, abs, filename string, data []byte, mode fs.FileMode, pf parsedFrontMatter, m metadata) (hooks.Hook, error) {
	id := NormalizeID(filename)
	if id == "" {
		return hooks.Hook{}, fieldError(rel, "id", "filename cannot produce stable hook ID")
	}
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = DefaultName(filename)
	}
	engine := hooks.HookEngine(m.Engine)
	if engine == "" {
		engine = hooks.HookEngineScript
	}
	runAs := hooks.RunAs(m.RunAs)
	if runAs == "" {
		runAs = hooks.RunAsUser
	}
	h := hooks.Hook{
		ID: id, Name: name, Description: m.Description,
		Type: hooks.HookType(m.Type), Engine: engine, RunAs: runAs, Blocking: m.Blocking,
		Pattern: m.Pattern, Ignore: m.Ignore, Phase: m.Phase,
		Subagent: m.Subagent, LanguageID: m.LanguageID, MinSeverity: m.MinSeverity,
		AbsPath: abs, RelPath: rel, HasShebang: pf.hasShebang,
		Executable: mode&0111 != 0, Extensions: m.Extensions,
	}
	if len(h.Extensions) == 0 {
		h.Extensions = nil
	}
	if h.Engine == hooks.HookEngineAI {
		h.Prompt = strings.TrimSpace(pf.body)
	}
	if err := validateHook(h, data); err != nil {
		return hooks.Hook{}, err
	}
	return h, nil
}

func validateHook(h hooks.Hook, data []byte) error {
	if h.Type == "" {
		return fieldError(h.RelPath, "type", "missing required field")
	}
	if !h.Type.Valid() {
		return fieldError(h.RelPath, "type", "unsupported hook type %q", h.Type)
	}
	if h.Engine == "" || !h.Engine.Valid() {
		return fieldError(h.RelPath, "engine", "unsupported hook engine %q", h.Engine)
	}
	if h.Engine == hooks.HookEngineBuiltin {
		return fieldError(h.RelPath, "engine", "builtin hooks require a registered implementation")
	}
	if !h.RunAs.Valid() {
		return fieldError(h.RelPath, "run_as", "unsupported run_as %q", h.RunAs)
	}
	if h.Type == hooks.HookTypeFile && strings.TrimSpace(h.Pattern) == "" {
		return fieldError(h.RelPath, "pattern", "file hooks require pattern")
	}
	if h.Phase != "" {
		if h.Phase == "all" {
			return fieldError(h.RelPath, "phase", "phase %q is reserved", h.Phase)
		}
		if !phaseNamePattern.MatchString(h.Phase) {
			return fieldError(h.RelPath, "phase", "invalid phase %q: use lowercase letters, digits, hyphens, and underscores", h.Phase)
		}
	}
	if h.Engine == hooks.HookEngineScript {
		if !hasFirstLineShebang(data) {
			return fieldError(h.RelPath, "shebang", "script hooks require shebang as first line")
		}
		if runtime.GOOS != "windows" && !h.Executable {
			return fieldError(h.RelPath, "mode", "script hooks must be executable")
		}
	}
	if h.Engine == hooks.HookEngineLSP {
		if !hasFirstLineShebang(data) {
			return fieldError(h.RelPath, "shebang", "lsp hooks require shebang as first line")
		}
		if runtime.GOOS != "windows" && !h.Executable {
			return fieldError(h.RelPath, "mode", "lsp hooks must be executable")
		}
		if strings.TrimSpace(h.LanguageID) == "" {
			return fieldError(h.RelPath, "language_id", "lsp hooks require language_id")
		}
		if h.Type != hooks.HookTypeFile {
			return fieldError(h.RelPath, "type", "lsp hooks require type file")
		}
		switch h.MinSeverity {
		case "", "error", "warning", "information", "info", "hint":
		default:
			return fieldError(h.RelPath, "min_severity", "unsupported severity %q", h.MinSeverity)
		}
	}
	return nil
}

func hasFirstLineShebang(data []byte) bool {
	return bytes.HasPrefix(data, []byte("#!"))
}

// NormalizeID returns the stable filename-derived hook ID.
func NormalizeID(filename string) string {
	base := filepath.Base(filename)
	if ext := strings.ToLower(filepath.Ext(base)); ext != "" {
		if _, ok := commonHookExts[ext]; ok {
			base = strings.TrimSuffix(base, filepath.Ext(base))
		}
	}
	base = strings.ToLower(base)
	base = strings.Trim(hookIDNonAlnum.ReplaceAllString(base, "-"), "-")
	base = hookIDOrderPrefix.ReplaceAllString(base, "")
	return strings.Trim(base, "-")
}

// DefaultName returns a readable filename-derived hook name.
func DefaultName(filename string) string {
	id := NormalizeID(filename)
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if _, err := fmt.Sscanf(p, "%d", new(int)); err == nil {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}
