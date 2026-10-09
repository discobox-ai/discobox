package handlers

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestHandlersDoNotImportResourcePackages holds the server's layering:
// handlers adapt transport to internal/services and never reach into a
// resource package (server/internal/resources/DESIGN.md, "Boundaries").
func TestHandlersDoNotImportResourcePackages(t *testing.T) {
	const resources = "github.com/discobox-ai/discobox/server/internal/resources"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == resources || strings.HasPrefix(path, resources+"/") {
				t.Errorf("%s imports %s; handlers go through internal/services", fset.Position(imp.Pos()), path)
			}
		}
	}
}
