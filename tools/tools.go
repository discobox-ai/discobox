// Package tools reads the tools a discobox can be worked on with: the diff
// viewer and editor its image ships, the reviewer a repository prefers, the IDE
// on this machine — each declared by a file (ADR 0125).
//
// A tool either runs in the discobox or runs here, on the machine the CLI is
// on, handed the discobox's ssh host, working tree or git URL. What runs is
// the declaration's program when the file is `.yaml`, and the file itself when
// it is a script. The file shapes are declared's; this package owns what a
// tool's fields mean, which layers may say what, and how the layers combine.
//
// It sits in the root module because two modules read declarations: the
// sandbox agent lists the image's and the repository's, and the CLI merges
// them with its own and the user's.
package tools

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/declared"
)

// ImageDir is where an image declares the tools it ships, beside its services
// and skills.
const ImageDir = "/usr/local/share/discobox/tools"

// SourceDirName is where a repository declares its tools, relative to its
// root, beside `.discobox/services`.
const SourceDirName = ".discobox/tools"

// DiffID is the tool the launcher opens from a discobox's git summary. The
// image declares it — discobox-review — and it is stated rather than derived
// from a filename so a client can recognize it whatever the file is called.
//
// Unlike a service id in sandboxservices' namespace it is not reserved: a
// repository or a person declaring `id: ai.discobox.diff` replaces the image's
// diff, under whatever name they give it, and the git summary opens theirs.
const DiffID = "ai.discobox.diff"

// Runs is where a tool runs.
type Runs string

const (
	// RunsSandbox is a program run in the discobox, in its primary source
	// directory. It is the default.
	RunsSandbox Runs = "sandbox"
	// RunsHost is a program run on this machine and handed the discobox.
	RunsHost Runs = "host"
)

// Layer is where a declaration came from. The layers are listed from lowest to
// highest: on a shared id, the later one wins (see Merge).
type Layer string

const (
	// LayerBuiltin is what the CLI embeds.
	LayerBuiltin Layer = "builtin"
	// LayerImage is ImageDir, in the discobox.
	LayerImage Layer = "image"
	// LayerSource is SourceDirName, in the discobox's primary source.
	LayerSource Layer = "source"
	// LayerUser is the user's own config directory, on this machine.
	LayerUser Layer = "user"
)

// InSandbox reports a layer whose declarations come out of a discobox, which
// is what decides that it may not put a program on this machine (§5).
func (l Layer) InSandbox() bool { return l == LayerImage || l == LayerSource }

// File is one file a tool carries into the discobox: its name on this machine,
// where it lands under the run user's home, and what the local copy starts as.
// See ADR 26-08-27-302 §7–12 for how it is delivered.
type File struct {
	Name    string
	Home    string
	Default string
}

// Definition is one declared tool, as its file says it.
//
// A definition with a Problem is still listed, with the reason it cannot run.
type Definition struct {
	ID          string
	Name        string
	Description string
	// Key is the single key the tool answers to in a picker, empty when it
	// asks for none.
	Key string

	Runs  Runs
	Layer Layer

	// FileName is the declaring file's own name, which orders the listing.
	FileName string
	// Path is the declaring file. For a script it is what runs: a path in the
	// discobox for the image and source layers, and on this machine for the
	// others.
	Path string
	// Script reports that the file itself is what runs; otherwise Program does.
	Script bool

	// Program is what a `.yaml` tool runs. A host tool may name several, and
	// the first one on PATH is run; ProgramEnv names a variable that picks one
	// instead.
	Program    []string
	ProgramEnv string
	// Args follow the program or the script, with the placeholders Expand
	// resolves.
	Args []string
	// Env is added to the tool's environment, in NAME=value form.
	Env []string

	// Files are what a sandbox tool carries in.
	Files []File

	Problem string

	data []byte
}

// Runnable reports whether the tool can be run.
func (d Definition) Runnable() bool { return d.Problem == "" }

// Label is what a picker calls the tool: its name.
func (d Definition) Label() string {
	if d.Name != "" {
		return d.Name
	}
	return d.ID
}

// ScriptData is the declaring script's content, for a tool that has to be
// copied into the discobox before it can run there.
func (d Definition) ScriptData() []byte { return d.data }

// Discover reads the tools declared in dir, for the given layer.
func Discover(dir string, layer Layer) ([]Definition, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	return DiscoverFS(os.DirFS(dir), dir, layer)
}

