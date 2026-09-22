package tools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

func write(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func discover(t *testing.T, dir string, layer Layer) map[string]Definition {
	t.Helper()
	defs, err := Discover(dir, layer)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Definition{}
	for _, def := range defs {
		out[def.ID] = def
	}
	return out
}

func TestAYAMLToolRunsItsProgram(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "20-fresh.yaml", `name: fresh
description: the fresh editor
key: f
program: fresh
args: ["."]
files:
  config.jsonc: .config/fresh/config.json
  trust.json: /.local/share/fresh/workspaces/{workspace}/trust.json
`, 0o644)
	write(t, dir, "fresh/config.jsonc", "// defaults\n{}\n", 0o644)

	fresh := discover(t, dir, LayerImage)["fresh"]
	if fresh.Problem != "" {
		t.Fatalf("problem: %s", fresh.Problem)
	}
	if fresh.Runs != RunsSandbox || fresh.Script || fresh.Key != "f" || fresh.Label() != "fresh" {
		t.Errorf("fresh = %+v", fresh)
	}
	if strings.Join(fresh.Program, " ") != "fresh" || strings.Join(fresh.Args, " ") != "." {
		t.Errorf("program %v args %v", fresh.Program, fresh.Args)
	}
	want := []File{
		{Name: "config.jsonc", Home: ".config/fresh/config.json", Default: "// defaults\n{}\n"},
		{Name: "trust.json", Home: ".local/share/fresh/workspaces/{workspace}/trust.json"},
	}
	if len(fresh.Files) != 2 || fresh.Files[0] != want[0] || fresh.Files[1] != want[1] {
		t.Errorf("files = %+v, want %+v", fresh.Files, want)
	}
}

func TestAScriptToolIsWhatRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the executable bit is POSIX-only")
	}
	dir := t.TempDir()
	write(t, dir, "lint.sh", "#!/bin/sh\n#---\n# key: l\n#---\nexec lint\n", 0o755)
	write(t, dir, "not-executable.sh", "#!/bin/sh\n#---\n#---\n", 0o644)
	write(t, dir, "with-program.sh", "#!/bin/sh\n#---\n# program: lint\n#---\n", 0o755)

	defs := discover(t, dir, LayerSource)
	if lint := defs["lint"]; lint.Problem != "" || !lint.Script || lint.Key != "l" {
		t.Errorf("lint = %+v", lint)
	}
	if defs["not-executable"].Problem == "" {
		t.Error("a source script run by its path needs the executable bit")
	}
	if defs["with-program"].Problem == "" {
		t.Error("a script that names a program has no problem")
	}

	// The same unexecutable script from this machine is copied in and made
	// executable, so only its shebang is asked for.
	if user := discover(t, dir, LayerUser)["not-executable"]; user.Problem != "" {
		t.Errorf("user-layer sandbox script problem = %q", user.Problem)
	}
}

func TestADeclarationIsCheckedForWhatItMeans(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"no-program.yaml":    "name: nothing\n",
		"bad-runs.yaml":      "runs: elsewhere\nprogram: x\n",
		"bad-key.yaml":       "key: ab\nprogram: x\n",
		"two-programs.yaml":  "program: [a, b]\n",
		"program-env.yaml":   "program: a\nprogram-env: A\n",
		"host-files.yaml":    "runs: host\nprogram: a\nfiles:\n  x: .x\n",
		"bad-env.yaml":       "program: a\nenv: [NOPE]\n",
		"bad-file-name.yaml": "program: a\nfiles:\n  ../x: .x\n",
	} {
		write(t, dir, name, body, 0o644)
	}
	for id, def := range discover(t, dir, LayerUser) {
		if def.Problem == "" {
			t.Errorf("%s has no problem: %+v", id, def)
		}
	}
}

func TestOnlyThisMachineDeclaresAHostTool(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "open.yaml", "runs: host\nprogram: open\nargs: ['{ssh.url}']\n", 0o644)

	for _, layer := range []Layer{LayerImage, LayerSource} {
		if def := discover(t, dir, layer)["open"]; !strings.Contains(def.Problem, "your machine") {
			t.Errorf("%s host tool problem = %q", layer, def.Problem)
		}
	}
	for _, layer := range []Layer{LayerBuiltin, LayerUser} {
		if def := discover(t, dir, layer)["open"]; def.Problem != "" {
			t.Errorf("%s host tool problem = %q", layer, def.Problem)
		}
	}
}

