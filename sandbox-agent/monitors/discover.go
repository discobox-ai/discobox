// Package monitors is the sandbox's event monitors (ADR 26-10-31-073): the
// scripts an agent enables to watch something on its behalf — a repository, a
// log, a URL — whose events wait in a queue until the agent pops them.
//
// A monitor is declared the way a service or a tool is, by a script with front
// matter in the repository's `.discobox/monitors` or the image's BuiltinDir. It
// does nothing until an instance of it is enabled: a file in Dirs.Enabled,
// naming the monitor and its options. The agent's Reconciler keeps one exec
// running per enabled file; what that exec runs is the instance's Runner (the
// agent's `monitor-run` subcommand), which runs the script and queues what it
// prints. The queue is a directory of files, so `discobox-monitor pop` is a
// rename and nothing has to be asked of the agent to read it.
package monitors

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/declared"
)

// DirName is where a repository declares its monitors, relative to the
// sandbox's primary source directory, beside `.discobox/services` and
// `.discobox/tools`. It is also where an agent writes a new one.
const DirName = ".discobox/monitors"

// BuiltinDir is where the image declares the monitors it ships.
const BuiltinDir = "/usr/local/share/discobox/monitors"

// Supported reports whether monitors run on this platform. A runner runs a
// monitor by its path, which is a `#!` script: Linux and macOS run one, and
// Windows does not, so a Windows sandbox has no monitors yet (ADR 26-10-31-073,
// Deferred) rather than ones that fail on every run.
const Supported = runtime.GOOS != "windows"

// MinInterval is the shortest polling interval a declaration may ask for. A
// monitor polling faster than this is a loop, and belongs in streaming mode.
const MinInterval = time.Second

// optionName is what an option may be called: it becomes an environment
// variable's suffix, so it is held to what one can spell after upper-casing.
var optionName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Option is one option a monitor takes.
type Option struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// EnvName is the environment variable the option's value reaches the monitor
// in: `pr` is DISCOBOX_OPT_PR, `max-age` DISCOBOX_OPT_MAX_AGE.
func (o Option) EnvName() string { return OptionEnvName(o.Name) }

