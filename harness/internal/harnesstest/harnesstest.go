// Package harnesstest stages a built-in harness's bundle from its
// declaration and lays the bundle's feature out under a test directory the
// way a box's provisioning installs it, so a harness's tests exercise what a
// box runs: the generated launcher, the shared runtime and the vendor adapter,
// not the declaration's intent (ADR 26-11-06-596).
package harnesstest

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/internal/harnessbundle"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// SourceDataPath is the mount the launcher looks for before wiring up
// source-scoped memory.
const SourceDataPath = "/.discobox/data-per-source/primary"

// Staged is one harness's staged bundle.
type Staged struct {
	*harnessbundle.Bundle
	// Dir is the staged build context.
	Dir string
	// Feature is the staged feature, as the sandbox agent loads it.
	Feature provisionspec.Feature
}

// Tree is the harness source tree (harness/), found from the working
// directory up.
func Tree(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, harnessbundle.Dockerfile)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("not inside the harness tree")
		}
		dir = parent
	}
}

// Stage stages directory's bundle into a test directory; "." is the working
// directory's harness.
func Stage(t testing.TB, directory string) *Staged {
	t.Helper()
	tree := Tree(t)
	if directory == "." {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		directory = filepath.Base(wd)
	}
	bundle, err := harnessbundle.Generate(os.DirFS(tree), directory)
	if err != nil {
		t.Fatal(err)
	}
	s := &Staged{Bundle: bundle, Dir: t.TempDir()}
	if err := bundle.Write(s.Dir); err != nil {
		t.Fatal(err)
	}
	if bundle.Declaration.Coding() {
		s.Feature, err = provisionspec.LoadFeature(filepath.Join(s.Dir, filepath.FromSlash(bundle.FeatureDir())))
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// Declaration is the staged harness's declaration.
func (s *Staged) Declaration() *harness.Declaration { return s.Bundle.Declaration }

// Label is the staged manifest label.
func (s *Staged) Label() []byte { return s.Files[harnessbundle.LabelFile].Content }

// FeatureFile is a staged feature file's content, by its name in the feature.
func (s *Staged) FeatureFile(t testing.TB, name string) []byte {
	t.Helper()
	f, ok := s.Files[s.FeatureDir()+"/"+name]
	if !ok {
		t.Fatalf("the feature has no %s", name)
	}
	return f.Content
}

// Installs is the path a file step places source at, or "" when no step
// places it.
func (s *Staged) Installs(source string) string {
	for _, step := range s.Feature.Spec.Steps {
		if step.File != nil && step.File.Source == source {
			return step.File.Path
		}
	}
	return ""
}

// Installed is the file a step installs at path: its staged content.
func (s *Staged) Installed(t testing.TB, path string) []byte {
	t.Helper()
	for _, step := range s.Feature.Spec.Steps {
		if step.File != nil && step.File.Path == path {
			return s.FeatureFile(t, step.File.Source)
		}
	}
	t.Fatalf("no step installs %s", path)
	return nil
}

// Runs reports whether a script one of its run steps names holds text.
func (s *Staged) Runs(t testing.TB, text string) bool {
	t.Helper()
	for _, step := range s.Feature.Spec.Steps {
		if step.Run != "" && strings.Contains(string(s.FeatureFile(t, step.Run)), text) {
			return true
		}
	}
	return false
}

// InstallsPackage reports whether a packages step installs the package.
func (s *Staged) InstallsPackage(name string) bool {
	for _, step := range s.Feature.Spec.Steps {
		if step.Packages != nil && slices.Contains(slices.Concat(step.Packages.Debian, step.Packages.Ubuntu), name) {
			return true
		}
	}
	return false
}

// Box is a staged feature installed under Root as a box's provisioning
// installs it: every file step's file at Root plus its path, and every
// absolute path the installed scripts and documents name rewritten to its
// place under Root, so nothing outside the test directory is read or written.
type Box struct {
	Root string
	// Home is the sandbox user's home.
	Home string
	// Bin holds the stubs put ahead of PATH.
	Bin      string
	env      []string
	replacer *strings.Replacer
}

// rooted are the absolute paths the generated feature and the shared runtime
// name, which a test installation puts under its root.
var rooted = []string{harness.LibexecDir, "/usr/local/bin/", "/etc/", "/run/lock/", SourceDataPath}

// Install installs the staged feature under a test directory. paths maps
// absolute paths to the test directories standing in for them; the source's
// data is a path that does not exist unless paths says otherwise, so by
// default the launcher takes the branch a source-less box does.
func (s *Staged) Install(t testing.TB, paths map[string]string) *Box {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a harness's feature runs on Linux")
	}
	dir := t.TempDir()
	b := &Box{Root: filepath.Join(dir, "root"), Home: filepath.Join(dir, "home"), Bin: filepath.Join(dir, "bin")}
	rewrites := map[string]string{SourceDataPath: filepath.Join(dir, "no-source-data")}
	for _, prefix := range rooted {
		if prefix != SourceDataPath {
			rewrites[prefix] = b.Root + prefix
		}
	}
	maps.Copy(rewrites, paths)
	for _, to := range rewrites {
		// The rewritten paths land inside single-quoted shell words.
		if strings.ContainsAny(to, `'\`) {
			t.Skipf("test directory %q cannot be named in a single-quoted shell word", to)
		}
	}
	// Longest first: the replacer takes the first of its arguments that
	// matches, and a path paths names is more specific than its prefix.
	froms := slices.Sorted(maps.Keys(rewrites))
	slices.SortStableFunc(froms, func(a, b string) int { return len(b) - len(a) })
	var pairs []string
	for _, from := range froms {
		pairs = append(pairs, from, rewrites[from])
	}
	replacer := strings.NewReplacer(pairs...)
	b.replacer = replacer
	for _, d := range []string{b.Root, b.Home, b.Bin, filepath.Join(b.Root, "run", "lock")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range s.Feature.Spec.Steps {
		if step.File == nil {
			continue
		}
		content := s.FeatureFile(t, step.File.Source)
		if text := string(content); !strings.ContainsRune(text, 0) {
			content = []byte(replacer.Replace(text))
		}
		target := b.Path(step.File.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0o755); err != nil { //nolint:gosec // Installed scripts are executable.
			t.Fatal(err)
		}
	}
	// sudo runs its command as the test's own user, which is enough for the
	// directories and files a launcher makes; a mount it attempts fails, and
	// the launcher falls back the way it would in a box without the grant.
	b.Stub(t, "sudo", "#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec \"$@\"\n")
	b.env = []string{"HOME=" + b.Home, "PATH=" + b.Bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	return b
}

// Path is where path is installed in the box: where the paths Install was
// given say, otherwise under Root.
func (b *Box) Path(path string) string {
	if rewritten := b.replacer.Replace(path); rewritten != path {
		return filepath.FromSlash(rewritten)
	}
	return filepath.Join(b.Root, filepath.FromSlash(path))
}

// Stub puts an executable POSIX shell script named name ahead of PATH.
func (b *Box) Stub(t testing.TB, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.Bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // A stub stands in for a command; it has to be executable.
		t.Fatal(err)
	}
}

// Write writes a file into the home.
func (b *Box) Write(t testing.TB, rel, content string) {
	t.Helper()
	p := filepath.Join(b.Home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Env adds environment variables to every command the box runs.
func (b *Box) Env(kv ...string) { b.env = append(b.env, kv...) }

// Command runs an installed command, by its installed path, in the box's
// environment. The caller's environment is kept but for the variables a
// harness reads from it.
func (b *Box) Command(t testing.TB, path string, args ...string) *exec.Cmd {
	t.Helper()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no POSIX shell: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), shell, append([]string{b.Path(path)}, args...)...) //nolint:gosec // An installed script under test.
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "HOME" || name == "PATH" || strings.HasPrefix(name, "OPENCODE_") || strings.HasPrefix(name, "XDG_") ||
			strings.HasPrefix(name, "DISCOBOX_") || strings.HasPrefix(name, "CODEX_") || name == "SSH_CONNECTION" ||
			name == "PI_PROXY" || name == "HTTPS_PROXY" {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, b.env...)
	cmd.Dir = b.Home
	cmd.Stderr = os.Stderr
	return cmd
}

// Launch runs the installed discobox-harness-run with args and returns its
// standard output and exit status.
func (b *Box) Launch(t testing.TB, args ...string) (string, int) {
	t.Helper()
	cmd := b.Command(t, harnessbundle.Launcher, args...)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("launch %v: %v", args, err)
	}
	return string(out), 0
}

// RunLauncher stages the working directory's harness, installs it, stubs
// agent to print each argument it is given on a line, launches it with args,
// and returns the argv the agent was given.
func RunLauncher(t *testing.T, agent string, paths map[string]string, args []string) []string {
	t.Helper()
	b := Stage(t, ".").Install(t, paths)
	b.Stub(t, agent, ArgvStub)
	return b.Argv(t, args...)
}

// ArgvStub prints one argument per line, which is unambiguous for the
// prompts a launcher test asks about: what it proves is that words the shell
// split arrive as one argument.
const ArgvStub = "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\n' \"$arg\"; done\n"

// Argv launches the box with args and returns the lines the agent stub
// printed, failing the test on a non-zero exit.
func (b *Box) Argv(t testing.TB, args ...string) []string {
	t.Helper()
	out, status := b.Launch(t, args...)
	if status != 0 {
		t.Fatalf("launch %v: exit status %d", args, status)
	}
	trimmed := strings.TrimSuffix(out, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// RequireNode skips a test whose launch runs the shared capture runtime.
func RequireNode(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for the harness runtime")
	}
}
