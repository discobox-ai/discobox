package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBuildEnvConstructsDiscoboxContract(t *testing.T) {
	req := Request{
		Hook: HookDefinition{
			ID:      "hook-id",
			Name:    "Hook Name",
			Type:    "file",
			Path:    ".discobox/hooks/hook-id",
			Pattern: "**/*.go",
		},
		SessionID:    "session-1",
		RepoRoot:     "/repo",
		RunID:        "run-1",
		ChangedFiles: []string{"a.go", "dir/b.go"},
		DBPath:       "/tmp/hooks.db",
		SocketPath:   "/tmp/hooks.sock",
		Environ:      []string{"PATH=/bin", "DISCOBOX_SESSION_ID=old", "CUSTOM=base"},
		Env:          map[string]string{"CUSTOM": "override", "EXTRA": "value"},
	}

	env := envMap(BuildEnv(req, `["a.go","dir/b.go"]`))

	assertEnv(t, env, "PATH", "/bin")
	assertEnv(t, env, "CUSTOM", "override")
	assertEnv(t, env, "EXTRA", "value")
	assertEnv(t, env, "DISCOBOX_SESSION_ID", "session-1")
	assertEnv(t, env, "DISCOBOX_REPO_ROOT", "/repo")
	assertEnv(t, env, "DISCOBOX_WORKSPACE", "/repo")
	assertEnv(t, env, "DISCOBOX_HOOK_ID", "hook-id")
	assertEnv(t, env, "DISCOBOX_HOOK_NAME", "Hook Name")
	assertEnv(t, env, "DISCOBOX_HOOK_TYPE", "file")
	assertEnv(t, env, "DISCOBOX_HOOK_PATH", ".discobox/hooks/hook-id")
	assertEnv(t, env, "DISCOBOX_HOOK_PATTERN", "**/*.go")
	assertEnv(t, env, "DISCOBOX_HOOK_RUN_ID", "run-1")
	assertEnv(t, env, "DISCOBOX_CHANGED_FILES", "a.go\ndir/b.go")
	assertEnv(t, env, "DISCOBOX_CHANGED_FILES_JSON", `["a.go","dir/b.go"]`)
	assertEnv(t, env, "DISCOBOX_DB_PATH", "/tmp/hooks.db")
	assertEnv(t, env, "DISCOBOX_SOCKET_PATH", "/tmp/hooks.sock")
}

func TestRunSuccessCapturesCombinedOutputAndChangedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script execution test is Unix-only")
	}
	repo := t.TempDir()
	script := writeScript(t, repo, "hook.sh", `#!/bin/sh
printf 'cwd=%s\n' "$PWD"
printf 'session=%s\n' "$DISCOBOX_SESSION_ID"
printf 'stderr-line\n' >&2
`)
	res := Run(context.Background(), Request{
		Hook:         HookDefinition{ID: "h1", Name: "Hook", Type: "file", Command: script},
		SessionID:    "s1",
		RepoRoot:     repo,
		RunID:        "r1",
		ChangedFiles: []string{"main.go"},
		Environ:      []string{"PATH=" + os.Getenv("PATH")},
	})

	if !res.Success {
		t.Fatalf("Run success = false, exit=%d err=%v output=%q", res.ExitCode, res.Err, res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "cwd="+repo) || !strings.Contains(res.Output, "session=s1") || !strings.Contains(res.Output, "stderr-line") {
		t.Fatalf("Output missing expected combined content: %q", res.Output)
	}
}

func TestRunFailureReturnsExitCodeAndOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script execution test is Unix-only")
	}
	repo := t.TempDir()
	script := writeScript(t, repo, "fail.sh", `#!/bin/sh
echo before-failure
exit 7
`)
	res := Run(context.Background(), Request{
		Hook:     HookDefinition{ID: "h1", Type: "file", Command: script},
		RepoRoot: repo,
		Environ:  []string{"PATH=" + os.Getenv("PATH")},
	})

	if res.Success {
		t.Fatal("Run success = true, want false")
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil for process exit failure", res.Err)
	}
	if !strings.Contains(res.Output, "before-failure") {
		t.Fatalf("Output missing process output: %q", res.Output)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group timeout test is Unix-only")
	}
	repo := t.TempDir()
	marker := filepath.Join(repo, "late-marker")
	script := writeScript(t, repo, "timeout.sh", `#!/bin/sh
(sleep 1; echo late > late-marker)&
echo started
sleep 5
`)

	res := Run(context.Background(), Request{
		Hook:     HookDefinition{ID: "h1", Type: "file", Command: script},
		RepoRoot: repo,
		Timeout:  300 * time.Millisecond,
		Environ:  []string{"PATH=" + os.Getenv("PATH")},
	})

	if res.Success {
		t.Fatal("Run success = true, want timeout failure")
	}
	if !res.TimedOut {
		t.Fatalf("TimedOut = false, result: %+v", res)
	}
	if res.ExitCode != ExitCodeTimeout {
		t.Fatalf("ExitCode = %d, want %d", res.ExitCode, ExitCodeTimeout)
	}
	if !strings.Contains(res.Output, "started") {
		t.Fatalf("Output missing pre-timeout output: %q", res.Output)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("process group child was not killed; marker stat err=%v", err)
	}
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

func assertEnv(t *testing.T, env map[string]string, key, want string) {
	t.Helper()
	if got := env[key]; got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

func writeScript(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}
