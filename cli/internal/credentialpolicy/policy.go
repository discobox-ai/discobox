// Package credentialpolicy resolves local credential policy files into the
// concrete route snapshot a binding holds (ADR 26-10-30-584).
package credentialpolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Reference is one file and the arguments scoped to that file.
type Reference struct {
	Path string
	Args map[string]string
}

// References accumulates policies and arguments in command-line order.
type References []Reference

// Add starts a policy; subsequent arguments apply only to it.
func (r *References) Add(path string) error {
	if path == "" {
		return fmt.Errorf("policy: a file path is required")
	}
	*r = append(*r, Reference{Path: path, Args: map[string]string{}})
	return nil
}

// Argument assigns NAME=VALUE to the preceding policy, refusing duplicates.
func (r *References) Argument(value string) error {
	if len(*r) == 0 {
		return fmt.Errorf("policy argument %q needs a preceding policy", value)
	}
	name, value, ok := strings.Cut(value, "=")
	if !ok || !parameterName.MatchString(name) || name == "policy" {
		return fmt.Errorf("policy argument: want NAME=VALUE with a valid parameter name")
	}
	args := (*r)[len(*r)-1].Args
	if _, ok := args[name]; ok {
		return fmt.Errorf("policy argument %q is repeated", name)
	}
	args[name] = value
	return nil
}

// Resolve reads each policy independently, then unions its routes into one list.
func (r References) Resolve() ([]secretroute.Route, error) {
	var out []secretroute.Route
	for _, ref := range r {
		routes, err := read(ref)
		if err != nil {
			return nil, fmt.Errorf("policy %q: %w", ref.Path, err)
		}
		out = append(out, routes...)
	}
	return out, nil
}

type document struct {
	Version     int                  `json:"version"`
	Description string               `json:"description,omitempty"`
	Parameters  map[string]parameter `json:"parameters,omitempty"`
	Routes      []secretroute.Route  `json:"routes"`
}
type parameter struct {
	Type     string  `json:"type"`
	Optional bool    `json:"optional,omitempty"`
	Default  *string `json:"default,omitempty"`
}

const policyByteLimit = 1 << 20

var parameterName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var segment = regexp.MustCompile(`^[A-Za-z0-9._~-]*$`)