// OptionEnvName is EnvName for an option known only by name.
func OptionEnvName(name string) string {
	return "DISCOBOX_OPT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// Definition is one declared monitor, as its file says it.
//
// A definition with a Problem is still listed, with the reason it cannot run:
// a monitor that silently fails to appear is indistinguishable from one nobody
// wrote.
type Definition struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Path        string   `json:"path"`
	FileName    string   `json:"-"`
	Options     []Option `json:"options,omitempty"`
	// Interval is how often a polling monitor is run. Zero is a streaming
	// monitor: run once, and kept running.
	Interval time.Duration `json:"interval,omitempty"`
	// Builtin marks a declaration that came from the image rather than the
	// repository.
	Builtin bool   `json:"builtin,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// Option is the named option, and whether the monitor declares it.
func (d Definition) Option(name string) (Option, bool) {
	for _, o := range d.Options {
		if o.Name == name {
			return o, true
		}
	}
	return Option{}, false
}

// Discover reads both declaration directories. A repository's monitor wins on
// a shared id, as its services and tools do. Missing directories declare
// nothing; only a directory that exists and cannot be read is an error.
func Discover(builtinDir, sourceRoot string) ([]Definition, error) {
	builtin, err := discoverDir(builtinDir, true)
	if err != nil {
		return nil, err
	}
	var project []Definition
	if strings.TrimSpace(sourceRoot) != "" {
		if project, err = discoverDir(filepath.Join(sourceRoot, DirName), false); err != nil {
			return nil, err
		}
	}
	overridden := map[string]bool{}
	for _, def := range project {
		overridden[def.ID] = true
	}
	out := make([]Definition, 0, len(builtin)+len(project))
	for _, def := range builtin {
		if !overridden[def.ID] {
			out = append(out, def)
		}
	}
	return append(out, project...), nil
}

// Load reads the one declaration at path, which is what a runner does before
// each run, so an edit to a monitor reaches the instances of it without
// enabling them again.
func Load(path string, builtin bool) (Definition, error) {
	defs, err := discoverDir(filepath.Dir(path), builtin)
	if err != nil {
		return Definition{}, err
	}
	for _, def := range defs {
		if def.Path == path {
			return def, nil
		}
	}
	return Definition{}, fmt.Errorf("monitor %s: no such declaration", path)
}

// Find is the definition with id.
func Find(defs []Definition, id string) (Definition, bool) {
	for _, def := range defs {
		if def.ID == id {
			return def, true
		}
	}
	return Definition{}, false
}

func discoverDir(dir string, builtin bool) ([]Definition, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	files, err := declared.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(files))
	for _, file := range files {
		def := definition(file)
		def.Builtin = builtin
		out = append(out, def)
	}
	return out, nil
}

func definition(file declared.File) Definition {
	def := Definition{
		ID:          file.ID,
		Name:        file.Name,
		Description: file.Description,
		Path:        file.Path,
		FileName:    file.FileName,
		Problem:     file.Problem,
	}
	if def.Problem != "" {
		return def
	}
	if file.Metadata {
		def.Problem = "a monitor is a script: a .yaml declaration has nothing to run"
		return def
	}
	if raw := file.Fields.String("interval"); raw != "" {
		interval, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			def.Problem = fmt.Sprintf("front matter: interval: %q is not a duration (60s, 5m)", raw)
			return def
		case interval < MinInterval:
			def.Problem = fmt.Sprintf("front matter: interval: %s is shorter than %s; a monitor that loops is a streaming one, with no interval", interval, MinInterval)
			return def
		}
		def.Interval = interval
	}
	raw, present := file.Fields["options"]
	if present && raw == nil {
		// What a nested block reads as once its indentation is gone: an empty
		// `options:` and its entries as fields of their own. Silently taking
		// no options would refuse every option the author meant to declare.
		def.Problem = "front matter: options: is empty; write each option on its own line as `- {name: repo, required: true, description: ...}`, since a commented block loses its indentation"
		return def
	}
	options, err := parseOptions(raw)
	if err != nil {
		def.Problem = "front matter: options: " + err.Error()
		return def
	}
	def.Options = options
	def.Problem = declared.ScriptProblem(file)
	return def
}

// parseOptions reads `options:`, a list of one-line mappings. It is a list of
// flow mappings rather than a nested block because a commented front-matter
// block loses its indentation: frontmatter strips what follows the `#`.
func parseOptions(value any) ([]Option, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("is not a list; write each option as `- {name: repo, required: true, description: ...}`")
	}
	out := make([]Option, 0, len(list))
	seen := map[string]bool{}
	for i, item := range list {
		var opt Option
		switch v := item.(type) {
		case string:
			opt.Name = strings.TrimSpace(v)
		case map[string]any:
			for key, field := range v {
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "name":
					opt.Name = strings.TrimSpace(fmt.Sprint(field))
				case "description":
					opt.Description = strings.TrimSpace(fmt.Sprint(field))
				case "required":
					required, ok := field.(bool)
					if !ok {
						return nil, fmt.Errorf("option %d: required: is not true or false", i+1)
					}
					opt.Required = required
				default:
					return nil, fmt.Errorf("option %d: unknown field %q (name, description, required)", i+1, key)
				}
			}
		default:
			return nil, fmt.Errorf("option %d: is not a name or a mapping", i+1)
		}
		if !optionName.MatchString(opt.Name) {
			return nil, fmt.Errorf("option %d: name %q must be lowercase letters, digits, - and _, starting with a letter", i+1, opt.Name)
		}
		if seen[opt.Name] {
			return nil, fmt.Errorf("option %q is declared twice", opt.Name)
		}
		seen[opt.Name] = true
		out = append(out, opt)
	}
	return out, nil
}
