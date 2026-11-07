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

// Lifecycle and interaction keys are registered from the descriptors. A new
// action cannot quietly acquire a shortcut without searchable metadata.
func TestConsoleCommandBindingCoverage(t *testing.T) {
	keys := map[string]bool{}
	for _, c := range console.Commands() {
		b := c.Bindings.TUI
		for _, key := range slices.Concat(b.Keys, b.Leader, b.Menu) {
			keys[key] = true
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
				// The registry's own leader sequences (console.Commands).
				leader, ok := arg.X.(*ast.Ident)
				if !ok || leader.Name != "leader" {
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

// The window's own key constants, which its hints, help and menus still name,
// are the registry's bindings: a key changed in the registry and not here
// would move the binding while every hint went on naming the old key.
func TestTheWindowsKeysAreTheRegistrys(t *testing.T) {
	type binding struct{ keys, leader, menu []string }
	for id, want := range map[string]binding{
		"review config": {keys: []string{harnessChangesKey}, leader: []string{harnessChangesKey}},
		"settings":      {keys: []string{settingsKey}, leader: []string{settingsKey}},
		"tools":         {keys: []string{toolsKey}, leader: []string{toolsKey}},
		"issue":         {keys: []string{issueKey}, leader: []string{issueKey}},
		"pr":            {keys: []string{pullKey}, leader: []string{pullKey}},
		"audit":         {leader: []string{auditKey}},
		"credentials":   {keys: []string{secretsKey}, leader: []string{secretsKey}},
		"harnesses":     {keys: []string{harnessesKey}, leader: []string{harnessesKey}},
		"quit":          {keys: []string{paneQuitKey}, leader: []string{paneQuitKey}},
		"all":           {menu: []string{boxMenuKey, allBoxesKey}},
		"services":      {leader: []string{paneServicesKey, paneServicesMenuKey}},
		"requests":      {keys: []string{credentialsKey}, leader: []string{credentialsLeaderKey}},
		"rejections":    {keys: []string{rejectedKey}, leader: []string{rejectedKey}},
		"files":         {keys: []string{pullFilesKey}, leader: []string{pullFilesKey}},
		"options":       {keys: []string{optionsKey}},
		"repaint":       {keys: []string{repaintKey}},
		"terminal":      {leader: []string{paneTerminalKey}},
		"end":           {leader: []string{paneEndKey}},
		"maximize":      {leader: []string{paneZoomKey}},
		"detach":        {leader: []string{paneDetachAlt}},
		"mouse":         {leader: []string{paneMouseKey}},
		"actions":       {keys: []string{boxMenuKey}, leader: []string{boxMenuKey}},
		"dismiss":       {leader: []string{bannerDismissKey}},
		"palette":       {keys: []string{":"}, leader: []string{paletteKey}},
	} {
		c, ok := console.Lookup(id)
		if !ok {
			t.Errorf("no command %s", id)
			continue
		}
		b := c.Bindings.TUI
		if !slices.Equal(b.Keys, want.keys) || !slices.Equal(b.Leader, want.leader) || !slices.Equal(b.Menu, want.menu) {
			t.Errorf("%s: the registry binds %+v, the window's keys are %+v", id, b, want)
		}
	}
}