func read(ref Reference) ([]secretroute.Route, error) {
	f, err := os.Open(ref.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, policyByteLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > policyByteLimit {
		return nil, fmt.Errorf("file exceeds 1 MiB")
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("file is not valid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSON(d, reflect.TypeFor[document](), "$", 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("version must be 1")
	}
	if len(doc.Routes) == 0 {
		return nil, fmt.Errorf("routes must be nonempty")
	}
	values := map[string]string{}
	for name, p := range doc.Parameters {
		if !parameterName.MatchString(name) || name == "policy" {
			return nil, fmt.Errorf("parameter %q has an invalid or reserved name", name)
		}
		if p.Type != "pathSegment" {
			return nil, fmt.Errorf("parameter %q: type must be pathSegment", name)
		}
		if p.Optional != (p.Default != nil) {
			return nil, fmt.Errorf("parameter %q: optional parameters require a default; required parameters cannot have one", name)
		}
		if p.Default != nil {
			if err := literal(*p.Default); err != nil {
				return nil, fmt.Errorf("parameter %q default: %w", name, err)
			}
		}
		v, ok := ref.Args[name]
		if !ok {
			if !p.Optional {
				return nil, fmt.Errorf("required parameter %q is missing", name)
			}
			v = *p.Default
		}
		if err := literal(v); err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		values[name] = v
	}
	for name := range ref.Args {
		if _, ok := doc.Parameters[name]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}
	}
	remainingPathBytes := policyByteLimit
	for i, route := range doc.Routes {
		// Only paths allow placeholders. Check all other fields as encoded JSON.
		paths := route.Paths
		route.Paths = nil
		other, err := json.Marshal(route)
		if err != nil {
			return nil, fmt.Errorf("routes[%d]: %w", i, err)
		}
		if strings.Contains(string(other), "${") {
			return nil, fmt.Errorf("routes[%d]: parameters are supported only in paths", i)
		}
		for j, path := range paths {
			originalSegments := strings.Split(path, "/")
			var b strings.Builder
			for {
				before, after, found := strings.Cut(path, "${")
				if len(before) > remainingPathBytes {
					return nil, fmt.Errorf("routes[%d].paths[%d]: expanded paths exceed 1 MiB", i, j)
				}
				remainingPathBytes -= len(before)
				b.WriteString(before)
				if !found {
					break
				}
				name, tail, closed := strings.Cut(after, "}")
				if !closed {
					return nil, fmt.Errorf("routes[%d].paths[%d]: unclosed parameter", i, j)
				}
				v, ok := values[name]
				if !ok {
					return nil, fmt.Errorf("routes[%d].paths[%d]: undeclared parameter %q", i, j, name)
				}
				if len(v) > remainingPathBytes {
					return nil, fmt.Errorf("routes[%d].paths[%d]: expanded paths exceed 1 MiB", i, j)
				}
				remainingPathBytes -= len(v)
				b.WriteString(v)
				path = tail
			}
			paths[j] = b.String()
			for k, part := range strings.Split(paths[j], "/") {
				if part == "" && originalSegments[k] != "" {
					return nil, fmt.Errorf("routes[%d].paths[%d]: an empty argument cannot remove a path segment", i, j)
				}
			}
		}
		route.Paths = paths
		normalized, err := secretroute.Normalize(route)
		if err != nil {
			return nil, fmt.Errorf("routes[%d]: %w", i, err)
		}
		doc.Routes[i] = normalized
	}
	return doc.Routes, nil
}

func literal(value string) error {
	if !segment.MatchString(value) || value == "." || value == ".." {
		return fmt.Errorf("want a literal path segment containing only letters, digits, ., _, ~ or -")
	}
	return nil
}

// checkJSON checks exact field names, duplicate keys, types, nulls, and depth
// before decoding. encoding/json alone accepts duplicate and case-folded keys.
func checkJSON(d *json.Decoder, t reflect.Type, path string, depth int) error {
	if depth > 64 {
		return fmt.Errorf("%s: JSON nesting exceeds 64", path)
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if tok == nil {
		return fmt.Errorf("%s: null is not permitted", path)
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		if tok != json.Delim('{') {
			return fmt.Errorf("%s: want an object", path)
		}
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok {
				return fmt.Errorf("%s: want an object key", path)
			}
			if seen[key] {
				return fmt.Errorf("%s: duplicate key %q", path, key)
			}
			seen[key] = true
			var child reflect.Type
			if t.Kind() == reflect.Map {
				child = t.Elem()
			} else {
				for i := 0; i < t.NumField(); i++ {
					f := t.Field(i)
					if strings.Split(f.Tag.Get("json"), ",")[0] == key {
						child = f.Type
						break
					}
				}
			}
			if child == nil {
				return fmt.Errorf("%s: unknown field %q", path, key)
			}
			if err := checkJSON(d, child, fmt.Sprintf("%s[%q]", path, key), depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case reflect.Slice:
		if tok != json.Delim('[') {
			return fmt.Errorf("%s: want an array", path)
		}
		for i := 0; d.More(); i++ {
			if err := checkJSON(d, t.Elem(), fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case reflect.String:
		if _, ok := tok.(string); !ok {
			return fmt.Errorf("%s: want a string", path)
		}
	case reflect.Bool:
		if _, ok := tok.(bool); !ok {
			return fmt.Errorf("%s: want a boolean", path)
		}
	case reflect.Int:
		if n, ok := tok.(float64); !ok || n != float64(int(n)) {
			return fmt.Errorf("%s: want an integer", path)
		}
	default:
		return fmt.Errorf("%s: unsupported value", path)
	}
	return nil
}
