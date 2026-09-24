// Package declared reads a directory of declared-by-file things: the services
// and tools a sandbox image, a repository, or a person declares by dropping a
// file in a directory (ADR 26-09-05-409, ADR 0125).
//
// A file is one of two shapes, and which one is decided by its name:
//
//   - A `.yaml` or `.yml` file is metadata and nothing else. Nothing is run from
//     it; whatever it declares is run by something else, or is not run at all.
//   - Any other file is a script carrying a front-matter block (see
//     frontmatter), and the script is what runs.
//
// This package owns the shape: which files are declarations, what the id,
// name and description are, and the problems that make a file unusable
// whatever it declares. What the rest of the fields mean belongs to the reader
// — services, tools — which is also what decides whether a problem stops the
// declaration from running.
package declared

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/discobox-ai/x/frontmatter"
)

// idPattern is what an explicit `id:` may look like: lowercase reverse-DNS.
// Deliberately narrower than what a filename normalizes to, because an id
// written by hand is a name other things match on, and case or punctuation
// differences would be invisible reasons for a match to fail.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*$`)

// File is one declaration, as its file says it.
//
// A file with a Problem is still a File: it is listed, with the reason it
// cannot be used, rather than dropped. A declaration that silently fails to
// appear is indistinguishable from one nobody wrote.
type File struct {
	// ID is derived from the filename — `10-discobox-api.sh` is
	// `discobox-api` — unless the declaration names one with `id:`.
	ID string
	// Name is the display name, defaulted from the filename.
	Name string
	// Description is what the declaration is for, empty when it says nothing.
	Description string
	// Path is the file's absolute path.
	Path string
	// FileName is the file's own name. It, not the ID, orders the listing: the
	// `NN-` prefix stripped from the ID is a statement about where the file
	// sits in the directory.
	FileName string
	// Metadata reports a `.yaml` declaration: the file is only metadata, and
	// there is no script in it to run.
	Metadata bool
	// Fields is the whole metadata block, for the reader to interpret.
	Fields frontmatter.Fields
	// Problem is why this file cannot be used, empty when it can.
	Problem string

	data []byte
	mode fs.FileMode
}

// IsMetadataName reports whether a filename is the metadata-only shape.
func IsMetadataName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// ReadDir reads every declaration in dir, in filename order.
//
// A missing directory is nothing declared, not an error: almost every
// repository declares nothing. Neither is a file that fails to parse — that is
// a File with a Problem. Only a directory that exists and cannot be read is an
// error, since then nothing can be said about what is declared.
//
// Subdirectories and dotfiles are skipped, which is what lets a declaration
// keep what it carries beside it (a tool's files, under a directory named for
// its id).
func ReadDir(dir string) ([]File, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	return ReadFS(os.DirFS(dir), dir)
}

// ReadFS is ReadDir over the root of fsys, for declarations that are not on a
// disk — the ones a binary embeds. root is what each File's Path is joined to,
// and may be empty.
func ReadFS(fsys fs.FS, root string) ([]File, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var out []File
	seen := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		file := parse(fsys, root, name)
		if file.ID == "" {
			continue
		}
		// Two files whose names normalize to one id would otherwise take turns
		// being "the" declaration depending on directory order.
		if first, ok := seen[file.ID]; ok {
			if file.Problem == "" {
				file.Problem = fmt.Sprintf("id %q is already declared by %s", file.ID, first)
			}
		} else {
			seen[file.ID] = name
		}
		out = append(out, file)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FileName < out[j].FileName })
	return out, nil
}

// parse reads one declaration file. It never fails: what went wrong is the
// File's Problem, and the id and name are the filename's whenever the file
// could not say otherwise.
func parse(fsys fs.FS, root, filename string) File {
	file := File{
		ID:       frontmatter.NormalizeID(filename),
		Name:     frontmatter.DefaultName(filename),
		Path:     filepath.Join(root, filename),
		FileName: filename,
		Metadata: IsMetadataName(filename),
	}
	if file.ID == "" {
		return file
	}
	data, err := fs.ReadFile(fsys, filename)
	if err != nil {
		file.Problem = err.Error()
		return file
	}
	file.data = data
	if info, err := fs.Stat(fsys, filename); err == nil {
		file.mode = info.Mode()
	}
	fields, err := decode(data, file.Metadata)
	if err != nil {
		file.Problem = err.Error()
		return file
	}
	file.Fields = fields
	// An explicit id replaces the filename-derived one, and is not normalized:
	// NormalizeID turns a dot into a dash, which would quietly make
	// `ai.discobox.desktop` into `ai-discobox-desktop` and break every match on
	// it. It is validated instead.
	if id := fields.String("id"); id != "" {
		if !idPattern.MatchString(id) {
			file.Problem = fmt.Sprintf("front matter: id: %q is not a lowercase reverse-DNS identifier", id)
			return file
		}
		file.ID = id
	}
	if name := fields.String("name"); name != "" {
		file.Name = name
	}
	file.Description = fields.String("description")
	return file
}

// Data is the file's content as it was read.
func (f File) Data() []byte { return f.data }

// decode reads a file's metadata: the whole of a `.yaml` file, or a script's
// front-matter block.
func decode(data []byte, metadata bool) (frontmatter.Fields, error) {
	if !metadata {
		parsed, err := frontmatter.Parse(data)
		if err != nil {
			return nil, err
		}
		fields, err := frontmatter.Decode(parsed.Meta)
		if err != nil {
			return nil, fmt.Errorf("front matter: %w", err)
		}
		return fields, nil
	}
	fields, err := frontmatter.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	return fields, nil
}

// Map is a field written as a YAML mapping of names to values, sorted by name.
// It is nil when the field is absent, and an error when it is present and not a
// mapping of scalars.
func Map(file File, key string) ([]KeyValue, error) {
	value, ok := file.Fields[frontmatter.NormalizeKey(key)]
	if !ok || value == nil {
		return nil, nil
	}
	mapping, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: is not a mapping", key)
	}
	out := make([]KeyValue, 0, len(mapping))
	for k, v := range mapping {
		switch v.(type) {
		case map[string]any, []any, nil:
			return nil, fmt.Errorf("%s: %q is not a name mapped to a value", key, k)
		}
		out = append(out, KeyValue{Key: k, Value: strings.TrimSpace(fmt.Sprint(v))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// KeyValue is one entry of a mapping field.
type KeyValue struct {
	Key   string
	Value string
}

// ScriptProblem holds a script that is run by its path to the two rules the
// kernel has for that: a shebang to know what to run it with, and the
// executable bit to be allowed to. Empty when both hold. The bit is not asked
// for on Windows, which has no such thing.
func ScriptProblem(file File) string {
	if problem := ShebangProblem(file); problem != "" {
		return problem
	}
	if runtime.GOOS != "windows" && file.mode&0o111 == 0 {
		return "script is not executable"
	}
	return ""
}

// ShebangProblem is the half of ScriptProblem that survives the file being
// copied somewhere else first, where it is given its executable bit.
func ShebangProblem(file File) string {
	if !frontmatter.HasShebangLine(file.data) {
		return "script must start with a shebang line"
	}
	return ""
}
