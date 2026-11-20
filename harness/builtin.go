package harness

import (
	"embed"
	"fmt"
	"io/fs"
	"sync"
)

// builtinDirectories is the inventory of built-in harnesses, in the order the
// server seeds them: the registry, the bundle build and the development image
// watcher all read it, so adding a built-in is a declaration, its adapter and
// this one line (ADR 26-11-06-596 §1). The shell is last because nobody picks
// it for an agent: its reserved slug withholds a run command. It is otherwise
// an ordinary harness (ADR 0043), and not a fallback (ADR 0048).
var builtinDirectories = []string{"claude-code", "codex-cli", "opencode", "copilot", "omp", "pi", "shell"}

// builtinFS holds every built-in's declaration and assets, so the registry
// needs no package per harness and validation sees what the bundle carries.
//
//go:embed */harness.json */adapter.ts all:*/assets
var builtinFS embed.FS

var (
	builtinOnce  sync.Once
	builtinDecls []*Declaration
	builtinErr   error
)

// Builtins returns every built-in harness's validated declaration, in
// inventory order. A declaration that does not validate is a build defect,
// and every test of this package reports it.
func Builtins() ([]*Declaration, error) {
	builtinOnce.Do(func() {
		builtinDecls, builtinErr = LoadBuiltins(builtinFS)
	})
	return builtinDecls, builtinErr
}

// LoadBuiltins reads the inventory's declarations from fsys, the harness tree.
func LoadBuiltins(fsys fs.FS) ([]*Declaration, error) {
	out := make([]*Declaration, 0, len(builtinDirectories))
	seen := map[string]string{}
	for _, dir := range builtinDirectories {
		d, err := LoadDeclaration(fsys, dir)
		if err != nil {
			return nil, err
		}
		if other, ok := seen[d.ID]; ok {
			return nil, fmt.Errorf("harness %s and %s both declare id %s", other, dir, d.ID)
		}
		seen[d.ID] = dir
		out = append(out, d)
	}
	return out, nil
}

// BuiltinDirectories is the inventory, in seeding order.
func BuiltinDirectories() []string {
	return append([]string(nil), builtinDirectories...)
}