func TestMergeLetsTheLastLayerWin(t *testing.T) {
	builtin := []Definition{{ID: "vscode", FileName: "50-vscode.yaml", Runs: RunsHost, Layer: LayerBuiltin}}
	image := []Definition{
		{ID: "diff", FileName: "10-diff.yaml", Runs: RunsSandbox, Layer: LayerImage, Name: "image diff"},
		{ID: "vscode", FileName: "vscode.yaml", Runs: RunsSandbox, Layer: LayerImage},
	}
	source := []Definition{{ID: "diff", FileName: "10-diff.sh", Runs: RunsSandbox, Layer: LayerSource, Name: "source diff"}}
	user := []Definition{{ID: "vscode", FileName: "vscode.yaml", Runs: RunsHost, Layer: LayerUser, Name: "my vscode"}}

	merged := Merge(builtin, image, source, user)

	diff, ok := Find(merged, "diff")
	if !ok || diff.Name != "source diff" {
		t.Errorf("diff = %+v, want the source's", diff)
	}
	vscode, ok := Find(merged, "vscode")
	if !ok || vscode.Name != "my vscode" || vscode.Problem != "" {
		t.Errorf("vscode = %+v, want the user's", vscode)
	}
	// The image's attempt on a host tool is listed with its problem, beside the
	// tool it did not replace.
	var refused int
	for _, def := range merged {
		if def.ID == "vscode" && def.Layer == LayerImage {
			refused++
			if def.Problem == "" {
				t.Errorf("image vscode has no problem")
			}
		}
	}
	if refused != 1 || len(merged) != 3 {
		t.Errorf("merged = %+v", merged)
	}
	if merged[0].ID != "diff" {
		t.Errorf("merged is not in filename order: %+v", merged)
	}
}

func TestDiscoverFSReadsAnEmbeddedLayer(t *testing.T) {
	fsys := fstest.MapFS{
		"50-vscode.yaml": {Data: []byte("runs: host\nkey: v\nprogram: [code, codium]\nprogram-env: DISCOBOX_VSCODE\nargs: [--new-window]\nenv: [A=b]\n")},
	}
	defs, err := DiscoverFS(fsys, "", LayerBuiltin)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Problem != "" || defs[0].ProgramEnv != "DISCOBOX_VSCODE" || len(defs[0].Program) != 2 || defs[0].Env[0] != "A=b" {
		t.Fatalf("defs = %+v", defs)
	}
}