// DiscoverFS reads the tools declared at the root of fsys. root is where the
// declarations' paths are joined to.
func DiscoverFS(fsys fs.FS, root string, layer Layer) ([]Definition, error) {
	files, err := declared.ReadFS(fsys, root)
	if err != nil {
		return nil, err
	}
	out := make([]Definition, 0, len(files))
	for _, file := range files {
		out = append(out, definition(fsys, file, layer))
	}
	return out, nil
}

func definition(fsys fs.FS, file declared.File, layer Layer) Definition {
	def := Definition{
		ID:          file.ID,
		Name:        file.Name,
		Description: file.Description,
		Runs:        RunsSandbox,
		Layer:       layer,
		FileName:    file.FileName,
		Path:        file.Path,
		Script:      !file.Metadata,
		Problem:     file.Problem,
		data:        file.Data(),
	}
	if def.Problem != "" {
		return def
	}
	def.Problem = def.read(fsys, file)
	return def
}

// read fills the definition from the file's fields, and reports the first
// thing that keeps it from running.
func (d *Definition) read(fsys fs.FS, file declared.File) string {
	fields := file.Fields
	if runs := Runs(strings.ToLower(fields.String("runs"))); runs != "" {
		d.Runs = runs
	}
	d.Key = fields.String("key")
	d.Program = fields.Strings("program")
	d.ProgramEnv = fields.String("program_env")
	d.Args = fields.Strings("args")
	d.Env = fields.Strings("env")
	files, err := declared.Map(file, "files")
	if err != nil {
		return err.Error()
	}
	for _, entry := range files {
		// The default is kept beside the declaration, under a directory named
		// for the tool. A file with none starts empty. Check refuses a name
		// that is not a plain file name before anything joins it to a path.
		content, _ := fs.ReadFile(fsys, path.Join(d.ID, entry.Key))
		d.Files = append(d.Files, File{Name: entry.Key, Home: strings.TrimPrefix(entry.Value, "/"), Default: string(content)})
	}
	if problem := d.Check(); problem != "" {
		return problem
	}
	if d.Script {
		return d.scriptProblem(file)
	}
	return ""
}

// idPattern is what a tool id may be: a filename-derived id, or a stated
// reverse-DNS one. Both are lowercase letters, digits, dashes and dots, which
// is also what keeps an id safe to name a directory with.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*$`)

// Check is every rule a tool definition has to satisfy, whoever produced it:
// discovery from a file, or a definition a sandbox agent sent over the wire.
// The second is the reason it is separate and exported — a CLI must not take a
// discobox's word that its declarations were checked, since anything with root
// in the box can answer that route (ADR 0125 §5). Empty when the definition can
// run; what it does not cover is the script on disk, which only discovery sees.
func (d Definition) Check() string {
	if !idPattern.MatchString(d.ID) {
		return fmt.Sprintf("id %q is not lowercase letters, digits, dashes and dots", d.ID)
	}
	switch d.Runs {
	case RunsSandbox, RunsHost:
	default:
		return fmt.Sprintf("runs: %q is not sandbox or host", d.Runs)
	}
	switch d.Layer {
	case LayerBuiltin, LayerImage, LayerSource, LayerUser:
	default:
		return fmt.Sprintf("layer %q is not builtin, image, source or user", d.Layer)
	}
	if d.Key != "" {
		r, size := utf8.DecodeRuneInString(d.Key)
		if size != len(d.Key) || !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return fmt.Sprintf("key: %q is not one key", d.Key)
		}
	}
	for _, entry := range d.Env {
		if name, _, ok := strings.Cut(entry, "="); !ok || name == "" {
			return fmt.Sprintf("env: %q is not NAME=value", entry)
		}
	}

	// §5: a program on this machine is yours or the CLI's to declare.
	if d.Runs == RunsHost && d.Layer.InSandbox() {
		return fmt.Sprintf("a tool that runs on your machine cannot be declared by the discobox's %s; declare it in your own tools directory", d.Layer)
	}

	if d.Script {
		if len(d.Program) > 0 || d.ProgramEnv != "" {
			return "program: is for a .yaml declaration; a script is itself what runs"
		}
	} else if len(d.Program) == 0 || strings.TrimSpace(d.Program[0]) == "" {
		return "a .yaml tool names the program: it runs"
	}

	switch d.Runs {
	case RunsSandbox:
		if len(d.Program) > 1 {
			return "a tool that runs in the discobox names one program"
		}
		if d.ProgramEnv != "" {
			return "program-env: picks a program on your machine, and this tool runs in the discobox"
		}
	case RunsHost:
		if len(d.Files) > 0 {
			return "files: are carried into the discobox, and this tool runs on your machine"
		}
	}

	for _, file := range d.Files {
		if !PlainFileName(file.Name) {
			return fmt.Sprintf("files: %q is not a file name", file.Name)
		}
		if strings.TrimPrefix(file.Home, "/") == "" {
			return fmt.Sprintf("files: %s lands nowhere", file.Name)
		}
	}
	return ""
}

