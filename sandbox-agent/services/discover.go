// Package services runs a repository's declared services: the scripts under
// `.discobox/services` that the sandbox starts for you at boot and that
// `disco box services` and the workspace act on afterwards.
//
// A service is an exec (ADR 0068). This package owns the declaration — where
// it is read from, what it may say, and what makes one invalid — and the
// mapping from a declaration to the exec running it; every runtime mechanic
// (units, shims, logs, transcripts, status) belongs to execs.Manager.
package services

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/frontmatter"
)

// DirName is where services are declared, relative to the sandbox's primary
// source directory. It sits beside `.discobox/hooks` and shares its file
// format (see frontmatter).
const DirName = ".discobox/services"

// Definition is one declared service, as its file says it.
//
// A definition with a Problem is still a definition: it is listed, with the
// reason it cannot run, rather than dropped. A service that silently fails to
// appear is indistinguishable from one nobody declared, and the usual cause —
// a missing executable bit — is invisible in an editor.
type Definition struct {
	// ID is the stable, filename-derived identity: `10-discobox-api.sh` is
	// `discobox-api`. It is what every command addresses a service by.
	ID string
	// Name is the display name, defaulted from the filename.
	Name string
	// Description is what the service is for, empty when it declared none.
	Description string
	// Path is the absolute path to the script inside the sandbox, which is
	// what actually gets run.
	Path string
	// FileName is the file's own name, kept because it — not the ID — is what
	// orders the listing: the `NN-` prefix stripped from the ID is a statement
	// about where the file sits in the directory.
	FileName string
	// Problem is why this declaration cannot run, empty when it can.
	Problem string
}

// Runnable reports whether this declaration can be started.
func (d Definition) Runnable() bool { return d.Problem == "" }

// Discover reads the service declarations under root/.discobox/services.
//
// An absent directory is not an error — most repositories declare no services
// — and neither is a file that fails to parse: that becomes a Definition with
// a Problem. Only a directory that exists and cannot be read is an error, since
// then nothing can be said about what is declared.
func Discover(root string) ([]Definition, error) {
	dir := filepath.Join(root, DirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []Definition
	seen := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		def := parseFile(filepath.Join(dir, name), name)
		if def.ID == "" {
			continue
		}
		// Two files whose names normalize to one id would otherwise take turns
		// being "the" service depending on directory order, and stopping one
		// would stop whichever the last listing happened to resolve to.
		if first, ok := seen[def.ID]; ok {
			def.Problem = fmt.Sprintf("service id %q is already declared by %s", def.ID, first)
		} else {
			seen[def.ID] = name
		}
		out = append(out, def)
	}
	// Filename order, so the `NN-` prefix does what it looks like it does.
	sort.Slice(out, func(i, j int) bool { return out[i].FileName < out[j].FileName })
	return out, nil
}

func parseFile(path, filename string) Definition {
	def := Definition{
		ID:       frontmatter.NormalizeID(filename),
		Name:     frontmatter.DefaultName(filename),
		Path:     path,
		FileName: filename,
	}
	if def.ID == "" {
		return def
	}
	data, err := os.ReadFile(path)
	if err != nil {
		def.Problem = err.Error()
		return def
	}
	parsed, err := frontmatter.Parse(data)
	if err != nil {
		def.Problem = err.Error()
		return def
	}
	fields, err := frontmatter.Decode(parsed.Meta)
	if err != nil {
		def.Problem = "front matter: " + err.Error()
		return def
	}
	if name := fields.String("name"); name != "" {
		def.Name = name
	}
	def.Description = fields.String("description")
	def.Problem = validate(path, data)
	return def
}

// validate holds a service script to the same two rules a hook script is held
// to, and for the same reason: the file is run by path, so the kernel needs a
// shebang to know what to run it with and the bit to be allowed to.
func validate(path string, data []byte) string {
	if !frontmatter.HasShebangLine(data) {
		return "script must start with a shebang line"
	}
	if runtime.GOOS == "windows" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	if info.Mode()&0o111 == 0 {
		return "script is not executable"
	}
	return ""
}
