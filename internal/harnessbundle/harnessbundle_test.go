package harnessbundle

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

func tree(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../harness")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// The label is the published contract, and moving a built-in onto its
// declaration changed none of it (ADR 26-11-06-596 §4): every generated
// label reads as the one the hand-written image.json published before it,
// testdata/labels/<directory>.json. A deliberate change to a label changes
// its file here too, so the change is reviewed as one.
func TestLabelsAreThoseTheBuiltInsPublishedBefore(t *testing.T) {
	for _, dir := range harness.BuiltinDirectories() {
		t.Run(dir, func(t *testing.T) {
			bundle, err := Generate(os.DirFS(tree(t)), dir)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "labels", dir+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := normalized(t, bundle.Files[LabelFile].Content), normalized(t, want); !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", " ")
				w, _ := json.MarshalIndent(want, "", " ")
				t.Fatalf("label differs from the published one:\n got %s\nwant %s", g, w)
			}
		})
	}
}

// normalized reads a label as its readers do: an empty list is no list.
func normalized(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var drop func(any) any
	drop = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				if list, ok := item.([]any); ok && len(list) == 0 {
					delete(x, k)
					continue
				}
				x[k] = drop(item)
			}
		case []any:
			for i, item := range x {
				x[i] = drop(item)
			}
		}
		return v
	}
	return drop(v)
}

// The generated feature installs everything at the paths the hand-written
// features did: what a box, a pinned bundle's save-back command or a person
// reads is where it was (ADR 26-11-06-596 §5).
func TestFeaturesInstallAtTheExistingPaths(t *testing.T) {
	for dir, paths := range map[string][]string{
		"claude-code": {"/usr/local/bin/discobox-harness-run", "/usr/local/libexec/discobox/agent.conf", "/usr/local/libexec/discobox/claude-code-capture",
			"/usr/local/libexec/discobox/claude-code-image.json", "/etc/claude-code/managed-settings.json"},
		"codex-cli": {"/usr/local/bin/discobox-harness-run", "/usr/local/libexec/discobox/codex-capture", "/usr/local/libexec/discobox/codex-image.json",
			"/etc/codex/config.toml", "/etc/codex/hooks.json", "/usr/local/libexec/discobox/codex-system-config.toml"},
		"copilot":  {"/usr/local/libexec/discobox/copilot-capture", "/etc/github-copilot/policy.d/discobox.json"},
		"opencode": {"/usr/local/libexec/discobox/opencode-capture", "/etc/opencode/opencode.json", "/usr/local/libexec/discobox/opencode-hook-plugin/server.js", "/usr/local/libexec/discobox/opencode-hook-plugin/package.json"},
		"omp":      {"/usr/local/libexec/discobox/omp-capture", "/etc/omp/discobox.yml", "/etc/profile.d/omp-proxy.sh", "/usr/local/libexec/discobox/omp-hook-extension.js"},
		"pi":       {"/usr/local/libexec/discobox/pi-capture", "/usr/local/libexec/discobox/pi-hook-extension.js"},
	} {
		bundle, err := Generate(os.DirFS(tree(t)), dir)
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		if err := bundle.Install(root); err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
				t.Errorf("%s installs nothing at %s", dir, p)
			}
		}
		// What a box runs is executable: its step says so, which is what the
		// box applies, and so does the file installed here, on a filesystem
		// that keeps POSIX permissions.
		spec, err := provisionspec.Parse(bundle.Files[bundle.FeatureDir()+"/"+provisionspec.FileName].Content)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"/usr/local/bin/discobox-harness-run", bundle.Declaration.CaptureCommand()} {
			if mode := stepMode(spec, p); mode != "0755" {
				t.Errorf("%s installs %s with mode %q, want 0755", dir, p, mode)
			}
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
			if err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
				t.Errorf("%s installs %s not executable", dir, p)
			}
		}
	}
}

// stepMode is the mode the file step installing path declares.
func stepMode(spec provisionspec.Spec, path string) string {
	for _, step := range spec.Steps {
		if step.File != nil && step.File.Path == path {
			return step.File.Mode
		}
	}
	return ""
}