// PlainFileName reports whether a name is one file name on every platform the
// CLI runs on: not empty, not a relative step, and with no separator of either
// kind — a backslash is a separator on Windows, and a name that is plain on
// Linux, where a repository can commit it, would otherwise climb out of the
// directory the name is joined to there.
func PlainFileName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:\x00")
}

// scriptProblem is what keeps a script from being run where this tool runs it.
//
//   - A sandbox script from the image or the source is run by its path in the
//     discobox, so it needs what the kernel needs.
//   - One from this machine is copied in and made executable first, so it only
//     needs a shebang.
//   - A host script is run here: a `.ps1` through PowerShell anywhere, a
//     `.cmd` or `.bat` directly on Windows, and anything else by its path, which
//     Windows cannot do.
func (d Definition) scriptProblem(file declared.File) string {
	if d.Runs == RunsSandbox {
		if d.Layer.InSandbox() {
			return declared.ScriptProblem(file)
		}
		return declared.ShebangProblem(file)
	}
	switch strings.ToLower(filepath.Ext(file.FileName)) {
	case ".ps1":
		return ""
	case ".cmd", ".bat":
		if runtime.GOOS == "windows" {
			return ""
		}
		return "a .cmd or .bat script runs only on Windows"
	}
	if runtime.GOOS == "windows" {
		return "a script run on Windows is a .ps1, .cmd or .bat"
	}
	return declared.ScriptProblem(file)
}

// Merge combines layers given lowest first: on a shared id, the later layer's
// declaration replaces the earlier one.
//
// One replacement is refused. A declaration out of the discobox cannot replace
// a tool that runs on this machine (ADR 0125 §5): it is kept, with a problem,
// and the host tool stays. What a key on your machine does is not the box's to
// change.
//
// The result is in filename order, then id. Find is how to look one up, since a
// refused declaration shares its id with the tool it tried to replace.
func Merge(layers ...[]Definition) []Definition {
	winners := map[string]int{}
	var out []Definition
	for _, layer := range layers {
		for _, def := range layer {
			i, seen := winners[def.ID]
			if !seen {
				winners[def.ID] = len(out)
				out = append(out, def)
				continue
			}
			held := out[i]
			if held.Runs == RunsHost && held.Problem == "" && def.Layer.InSandbox() {
				def.Problem = fmt.Sprintf("id %q is a tool that runs on your machine, declared by %s; the discobox's %s cannot replace it", def.ID, held.Layer, def.Layer)
				out = append(out, def)
				continue
			}
			out[i] = def
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FileName != out[j].FileName {
			return out[i].FileName < out[j].FileName
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Lookup is the tool a person named: by id, or else by a name exactly one
// declared tool wears — `discobox tools diff` for ai.discobox.diff. An id wins
// over a name, so a tool can never be shadowed by another's display name.
func Lookup(defs []Definition, name string) (Definition, bool) {
	if def, ok := Find(defs, name); ok {
		return def, true
	}
	id := ""
	for _, def := range defs {
		if def.Name != name || def.ID == id {
			continue
		}
		if id != "" {
			return Definition{}, false
		}
		id = def.ID
	}
	if id == "" {
		return Definition{}, false
	}
	return Find(defs, id)
}

// Find is the tool with an id: the one Merge let stand, whenever one did.
func Find(defs []Definition, id string) (Definition, bool) {
	var found Definition
	ok := false
	for _, def := range defs {
		if def.ID != id {
			continue
		}
		if def.Problem == "" {
			return def, true
		}
		if !ok {
			found, ok = def, true
		}
	}
	return found, ok
}
