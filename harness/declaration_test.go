package harness

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// minimal is a valid coding harness's declaration, as JSON, for each case
// below to break one way.
func minimal() map[string]any {
	return map[string]any{
		"schema": 1, "id": "toy", "bundle": "discobox-harness-toy", "name": "Toy",
		"platformAPIVersion": "discobox.ai/platform/v1",
		"install":            map[string]any{"requires": []any{"core", "node"}, "package": "toy-cli", "executable": "toy"},
		"launch":             map[string]any{"resume": []any{"--continue"}, "prompt": map[string]any{"form": "positional"}},
		"environment":        map[string]any{},
		"settings":           map[string]any{},
		"localState": map[string]any{
			"trust":  map[string]any{"strategy": "none", "none": "it asks none"},
			"memory": map[string]any{"strategy": "none", "none": "it keeps none"},
		},
		"credentials": map[string]any{"store": map[string]any{"strategy": "file", "document": ".toy/auth.json"}},
		"hooks":       map[string]any{"provider": "toy", "transport": "command", "format": "claude-code", "path": "/etc/toy/hooks.json", "timeout": 3, "events": []any{"Stop"}},
		"capture": map[string]any{
			"version": 1, "watch": []any{".toy/auth.json"},
			"providers":       []any{map[string]any{"id": "toy", "envName": "TOY_TOKEN", "types": []any{"token"}, "routes": []any{map[string]any{"hosts": []any{"api.toy.example"}}}}},
			"settings":        []any{},
			"credentialFiles": []any{".toy/auth.json"},
		},
	}
}

func load(t *testing.T, decl map[string]any, files fstest.MapFS) error {
	t.Helper()
	raw, err := json.Marshal(decl)
	if err != nil {
		t.Fatal(err)
	}
	if files == nil {
		files = fstest.MapFS{}
	}
	files["toy/"+DeclarationFile] = &fstest.MapFile{Data: raw}
	_, err = LoadDeclaration(files, "toy")
	return err
}

func TestAValidDeclarationLoads(t *testing.T) {
	if err := load(t, minimal(), nil); err != nil {
		t.Fatal(err)
	}
}

// A declaration is checked strictly and as a whole: an unknown key, a schema
// this build does not read, a part neither configured nor declared absent, an
// asset nothing references, and two parts that disagree all fail the build
// rather than produce a bundle (ADR 26-11-06-596 §2).
func TestADeclarationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(map[string]any)
		files fstest.MapFS
		want  string
	}{
		{"an unknown key", func(d map[string]any) { d["configure"] = true }, nil, "unknown field"},
		{"an unsupported schema", func(d map[string]any) { d["schema"] = 2 }, nil, "schema 2"},
		{"a bundle not named for its id", func(d map[string]any) { d["bundle"] = "toy" }, nil, "bundle"},
		{"no install, but a launch", func(d map[string]any) { d["install"] = map[string]any{"none": "nothing"} }, nil, "launch needs an install"},
		{"an install without node", func(d map[string]any) {
			d["install"].(map[string]any)["requires"] = []any{"core"}
		}, nil, "requires node"},
		{"a launch argument the launcher would split", func(d map[string]any) {
			d["launch"].(map[string]any)["args"] = []any{"--a b"}
		}, nil, "pass literally"},
		{"an unknown prompt form", func(d map[string]any) {
			d["launch"].(map[string]any)["prompt"] = map[string]any{"form": "stdin"}
		}, nil, "prompt form"},
		{"trust none with no reason", func(d map[string]any) {
			d["localState"].(map[string]any)["trust"] = map[string]any{"strategy": "none"}
		}, nil, "needs a reason"},
		{"system-layer trust with no baseline", func(d map[string]any) {
			d["localState"].(map[string]any)["trust"] = map[string]any{"strategy": "system-layer", "layer": "/etc/toy/config.toml", "lock": "/run/lock/toy.lock"}
		}, nil, "no policy asset"},
		{"a missing asset", func(d map[string]any) {
			d["settings"] = map[string]any{"policy": []any{map[string]any{"asset": "assets/policy.json", "path": "/etc/toy/policy.json", "layer": "managed"}}}
		}, nil, "assets/policy.json"},
		{"an asset nothing references", func(map[string]any) {},
			fstest.MapFS{"toy/assets/stray.js": &fstest.MapFile{Data: []byte("//")}}, "referenced by nothing"},
		{"a credential document capture does not read", func(d map[string]any) {
			d["credentials"].(map[string]any)["store"] = map[string]any{"strategy": "file", "document": ".toy/other.json"}
		}, nil, "credentialFiles"},
		{"a secret and its provider spending it on different routes", func(d map[string]any) {
			d["credentials"].(map[string]any)["secrets"] = []any{map[string]any{"name": "TOY_TOKEN", "routes": []any{map[string]any{"hosts": []any{"elsewhere.example"}}}}}
		}, nil, "different routes"},
		{"a misspelled secret key", func(d map[string]any) {
			d["credentials"].(map[string]any)["secrets"] = []any{map[string]any{"name": "TOY_TOKEN", "rejectionScopes": []any{}}}
		}, nil, "unknown field"},
		{"a misspelled seed file key", func(d map[string]any) {
			d["settings"] = map[string]any{"files": []any{map[string]any{"path": ".toy/settings.json", "contents": "{}"}}}
		}, nil, "unknown field"},
		{"command hooks from an asset", func(d map[string]any) {
			d["hooks"].(map[string]any)["asset"] = "assets/hooks.json"
		}, nil, "generated"},
		{"a shell that installs an agent", func(d map[string]any) { d["id"] = ShellSlug; d["bundle"] = "discobox-harness-shell" }, nil, "must be none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := minimal()
			tc.edit(d)
			err := load(t, d, tc.files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one saying %q", err, tc.want)
			}
		})
	}
}

// Every built-in loads, and in the inventory's order.
func TestBuiltinsLoad(t *testing.T) {
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range builtins {
		if d.Directory != builtinDirectories[i] {
			t.Fatalf("builtin %d is %s, want %s", i, d.Directory, builtinDirectories[i])
		}
	}
}
