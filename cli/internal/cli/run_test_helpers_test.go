package cli

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func preparePromptCreateSSHSync(t *testing.T) func(http.ResponseWriter, *http.Request) bool {
	t.Helper()
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
			_, _ = w.Write([]byte(`{"id":"project-1","name":"P","ownerUserId":"user-1","default":false,"welcomed":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/ssh":
			_, _ = w.Write([]byte(sshConfigEnabledIngress))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/ssh-keys":
			_, _ = w.Write([]byte(`{"sshKeys":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/ssh-keys":
			_, _ = w.Write([]byte(`{"id":"sshkey_1","projectId":"project-1","publicKey":"ssh-ed25519 AAAA","fingerprint":"SHA256:test","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes":
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sbx_9qk5n25t2hh2rv00","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
		default:
			return false
		}
		return true
	}
}

func newRunSourceTestRepo(t *testing.T) string {
	t.Helper()
	// Canonicalized because callers compare it against a path that came back
	// out of git: Windows hands t.TempDir() an 8.3 short name (RUNNER~1) where
	// git reports the long one, and macOS hands out /var where git reports
	// /private/var. Both are the same directory by two names.
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := runSourceTestGit(t, repo)
	git("init", "-b", "feature-foo")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-m", "base")
	return repo
}

func runSourceTestGit(t *testing.T, dir string) func(...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
		}
		return string(out)
	}
}
