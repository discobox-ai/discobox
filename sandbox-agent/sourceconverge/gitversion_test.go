package sourceconverge

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

func TestParseGitVersion(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want [2]int
		ok   bool
	}{
		{"git version 2.43.0\n", [2]int{2, 43}, true},
		{"git version 2.56.0", [2]int{2, 56}, true},
		{"git version 2.39.5 (Apple Git-154)", [2]int{2, 39}, true},
		{"git version 2.46.0.windows.1", [2]int{2, 46}, true},
		{"git version", [2]int{}, false},
		{"git version x.y", [2]int{}, false},
	} {
		got, ok := parseGitVersion(tc.out)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseGitVersion(%q) = %v, %v; want %v, %v", tc.out, got, ok, tc.want, tc.ok)
		}
	}
}

// oldGitOnPath puts a git first on the PATH that reports itself as 2.43 and
// runs the real one for everything else, as a box does before its core feature
// has installed a git that takes a bearer credential.
func oldGitOnPath(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = version ]; then echo 'git version 2.43.0'; exit 0; fi\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A source with a token waits on the sandbox, saying why, while its git cannot
// take a bearer credential, and is cloned once it can. One already in place is
// not taken back to waiting by an old git: only the clone sends the token.
func TestASourceWaitsOnTheSandboxForAGitThatTakesItsToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell-script git")
	}
	path := os.Getenv("PATH")
	o := newOrigin(t)
	first := o.commit("README.md", "hello\n")
	target := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, sandboxconfig.Source{Slug: "primary", Target: target, RefName: "main", RefType: "branch"})
	source := sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}

	oldGitOnPath(t)
	state := h.pass(t, document(1, source), "primary")
	if state.State != StateWaiting || state.WaitingOn != WaitingOnSandbox || !strings.Contains(state.Error, "2.43") {
		t.Fatalf("state = %+v, want waiting on the sandbox's git", state)
	}

	t.Setenv("PATH", path)
	if state := h.pass(t, document(2, source), "primary"); state.State != StateMaterialized || state.Commit != first {
		t.Fatalf("state = %+v, want materialized once git takes the token", state)
	}

	oldGitOnPath(t)
	if state := h.pass(t, document(3, source), "primary"); state.State != StateMaterialized || state.Error != "" {
		t.Fatalf("state = %+v, want a materialized source left materialized", state)
	}
}
