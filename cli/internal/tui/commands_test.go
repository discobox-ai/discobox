package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// Every key the list, the tree, the window's leader and the panes bind is the
// registry's (ADR 26-11-06-671 §4, extended to every frontend by 26-11-07-598
// §3): a command's, or a gesture's, which the registry declares as one. A
// screen's own keys (an issue tab, the audit screen, a dialog) are that
// screen's, and not read here. A new action cannot quietly acquire a
// shortcut without searchable metadata, and a pane cannot bind a key the
// registry does not hand it.
func TestConsoleCommandBindingCoverage(t *testing.T) {
	keys, ids := map[string]bool{}, map[string]bool{}
	note := func(b console.TUIBinding) {
		for _, seq := range b.Leader {
			for _, key := range seq {
				keys[key] = true
			}
		}
		for _, key := range slices.Concat(b.Keys, b.Chords, b.Menu) {
			keys[key] = true
		}
	}
	for _, c := range console.Commands() {
		note(c.Bindings.TUI)
		ids[c.ID] = true
	}
	for _, g := range console.Gestures() {
		note(g.Bindings.TUI)
	}
	for _, page := range treePages {
		if !ids[page.id] {
			t.Errorf("tree entry %q opens %s, which is no command", page.label, page.id)
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	constants := map[string]string{}
	parsed := map[string]*ast.File{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed[path] = f
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
				return true
			}
			if value, ok := registryKey(spec.Values[0]); ok {
				constants[spec.Names[0].Name] = value
			}
			return true
		})
	}
	// A key is a literal, a name for one, or read from the registry.
	resolve := func(expr ast.Expr) (string, bool) {
		if value, ok := registryKey(expr); ok {
			return value, true
		}
		if id, ok := expr.(*ast.Ident); ok {
			value, ok := constants[id.Name]
			return value, ok
		}
		return "", false
	}
	check := func(expr ast.Expr, where string) {
		key, ok := resolve(expr)
		if !ok {
			t.Errorf("unclassified computed window binding in %s", where)
			return
		}
		if !keys[key] {
			t.Errorf("window binding %q in %s is neither a command's nor a gesture's in the registry", key, where)
		}
	}
	isKey := func(expr ast.Expr) bool {
		if id, ok := expr.(*ast.Ident); ok && id.Name == "key" {
			return true
		}
		if call, ok := expr.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "keyName" {
				return true
			}
		}
		return false
	}
	// What the leader's key runs, the window answers by command ID.
	isID := func(expr ast.Expr) bool {
		id, ok := expr.(*ast.Ident)
		return ok && id.Name == "id"
	}
	checkID := func(expr ast.Expr, where string) {
		lit, ok := expr.(*ast.BasicLit)
		if !ok {
			return
		}
		id, _ := strconv.Unquote(lit.Value)
		if !ids[id] {
			t.Errorf("%s answers %q, which is no command", where, id)
		}
	}
	for _, path := range []string{"model.go", "tree.go"} {
		for _, decl := range parsed[path].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "updateKey" && fn.Name.Name != "updateList" && fn.Name.Name != "treeKey" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sw, ok := n.(*ast.SwitchStmt); ok && sw.Tag != nil {
					for _, stmt := range sw.Body.List {
						for _, expr := range stmt.(*ast.CaseClause).List {
							switch {
							case isKey(sw.Tag):
								check(expr, fn.Name.Name)
							case isID(sw.Tag):
								checkID(expr, fn.Name.Name)
							}
						}
					}
				}
				if bin, ok := n.(*ast.BinaryExpr); ok && bin.Op == token.EQL {
					if isKey(bin.X) {
						if _, ok := resolve(bin.Y); ok {
							check(bin.Y, fn.Name.Name)
						}
					}
					if isID(bin.X) {
						checkID(bin.Y, fn.Name.Name)
					}
				}
				return true
			})
		}
	}

	// A pane binds only what paneOptions reads out of the registry: a leader
	// sequence's key, a leaderless chord, and the one chord map per leading
	// key it gathers them into. A key written into a binding by hand fails
	// here.
	ast.Inspect(parsed["pane.go"], func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		switch sel.Sel.Name {
		case "WithPrefixBinding", "WithRepeatingPrefixBinding", "WithBinding":
			switch arg := call.Args[0].(type) {
			case *ast.IndexExpr:
				if seq, ok := arg.X.(*ast.Ident); !ok || seq.Name != "seq" {
					t.Errorf("%s binds a key that is not a registry sequence's", sel.Sel.Name)
				}
			case *ast.Ident:
				if arg.Name != "chord" {
					t.Errorf("%s binds %s, which is not a registry chord", sel.Sel.Name, arg.Name)
				}
			default:
				t.Errorf("%s binds a key written by hand", sel.Sel.Name)
			}
		case "WithPrefixChord":
			lead, ok := call.Args[0].(*ast.Ident)
			bindings, ok2 := call.Args[1].(*ast.Ident)
			if !ok || !ok2 || lead.Name != "lead" || bindings.Name != "bindings" {
				t.Error("a handwritten chord bypasses the registry")
			}
		}
		return true
	})
}

