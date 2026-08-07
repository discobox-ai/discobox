package model_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	apigen "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/server/internal/model"
)

// TestModelEnumsMatchAPISchema pins every string-typed domain enum to the
// OpenAPI enum the API layer serves it through. The server hands these values
// out verbatim (services.Convert JSON-marshals the domain model into the
// generated API type), but json.Unmarshal into an ogen enum does NOT validate,
// so a value the OpenAPI schema omits is only rejected when the CLIENT decodes
// the response — far from the model change that introduced it. That is exactly
// how "deleting" reached the worker phase enum: added to the model, missing from
// server.yaml, invisible until `discobox pool ls` failed to decode.
//
// This test makes the two lists fail CI the moment they diverge, in either
// direction. When it fails: a value in the model but not the schema means the
// OpenAPI enum in api/openapi/server.yaml needs the value added (then regenerate);
// a value in the schema but not the model means the model registry is stale.
func TestModelEnumsMatchAPISchema(t *testing.T) {
	cases := []struct {
		name  string
		model []string
		api   []string
	}{
		{"pool state", model.PoolStates, values(apigen.PoolState("").AllValues())},
		{"pool desired state", model.PoolDesiredStates, values(apigen.PoolDesiredState("").AllValues())},
		{"sandbox state", model.SandboxStates, values(apigen.SandboxRuntimeState("").AllValues())},
		{"sandbox desired state", model.SandboxDesiredStates, values(apigen.SandboxRuntimeDesiredState("").AllValues())},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			modelOnly := difference(tc.model, tc.api)
			apiOnly := difference(tc.api, tc.model)
			if len(modelOnly) > 0 {
				t.Errorf("values in the model but missing from the OpenAPI enum: %v\n"+
					"add them to the %s enum in api/openapi/server.yaml and regenerate", modelOnly, tc.name)
			}
			if len(apiOnly) > 0 {
				t.Errorf("values in the OpenAPI enum but missing from the model registry: %v\n"+
					"the model enum registry is stale", apiOnly)
			}
		})
	}
}

// TestModelEnumConstsAreRegistered closes the gap the schema cross-check leaves:
// TestModelEnumsMatchAPISchema compares the registry SLICES to the API, so a
// const added to the model but forgotten from its slice — where slice and schema
// still agree by shared omission — slips past it. This scans the model package
// source for every enum const by name prefix and asserts its value is in the
// matching registry slice, so adding a const without registering it fails here.
//
// It relies only on the naming convention (PoolPhaseFoo = "foo"), which the
// whole enum family already follows; a const named off-pattern would not be
// caught, but that is a far more visible mistake than a missing slice entry.
func TestModelEnumConstsAreRegistered(t *testing.T) {
	// Prefix → the registry slices a const with that prefix may live in. No
	// prefix here is a prefix of another, so each const matches at most one
	// entry. DesiredState maps to two because the vocabulary is per-resource
	// since ADR 0022 §1: a value belonging to either resource is registered.
	registries := map[string][][]string{
		"PoolState":    {model.PoolStates},
		"SandboxState": {model.SandboxStates},
		"DesiredState": {model.SandboxDesiredStates, model.PoolDesiredStates},
	}

	for name, value := range stringConstsInPackage(t) {
		prefix := matchingPrefix(name, registries)
		if prefix == "" {
			continue
		}
		if !anySliceContains(registries[prefix], value) {
			t.Errorf("const %s = %q is not in any %s registry slice; add it there (a value the "+
				"registry omits is served but never validated)", name, value, prefix)
		}
	}
}

// stringConstsInPackage parses the model package sources (excluding tests) and
// returns every string-literal const as name → value. Consts defined as
// references to other consts (the Sandbox* operation-status aliases) are not
// string literals and are skipped; their canonical definitions are scanned.
func stringConstsInPackage(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read model package dir: %v", err)
	}
	fset := token.NewFileSet()
	consts := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vspec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range vspec.Names {
					if i >= len(vspec.Values) {
						continue
					}
					lit, ok := vspec.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote const %s value %s: %v", ident.Name, lit.Value, err)
					}
					consts[ident.Name] = value
				}
			}
		}
	}
	return consts
}

// matchingPrefix returns the registry prefix that name starts with, or "".
func matchingPrefix(name string, registries map[string][][]string) string {
	for prefix := range registries {
		if strings.HasPrefix(name, prefix) {
			return prefix
		}
	}
	return ""
}

func anySliceContains(slices [][]string, target string) bool {
	for _, values := range slices {
		for _, v := range values {
			if v == target {
				return true
			}
		}
	}
	return false
}

// values projects a generated enum's typed AllValues() slice to plain strings.
func values[E ~string](in []E) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

// difference returns the members of a that are not in b.
func difference(a, b []string) []string {
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}
	var missing []string
	for _, v := range a {
		if _, ok := set[v]; !ok {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	return missing
}