func TestRemoteExpand(t *testing.T) {
	r := Remote{SandboxID: "sbx_1", Host: "discobox.box", Workdir: "/home/agent/my repo"}
	got, err := r.Expand([]string{
		"--folder-uri", "vscode-remote://ssh-remote+{ssh.host}{workdir.urlpath}",
		"{ssh.url}", "{git.url}", "{workdir}", "{discobox.id}",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--folder-uri", "vscode-remote://ssh-remote+discobox.box/home/agent/my%20repo",
		"ssh://discobox.box/home/agent/my%20repo", "ssh://discobox.box/home/agent/my%20repo",
		"/home/agent/my repo", "sbx_1",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("expanded = %q\nwant %q", got, want)
	}

	root := Remote{Host: "discobox.box"}
	if got, _ := root.Expand([]string{"{ssh.url}"}); got[0] != "ssh://discobox.box/workspace" {
		t.Errorf("no working tree ssh url = %q", got[0])
	}
	if _, err := root.Expand([]string{"{git.url}"}); err == nil {
		t.Error("a git url with no working tree expanded")
	}
}

// The declarations the image actually ships, parsed from the tree it is built
// from: a typo in one is a tool that never appears in any discobox.
func TestTheShippedImageToolsAreValid(t *testing.T) {
	defs := discover(t, filepath.Join("..", "sandbox-agent", "image", "tools"), LayerImage)
	for _, id := range []string{DiffID, "fresh"} {
		def, ok := defs[id]
		if !ok {
			t.Fatalf("the image ships no %s tool; got %+v", id, defs)
		}
		if def.Problem != "" || def.Runs != RunsSandbox || def.Key == "" {
			t.Errorf("%s = %+v", id, def)
		}
	}
	for _, file := range defs["fresh"].Files {
		if file.Default == "" {
			t.Errorf("fresh's %s has no default beside the declaration", file.Name)
		}
	}
	if len(defs["fresh"].Files) != 2 {
		t.Errorf("fresh files = %+v", defs["fresh"].Files)
	}
}

// fresh's config has to tell an editor what it is twice over: the local copy
// says it in its .jsonc name, and the copy in the discobox — whose name fresh
// dictates — says it in a vim modeline within the first five lines, before the
// object where a comment is legal.
func TestTheShippedFreshConfigDeclaresItsFormat(t *testing.T) {
	fresh := discover(t, filepath.Join("..", "sandbox-agent", "image", "tools"), LayerImage)["fresh"]
	var config File
	for _, file := range fresh.Files {
		if file.Name == "config.jsonc" {
			config = file
		}
	}
	if config.Home != ".config/fresh/config.json" {
		t.Fatalf("config = %+v, want config.jsonc landing where fresh reads it", config)
	}
	lines := strings.Split(config.Default, "\n")
	found := false
	for i, line := range lines[:min(5, len(lines))] {
		if strings.Contains(line, "vim:") {
			found = true
			// The set form's options end at a colon; without it vim gives up.
			if !strings.Contains(line, "set ft=jsonc") || !strings.HasSuffix(strings.TrimSpace(line), ":") {
				t.Errorf("line %d is not a well-formed jsonc modeline: %q", i+1, line)
			}
		}
	}
	if !found {
		t.Error("no modeline in the first five lines")
	}
	if strings.Index(config.Default, "{") < strings.Index(config.Default, "vim:") {
		t.Error("the modeline must come before the object")
	}
}

// The live-diff seed is plugin state parsed by a strict JSON reader: a comment
// would take the whole file, and the enable with it, silently.
func TestTheShippedLiveDiffSeedIsStrictJSON(t *testing.T) {
	fresh := discover(t, filepath.Join("..", "sandbox-agent", "image", "tools"), LayerImage)["fresh"]
	for _, file := range fresh.Files {
		if file.Name != "live_diff.json" {
			continue
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(file.Default), &state); err != nil {
			t.Fatalf("does not parse as strict JSON: %v", err)
		}
		if state["live_diff.global_enabled"] != true {
			t.Errorf("global_enabled = %v, want true", state["live_diff.global_enabled"])
		}
		if mode, _ := state["live_diff.default_mode"].(map[string]any); mode["kind"] != "head" {
			t.Errorf("default_mode = %v, want HEAD", state["live_diff.default_mode"])
		}
		return
	}
	t.Fatal("fresh carries no live_diff.json")
}

func TestLookupTakesAnIDOrAName(t *testing.T) {
	defs := []Definition{
		{ID: DiffID, Name: "diff"},
		{ID: "fresh", Name: "fresh"},
		{ID: "a", Name: "twin"},
		{ID: "b", Name: "twin"},
		// A tool whose name is another's id does not shadow it.
		{ID: "c", Name: "fresh"},
	}
	for name, want := range map[string]string{DiffID: DiffID, "diff": DiffID, "fresh": "fresh", "c": "c"} {
		if got, ok := Lookup(defs, name); !ok || got.ID != want {
			t.Errorf("Lookup(%q) = %q, %v; want %q", name, got.ID, ok, want)
		}
	}
	if got, ok := Lookup(defs, "twin"); ok {
		t.Errorf("an ambiguous name resolved to %q", got.ID)
	}
}

// fresh's trust decision is recorded by its own script, under the working
// directory as fresh's `encode_path_for_filename` spells it. The cases are the
// ones where a plausible guess and the real rule differ, and the script is run
// for real: a wrong encoding is silent, the file landing where nothing reads it.
func TestTheShippedFreshScriptTrustsItsWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs in the discobox, under sh")
	}
	script, err := filepath.Abs(filepath.Join("..", "sandbox-agent", "image", "tools", "20-fresh.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, bin, "fresh", "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HOME/argv\"\n", 0o755)

	for _, tc := range []struct{ dir, slug string }{
		{"home/darren/src/disco2", "home_darren_src_disco2"}, // observed in a real discobox
		{"tmp/probe-dot", "tmp_probe-dot"},
		{"workspace/repo.git", "workspace_repo.git"},
		{"home/u/my_repo", "home_u_my%5Frepo"},       // "_" is not a separator
		{"home/u/my project", "home_u_my%20project"}, // everything else, per byte
	} {
		t.Run(tc.slug, func(t *testing.T) {
			home := t.TempDir()
			work := filepath.Join(t.TempDir(), tc.dir)
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			run := func() {
				cmd := exec.CommandContext(t.Context(), script)
				cmd.Dir = work
				cmd.Env = append(os.Environ(), "HOME="+home, "PWD="+work, "XDG_DATA_HOME=", "PATH="+bin+":"+os.Getenv("PATH"))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("run: %v\n%s", err, out)
				}
			}
			run()
			entries, err := os.ReadDir(filepath.Join(home, ".local/share/fresh/workspaces"))
			if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "_"+tc.slug) {
				t.Fatalf("workspaces = %v, %v; want one ending in %q", entries, err, tc.slug)
			}
			trust := filepath.Join(home, ".local/share/fresh/workspaces", entries[0].Name(), "trust.json")
			var state map[string]any
			data, _ := os.ReadFile(trust)
			if err := json.Unmarshal(data, &state); err != nil || state["level"] != "trusted" {
				t.Fatalf("trust.json = %q, %v", data, err)
			}
			// A choice made inside the box stands.
			if err := os.WriteFile(trust, []byte(`{"level":"restricted"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			run()
			if data, _ := os.ReadFile(trust); string(data) != `{"level":"restricted"}` {
				t.Fatalf("the script overwrote a recorded decision: %q", data)
			}
			// And fresh is opened on the directory.
			if argv, _ := os.ReadFile(filepath.Join(home, "argv")); string(argv) != ".\n" {
				t.Fatalf("fresh argv = %q, want the directory", argv)
			}
		})
	}
}
