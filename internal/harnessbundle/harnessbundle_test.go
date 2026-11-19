package harnessbundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
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
		for _, p := range []string{"/usr/local/bin/discobox-harness-run", bundle.Declaration.CaptureCommand()} {
			info, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
			if err == nil && info.Mode().Perm()&0o111 == 0 {
				t.Errorf("%s installs %s not executable", dir, p)
			}
		}
	}
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
