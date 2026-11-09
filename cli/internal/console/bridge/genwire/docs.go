package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// sources reads what reflection cannot see from a package's Go source: the
// doc comments of its types and their fields, and the constants of each
// named string type, which are the values of an enum on the wire.
type sources struct {
	packages map[string]*packageDocs
}

type packageDocs struct {
	types  map[string]string
	fields map[string]string // "Type.Field"
	consts map[string][]enumValue
	// unread names a constant of each type whose value is not a string
	// literal, which an enum cannot be sure of.
	unread map[string]string
}

type enumValue struct {
	Value string
	Doc   string
}

// wireBuild is the build the wire is read for: the same on every host.
var wireBuild = func() build.Context {
	c := build.Default
	c.GOOS, c.GOARCH = "linux", "amd64"
	return c
}()

func newSources() *sources { return &sources{packages: map[string]*packageDocs{}} }

func (s *sources) pkg(path string) (*packageDocs, error) {
	if p, ok := s.packages[path]; ok {
		return p, nil
	}
	out, err := exec.CommandContext(context.Background(), "go", "list", "-f", "{{.Dir}}", path).Output()
	if err != nil {
		return nil, fmt.Errorf("finding %s: %w", path, err)
	}
	p, err := parsePackage(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	s.packages[path] = p
	return p, nil
}

func parsePackage(dir string) (*packageDocs, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	p := &packageDocs{types: map[string]string{}, fields: map[string]string{}, consts: map[string][]enumValue{}, unread: map[string]string{}}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Only what a build compiles, on one platform whichever this is, so
		// the generated files are the same everywhere they are generated.
		if match, err := wireBuild.MatchFile(dir, name); err != nil || !match {
			if err != nil {
				return nil, err
			}
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gen.Tok {
			case token.TYPE:
				p.readTypes(gen)
			case token.CONST:
				if err := p.readConsts(gen); err != nil {
					return nil, err
				}
			}
		}
	}
	return p, nil
}

func (p *packageDocs) readTypes(gen *ast.GenDecl) {
	for _, spec := range gen.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		doc := ts.Doc
		if doc == nil && len(gen.Specs) == 1 {
			doc = gen.Doc
		}
		p.types[ts.Name.Name] = docText(doc)
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		for _, field := range st.Fields.List {
			doc := field.Doc
			if doc == nil {
				doc = field.Comment
			}
			for _, name := range field.Names {
				p.fields[ts.Name.Name+"."+name.Name] = docText(doc)
			}
		}
	}
}

// readConsts records each constant of a named type with a string value,
// `TypeHello Type = "hello"` or `TypeHello = Type("hello")`. A constant of a
// named type with any other value is noted, so an enum is never generated
// missing a value it could not read.
func (p *packageDocs) readConsts(gen *ast.GenDecl) error {
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		doc := vs.Doc
		if doc == nil && len(gen.Specs) == 1 {
			doc = gen.Doc
		}
		for i, name := range vs.Names {
			var expr ast.Expr
			if i < len(vs.Values) {
				expr = vs.Values[i]
			}
			typ, lit := constant(vs.Type, expr)
			if typ == "" {
				continue
			}
			if lit == nil || lit.Kind != token.STRING {
				p.unread[typ] = name.Name
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return err
			}
			p.consts[typ] = append(p.consts[typ], enumValue{Value: value, Doc: docText(doc)})
		}
	}
	return nil
}

// constant is the named type a constant is declared with, and its value when
// that is a literal.
func constant(typ, value ast.Expr) (string, *ast.BasicLit) {
	lit, _ := value.(*ast.BasicLit)
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name, lit
	}
	if call, ok := value.(*ast.CallExpr); ok && typ == nil && len(call.Args) == 1 {
		if id, ok := call.Fun.(*ast.Ident); ok {
			lit, _ := call.Args[0].(*ast.BasicLit)
			return id.Name, lit
		}
	}
	return "", nil
}

// docText is a doc comment as paragraphs: each one's lines joined, the
// paragraphs separated by a blank line.
func docText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	var paragraphs []string
	for _, para := range strings.Split(strings.TrimSpace(g.Text()), "\n\n") {
		var lines []string
		for _, line := range strings.Split(para, "\n") {
			lines = append(lines, strings.TrimSpace(line))
		}
		paragraphs = append(paragraphs, strings.Join(lines, " "))
	}
	return strings.Join(paragraphs, "\n\n")
}
