package parser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	hooks "github.com/discobox-ai/discobox/hooks"
)

func TestNormalizeID(t *testing.T) {
	tests := map[string]string{
		"go-check.sh":         "go-check",
		"01 Review Go.md":     "review-go",
		"90-review-lint.sh":   "review-lint",
		"001_review_lint.sh":  "review-lint",
		"v2-review-lint.sh":   "v2-review-lint",
		"terraform.plan.bash": "terraform-plan",
		"!!!.sh":              "",
	}
	for in, want := range tests {
		if got := NormalizeID(in); got != want {
			t.Fatalf("NormalizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseShellCommentScriptHook(t *testing.T) {
	root := t.TempDir()
	path := writeHook(t, root, "go-check.sh", `#!/usr/bin/env bash
#---
# name: Go tests
# type: file
# pattern: "**/*.go"
# exclude:
#   - vendor/**
# run-as: root
# phase: review
# extra-field: kept
#---
go test ./...
`, 0o755)

	hook, err := ParseFile(root, path)
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	if hook.ID != "go-check" || hook.Name != "Go tests" {
		t.Fatalf("unexpected id/name: %#v", hook)
	}
	if hook.Type != hooks.HookTypeFile || hook.Engine != hooks.HookEngineScript {
		t.Fatalf("unexpected type/engine: %#v", hook)
	}
	if hook.Pattern != "**/*.go" {
		t.Fatalf("unexpected pattern %q", hook.Pattern)
	}
	if len(hook.Ignore) != 1 || hook.Ignore[0] != "vendor/**" {
		t.Fatalf("unexpected ignore %#v", hook.Ignore)
	}
	if hook.RunAs != hooks.RunAsRoot || hook.Phase != "review" {
		t.Fatalf("aliases/fields not normalized: %#v", hook)
	}
	if !hook.HasShebang {
		t.Fatalf("script validation flags not set: %#v", hook)
	}
	// Executable mirrors the mode bit, which Windows does not carry — which is
	// why Validate only requires it off Windows. Asserting it here would be
	// asserting the filesystem, not the parser.
	if runtime.GOOS != "windows" && !hook.Executable {
		t.Fatalf("script validation flags not set: %#v", hook)
	}
	if hook.Extensions["extra_field"] != "kept" {
		t.Fatalf("unknown field not preserved: %#v", hook.Extensions)
	}
	if hook.RelPath != ".discobox/hooks/go-check.sh" || hook.AbsPath != path {
		t.Fatalf("unexpected paths rel=%q abs=%q", hook.RelPath, hook.AbsPath)
	}
}

func TestParseSlashCommentScriptHook(t *testing.T) {
	root := t.TempDir()
	path := writeHook(t, root, "lint.js", `#!/usr/bin/env node
//---
// type: session
// blocking: true
//---
console.log("lint")
`, 0o755)

	hook, err := ParseFile(root, path)
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	if hook.Type != hooks.HookTypeSession || !hook.Blocking {
		t.Fatalf("unexpected hook: %#v", hook)
	}
	if hook.Name != "Lint" {
		t.Fatalf("default name = %q, want Lint", hook.Name)
	}
}

func TestParseAIPromptCompatibility(t *testing.T) {
	root := t.TempDir()
	path := writeHook(t, root, "01 Review Go.md", `---
name: Go review
type: file
engine: ai
pattern: "**/*.go"
subagent: reviewer
---
Review changed Go files.
`, 0o644)

	hook, err := ParseFile(root, path)
	if err != nil {
		t.Fatalf("ParseFile returned error: %v", err)
	}
	if hook.ID != "review-go" || hook.Engine != hooks.HookEngineAI {
		t.Fatalf("unexpected hook: %#v", hook)
	}
	if hook.Prompt != "Review changed Go files." {
		t.Fatalf("prompt = %q", hook.Prompt)
	}
	if hook.Subagent != "reviewer" {
		t.Fatalf("subagent = %q", hook.Subagent)
	}
}

func TestDiscoverRulesSortingAndGlobalIgnore(t *testing.T) {
	root := t.TempDir()
	writeHook(t, root, "b.sh", `#!/bin/sh
#---
# name: B hook
# type: session
#---
:
`, 0o755)
	writeHook(t, root, "a.sh", `#!/bin/sh
#---
# name: A hook
# type: session
#---
:
`, 0o755)
	writeHook(t, root, ".hidden.sh", `#!/bin/sh
#---
# type: session
#---
:
`, 0o755)
	if err := os.Mkdir(filepath.Join(root, HooksDirName, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, HooksDirName, GlobalIgnoreName), []byte("# comment\n\nnode_modules/**\ndist/**\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	disc, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if got := len(disc.Hooks); got != 2 {
		t.Fatalf("hook count = %d, want 2", got)
	}
	if disc.Hooks[0].ID != "a" || disc.Hooks[1].ID != "b" {
		t.Fatalf("hooks not sorted by name/id: %#v", disc.Hooks)
	}
	if strings.Join(disc.GlobalIgnore, ",") != "node_modules/**,dist/**" {
		t.Fatalf("unexpected global ignore: %#v", disc.GlobalIgnore)
	}
}

func TestDiscoverAbsentHooksDirReturnsEmpty(t *testing.T) {
	disc, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(disc.Hooks) != 0 {
		t.Fatalf("hooks = %#v, want empty", disc.Hooks)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		content string
		mode    os.FileMode
		field   string
	}{
		{
			name: "missing front matter",
			file: "missing.sh",
			content: `#!/bin/sh
:
`,
			mode:  0o755,
			field: "front_matter",
		},
		{
			name: "missing type",
			file: "missing-type.sh",
			content: `#!/bin/sh
#---
# name: Missing Type
#---
:
`,
			mode:  0o755,
			field: "type",
		},
		{
			name: "unsupported type",
			file: "bad-type.sh",
			content: `#!/bin/sh
#---
# type: save
#---
:
`,
			mode:  0o755,
			field: "type",
		},
		{
			name: "unsupported engine",
			file: "bad-engine.sh",
			content: `#!/bin/sh
#---
# type: session
# engine: native
#---
:
`,
			mode:  0o755,
			field: "engine",
		},
		{
			name: "builtin engine without registration",
			file: "builtin.sh",
			content: `#!/bin/sh
#---
# type: session
# engine: builtin
#---
:
`,
			mode:  0o755,
			field: "engine",
		},
		{
			name: "file missing pattern",
			file: "no-pattern.sh",
			content: `#!/bin/sh
#---
# type: file
#---
:
`,
			mode:  0o755,
			field: "pattern",
		},
		{
			name: "reserved phase",
			file: "reserved-phase.sh",
			content: `#!/bin/sh
#---
# type: file
# pattern: "**/*.go"
# phase: all
#---
:
`,
			mode:  0o755,
			field: "phase",
		},
		{
			name: "bad phase",
			file: "bad-phase.sh",
			content: `#!/bin/sh
#---
# type: file
# pattern: "**/*.go"
# phase: bad/phase
#---
:
`,
			mode:  0o755,
			field: "phase",
		},
		{
			name: "script missing shebang",
			file: "no-shebang.sh",
			content: `#---
# type: session
#---
:
`,
			mode:  0o755,
			field: "shebang",
		},
	}
	if runtime.GOOS != "windows" {
		tests = append(tests, struct {
			name    string
			file    string
			content string
			mode    os.FileMode
			field   string
		}{
			name: "script not executable",
			file: "not-executable.sh",
			content: `#!/bin/sh
#---
# type: session
#---
:
`,
			mode:  0o644,
			field: "mode",
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeHook(t, root, tt.file, tt.content, tt.mode)
			_, err := ParseFile(root, path)
			if err == nil {
				t.Fatal("expected validation error")
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error %T = %v, want ValidationError", err, err)
			}
			if verr.Field != tt.field {
				t.Fatalf("field = %q, want %q (err %v)", verr.Field, tt.field, err)
			}
			if !strings.Contains(verr.Path, tt.file) {
				t.Fatalf("path = %q, want it to include %q", verr.Path, tt.file)
			}
		})
	}
}

func writeHook(t *testing.T, root, name, content string, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(root, HooksDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}