// registryKey is a key an expression spells, when it is one: a string
// literal, or one of the window's reads of the registry (bareKey and its
// neighbors), evaluated.
func registryKey(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(e.Value)
		return value, err == nil
	case *ast.IndexExpr:
		call, ok := e.X.(*ast.CallExpr)
		idx, ok2 := e.Index.(*ast.BasicLit)
		if !ok || !ok2 || len(call.Args) != 1 {
			return "", false
		}
		fn, ok := call.Fun.(*ast.Ident)
		arg, ok3 := call.Args[0].(*ast.BasicLit)
		if !ok || !ok3 || fn.Name != "leaderSeq" {
			return "", false
		}
		id, _ := strconv.Unquote(arg.Value)
		i, _ := strconv.Atoi(idx.Value)
		return leaderSeq(id)[i], true
	case *ast.CallExpr:
		fn, ok := e.Fun.(*ast.Ident)
		if !ok || len(e.Args) != 1 {
			return "", false
		}
		arg, ok := e.Args[0].(*ast.BasicLit)
		if !ok {
			return "", false
		}
		id, _ := strconv.Unquote(arg.Value)
		read := map[string]func(string) string{"bareKey": bareKey, "leaderKey": leaderKey, "chordKey": chordKey, "menuKey": menuKey}[fn.Name]
		if read == nil {
			return "", false
		}
		return read(id), true
	}
	return "", false
}

// The help is generated from the registry: every command the window binds is
// named there by the keys the registry gives it — its list key, and its
// sequence after the leader — so a command added with a key is in the help
// without anyone writing it in, and a key moved in the registry moves there.
func TestTheHelpNamesEveryBoundCommand(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	// The keys the help's rows name: each row's key column, `    F1 / ?   …`,
	// split where it offers more than one.
	named := map[string]bool{}
	for _, line := range strings.Split(m.helpText(), "\n") {
		if !strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "     ") {
			continue
		}
		column, _, _ := strings.Cut(strings.TrimPrefix(line, "    "), "  ")
		for _, alt := range strings.Split(column, " / ") {
			for _, key := range strings.Split(alt, " or ") {
				named[key] = true
			}
		}
	}
	for _, c := range console.Commands() {
		b := c.Bindings.TUI
		if len(b.Keys) > 0 && !named[keyLabel(b.Keys[0])] {
			t.Errorf("no row of the help is %s's key %s", c.ID, keyLabel(b.Keys[0]))
		}
		if len(b.Leader) > 0 && !named[m.leaderKeys(c.ID)] {
			t.Errorf("no row of the help is %s's %s", c.ID, m.leaderKeys(c.ID))
		}
		for _, chord := range b.Chords {
			if !named[keyLabel(chord)] {
				t.Errorf("no row of the help is %s's chord %s", c.ID, keyLabel(chord))
			}
		}
	}
	for _, id := range []string{"pane", "service"} {
		if !named[m.gestureKeys(id)] {
			t.Errorf("no row of the help is the %s gesture's %s", id, m.gestureKeys(id))
		}
	}
}

// The palette spells a command's binding the way the help does.
func TestThePaletteSpellsBindingsLikeTheHelp(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if got, want := m.commandBinding(command("previous box")), m.leaderKeys("previous box"); got != want || !strings.Contains(got, "↑") {
		t.Errorf("previous box is bound as %q, want %q", got, want)
	}
	if got := m.commandBinding(command("harnesses")); got != "F3" {
		t.Errorf("harnesses is bound as %q, want F3", got)
	}
}
