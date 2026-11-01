package monitors

import (
	"os"
	"path/filepath"
	"runtime"
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
	want := len(cases)
	// Windows has no executable bit to withhold.
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(filepath.Join(dir, "noexec.sh"), []byte("#!/bin/sh\n#---\n#---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		want++
	}
	defs, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != want {
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

func TestLoadReadsNothingBesideTheFile(t *testing.T) {
	// A skill's monitor beside its notes: watch.md normalizes to the same id
	// and sorts first, which would make watch.sh a duplicate in a listing.
	dir := t.TempDir()
	path := writeMonitor(t, dir, "watch.sh", githubLike)
	writeMonitor(t, dir, "watch.md", "# notes\n")
	def, err := Load(path, false)
	if err != nil || def.Problem != "" {
		t.Fatalf("load = %+v, %v", def, err)
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

func TestAnInstanceChoosesItsIntervalOnlyWhenTheMonitorDeclaresTheOption(t *testing.T) {
	dir := t.TempDir()
	writeMonitor(t, dir, "paced.sh", "#!/bin/sh\n#---\n# interval: 60s\n# min-interval: 10s\n# options:\n# - interval\n#---\n")
	writeMonitor(t, dir, "fixed.sh", "#!/bin/sh\n#---\n# interval: 60s\n#---\n")
	defs, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	paced, _ := Find(defs, "paced")
	fixed, _ := Find(defs, "fixed")
	if paced.Problem != "" || paced.MinInterval != 10*time.Second {
		t.Fatalf("paced = %+v", paced)
	}
	for _, tc := range []struct {
		def     Definition
		opts    map[string]string
		want    time.Duration
		problem string
	}{
		{paced, nil, time.Minute, ""},
		{paced, map[string]string{"interval": "15s"}, 15 * time.Second, ""},
		{paced, map[string]string{"interval": "5s"}, 0, "shorter than 10s"},
		{paced, map[string]string{"interval": "soon"}, 0, "not a duration"},
		// Not declared: CheckOptions refuses it as unknown; the runner keeps
		// the declared interval.
		{fixed, map[string]string{"interval": "15s"}, time.Minute, ""},
	} {
		got, err := tc.def.IntervalFor(tc.opts)
		if tc.problem != "" {
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Errorf("%s %v: err = %v, want %q", tc.def.ID, tc.opts, err, tc.problem)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s %v = %v, %v; want %v", tc.def.ID, tc.opts, got, err, tc.want)
		}
	}
	if err := CheckOptions(paced, map[string]string{"interval": "1s"}); err == nil || !strings.Contains(err.Error(), "shorter than 10s") {
		t.Fatalf("CheckOptions = %v", err)
	}
}

func TestAMinIntervalOutsideItsBoundsIsAProblem(t *testing.T) {
	dir := t.TempDir()
	writeMonitor(t, dir, "streaming.sh", "#!/bin/sh\n#---\n# min-interval: 10s\n#---\n")
	writeMonitor(t, dir, "above.sh", "#!/bin/sh\n#---\n# interval: 30s\n# min-interval: 1m\n#---\n")
	defs, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range defs {
		if !strings.Contains(def.Problem, "min-interval") {
			t.Errorf("%s: problem = %q", def.ID, def.Problem)
		}
	}
}

func TestSinceIsDeclaredInFrontMatter(t *testing.T) {
	dir := t.TempDir()
	writeMonitor(t, dir, "yes.sh", "#!/bin/sh\n#---\n# interval: 60s\n# since: true\n#---\n")
	writeMonitor(t, dir, "no.sh", "#!/bin/sh\n#---\n# interval: 60s\n#---\n")
	writeMonitor(t, dir, "bad.sh", "#!/bin/sh\n#---\n# since: sometimes\n#---\n")
	defs, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Definition{}
	for _, def := range defs {
		got[def.ID] = def
	}
	if !got["yes"].Since || got["no"].Since {
		t.Errorf("since = %v, %v", got["yes"].Since, got["no"].Since)
	}
	if !strings.Contains(got["bad"].Problem, "since") {
		t.Errorf("bad since problem = %q", got["bad"].Problem)
	}
}
