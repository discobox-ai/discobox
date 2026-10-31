package monitors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeMonitor(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const githubLike = `#!/usr/bin/env bash
#---
# name: GitHub repository
# description: New activity
# interval: 60s
# options:
# - {name: repo, required: true, description: owner/name}
# - {name: watch, description: "issues, prs, actions; all three when omitted"}
# - pr
#---
echo hi
`

func TestDiscoverReadsOptionsWrittenAsOneLineMappings(t *testing.T) {
	builtin := t.TempDir()
	writeMonitor(t, builtin, "10-github-repo.sh", githubLike)
	defs, err := Discover(builtin, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("got %d definitions", len(defs))
	}
	def := defs[0]
	if def.ID != "github-repo" || def.Problem != "" || !def.Builtin {
		t.Fatalf("definition = %+v", def)
	}
	if def.Interval != time.Minute {
		t.Fatalf("interval = %v", def.Interval)
	}
	want := []Option{
		{Name: "repo", Required: true, Description: "owner/name"},
		{Name: "watch", Description: "issues, prs, actions; all three when omitted"},
		{Name: "pr"},
	}
	if len(def.Options) != len(want) {
		t.Fatalf("options = %+v", def.Options)
	}
	for i := range want {
		if def.Options[i] != want[i] {
			t.Fatalf("option %d = %+v, want %+v", i, def.Options[i], want[i])
		}
	}
}

func TestARepositoryMonitorWinsOverTheImagesOnASharedID(t *testing.T) {
	builtin, root := t.TempDir(), t.TempDir()
	writeMonitor(t, builtin, "10-github-repo.sh", githubLike)
	path := writeMonitor(t, filepath.Join(root, DirName), "github-repo.sh", "#!/bin/sh\n#---\n# description: ours\n#---\n")
	defs, err := Discover(builtin, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Path != path || defs[0].Builtin || defs[0].Interval != 0 {
		t.Fatalf("definitions = %+v", defs)
	}
}

func TestADeclarationThatCannotRunIsListedWithItsProblem(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"fast.sh":       "#!/bin/sh\n#---\n# interval: 10ms\n#---\n",
		"nested.sh":     "#!/bin/sh\n#---\n# options:\n#   repo: {required: true}\n#---\n",
		"badname.sh":    "#!/bin/sh\n#---\n# options:\n# - {name: Repo}\n#---\n",
		"twice.sh":      "#!/bin/sh\n#---\n# options:\n# - a\n# - a\n#---\n",
		"metadata.yaml": "name: only metadata\n",
	}
	for name, body := range cases {
		writeMonitor(t, dir, name, body)
	}
	if err := os.WriteFile(filepath.Join(dir, "noexec.sh"), []byte("#!/bin/sh\n#---\n#---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != len(cases)+1 {
		t.Fatalf("got %d definitions, want every file listed", len(defs))
	}
	for _, def := range defs {
		if def.Problem == "" {
			t.Errorf("%s has no problem", def.FileName)
		}
	}
}

func TestLoadReadsOneDeclarationByPath(t *testing.T) {
	dir := t.TempDir()
	path := writeMonitor(t, dir, "10-github-repo.sh", githubLike)
	def, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if def.ID != "github-repo" {
		t.Fatalf("id = %q", def.ID)
	}
	if _, err := Load(filepath.Join(dir, "gone.sh"), true); err == nil || !strings.Contains(err.Error(), "no such declaration") {
		t.Fatalf("load of a missing file = %v", err)
	}
}

// The image's monitors are front matter written by hand, where one unquoted
// comma in a description makes the whole monitor unrunnable.
func TestTheImagesMonitorsAllParse(t *testing.T) {
	defs, err := Discover("../features/cli/monitors", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 {
		t.Fatal("no monitors found in the image's directory")
	}
	for _, def := range defs {
		if def.Problem != "" {
			t.Errorf("%s: %s", def.FileName, def.Problem)
		}
	}
}