// The shell's bundle is its label, with no feature and so no layer.
func TestTheShellsBundleIsItsLabel(t *testing.T) {
	bundle, err := Generate(os.DirFS(tree(t)), "shell")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Target != TargetLabel || len(bundle.Files) != 3 {
		t.Fatalf("shell stages %d files for %s", len(bundle.Files), bundle.Target)
	}
	for name := range bundle.Files {
		if strings.HasPrefix(name, "features/") {
			t.Fatalf("shell stages %s", name)
		}
	}
}

// Staging replaces only a staged bundle, removes what is no longer generated,
// and leaves an unchanged file as it was.
func TestWriteKeepsToAStagedBundle(t *testing.T) {
	bundle, err := Generate(os.DirFS(tree(t)), "pi")
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "mine.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(other); err == nil {
		t.Fatal("Write replaced a directory it did not stage")
	}
	if _, err := os.Stat(filepath.Join(other, "mine.txt")); err != nil {
		t.Fatal("Write removed a file it did not stage")
	}
	dir := filepath.Join(t.TempDir(), "pi")
	if err := bundle.Write(dir); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "features", "pi", "stale.sh")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	label := filepath.Join(dir, LabelFile)
	old := time.Unix(1000, 0)
	if err := os.Chtimes(label, old, old); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a file no longer generated lingers")
	}
	if info, err := os.Stat(label); err != nil || !info.ModTime().Equal(old) {
		t.Fatal("an unchanged file was written again")
	}
}

// The launcher quotes every declared value it writes, so it holds no shell
// even where Validate would let one through: each value reaches the runtime
// as one literal word, and nothing in it runs.
func TestTheLauncherQuotesEveryDeclaredValue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs the launcher with sh")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	hostile := "a b'\"`touch " + marker + "`$(touch " + marker + ");touch " + marker + "\ntouch " + marker
	for _, dir := range []string{"claude-code", "codex-cli"} {
		t.Run(dir, func(t *testing.T) {
			d, err := harness.LoadDeclaration(os.DirFS(tree(t)), dir)
			if err != nil {
				t.Fatal(err)
			}
			d.Name, d.Directory = hostile, hostile
			d.Install.Executable = hostile
			d.Environment.Defaults = map[string]string{hostile: hostile}
			d.Environment.Aliases = []harness.Alias{{Name: hostile, From: hostile}}
			d.LocalState.Memory.Name = hostile
			d.Launch.Args = []string{hostile}
			d.Launch.Resume = []string{hostile}
			d.Launch.Prompt.Form = hostile
			d.Launch.Prompt.Option = hostile
			d.Launch.Prompt.LeadingAt = hostile
			if d.Launch.Home != nil {
				d.Launch.Home.Env, d.Launch.Home.Default = hostile, hostile
			}
			if d.Launch.Retry != nil {
				d.Launch.Retry.Sessions = hostile
			}
			script, err := launcherScript(d)
			if err != nil {
				t.Fatal(err)
			}
			// The runtime's functions, each printing its words one to a line.
			source := ". " + shellQuote(RuntimeInstallDir+"/launch.sh") + "\n"
			if !strings.Contains(string(script), source) {
				t.Fatalf("launcher sources no runtime:\n%s", script)
			}
			var stubs strings.Builder
			for _, fn := range []string{"env_default", "env_alias", "memory_managed_setting", "memory_bind_mount", "trust_system_layer", "prepare", "run"} {
				fmt.Fprintf(&stubs, "discobox_harness_%s() { printf '%%s\\n' \"$@\"; }\n", fn)
			}
			//nolint:gosec // G204: the generated launcher under test, its runtime stubbed.
			cmd := exec.CommandContext(t.Context(), "sh", "-c", strings.Replace(string(script), source, stubs.String(), 1))
			cmd.Dir = t.TempDir()
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("a declared value ran:\n%s", script)
			}
			// Each value arrives whole: with every whole one taken out, no
			// part of one is left.
			if rest := strings.ReplaceAll(string(out), hostile, ""); strings.Contains(rest, "touch") {
				t.Errorf("a declared value split or changed:\n%s", out)
			}
			if !strings.Contains(string(out), "\n"+hostile+"\n") {
				t.Errorf("no declared value arrived as one word:\n%s", out)
			}
		})
	}
}
