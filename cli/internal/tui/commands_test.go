package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Lifecycle and interaction keys are registered from the descriptors. A new
// action cannot quietly acquire a shortcut without searchable metadata.
func TestConsoleCommandBindingCoverage(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	commands := m.consoleCommands(nil)
	bareKeys := map[string]bool{}
	ids, keys, aliases := map[string]bool{}, map[string]bool{}, map[string]string{}
	for _, c := range commands {
		if c.id == "" || c.label == "" || c.detail == "" || c.run == nil || len(c.aliases) == 0 {
			t.Fatalf("incomplete command: %+v", c)
		}
		if ids[c.id] {
			t.Fatalf("duplicate ID %s", c.id)
		}
		ids[c.id] = true
		if c.key != "" {
			if bareKeys[c.key] {
				t.Fatalf("duplicate binding %s", c.key)
			}
			bareKeys[c.key] = true
			keys[c.key] = true
		}
		for _, key := range c.keyAliases {
			keys[key] = true
		}
		for _, key := range c.menuKeys {
			keys[key] = true
		}
		for _, key := range c.prefixKeys {
			if key != "" {
				keys[key] = true
			}
		}
		for _, alias := range c.aliases {
			alias = strings.ToLower(alias)
			if old, ok := aliases[alias]; ok && old != c.id {
				t.Fatalf("ambiguous alias %s for %s and %s", alias, old, c.id)
			}
			aliases[alias] = c.id
		}
	}
	for key := range verbs {
		if !keys[key] {
			t.Errorf("lifecycle binding %s has no command", key)
		}
	}
	for key := range interactions {
		if !keys[key] {
			t.Errorf("interaction binding %s has no command", key)
		}
	}
	for _, page := range treePages {
		if !keys[page.key] {
			t.Errorf("tree binding %s has no command", page.key)
		}
	}

	// Only cursor motion, selection and terminal forwarding are gestures. Real
	// verbs must be described even when a handwritten window dispatch owns them.
	gestures := map[string]bool{}
	for _, key := range []string{"down", "up", "left", "right", "j", "k", "h", "l", "pgdown", "pgup", "home", "end", "g", "G", "esc", "tab", "shift+tab", "V", " ", "c", "enter", "ctrl+c", boxUpKey, boxDownKey} {
		gestures[key] = true
	}
	constants := map[string]string{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
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
			if literal, ok := spec.Values[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				value, _ := strconv.Unquote(literal.Value)
				constants[spec.Names[0].Name] = value
			}
			return true
		})
	}
	resolve := func(expr ast.Expr) (string, bool) {
		if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			value, err := strconv.Unquote(lit.Value)
			return value, err == nil
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
		if !keys[key] && !gestures[key] {
			t.Errorf("window binding %q in %s has no command descriptor or gesture classification", key, where)
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
	for _, path := range []string{"model.go", "tree.go"} {
		for _, decl := range parsed[path].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "updateKey" && fn.Name.Name != "updateList" && fn.Name.Name != "treeKey" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sw, ok := n.(*ast.SwitchStmt); ok && sw.Tag != nil && isKey(sw.Tag) {
					for _, stmt := range sw.Body.List {
						clause := stmt.(*ast.CaseClause)
						for _, expr := range clause.List {
							check(expr, fn.Name.Name)
						}
					}
				}
				if bin, ok := n.(*ast.BinaryExpr); ok && bin.Op == token.EQL {
					if isKey(bin.X) {
						if _, ok := resolve(bin.Y); ok {
							check(bin.Y, fn.Name.Name)
						}
					}
				}
				return true
			})
		}
	}

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
				field, ok := arg.X.(*ast.SelectorExpr)
				if !ok || field.Sel.Name != "prefixKeys" {
					t.Error("unclassified computed prefix binding")
				}
			case *ast.CallExpr:
				selector, ok := arg.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Itoa" {
					t.Error("unclassified computed prefix binding")
				}
			default:
				check(arg, sel.Sel.Name)
			}
		case "WithPrefixChord":
			// One generated chord map joins descriptor sequences with numeric service
			// navigation. A new handwritten chord must join that registry instead.
			lead, ok := call.Args[0].(*ast.Ident)
			if !ok || lead.Name != "lead" {
				t.Error("handwritten chord bypasses command registry")
			}
			bindings, ok := call.Args[1].(*ast.Ident)
			if !ok || bindings.Name != "bindings" {
				t.Error("handwritten chord bindings bypass command registry")
			}
		}
		return true
	})

}

func TestConsoleCommandEligibility(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	tests := []struct {
		state   State
		command string
		enabled bool
	}{
		{StateRunning, "stop", true}, {StateSuspended, "stop", true}, {StateStopped, "stop", false},
		{StateStopped, "start", true}, {StateSuspended, "start", true}, {StateArchived, "start", false},
		{StateArchived, "rename", false}, {StateArchived, "settings", false}, {StateArchived, "purge", true},
		{StateRunning, "purge", false}, {StateError, "repair", true},
	}
	for _, tt := range tests {
		box := Sandbox{ID: "box", State: tt.state}
		found := false
		for _, c := range m.consoleCommands(&box) {
			if c.id == tt.command {
				found = true
				if c.enabled != tt.enabled {
					t.Errorf("%s on %s = %v (%s)", tt.command, tt.state, c.enabled, c.why)
				}
			}
		}
		if !found {
			t.Errorf("missing command %s", tt.command)
		}
	}
}
