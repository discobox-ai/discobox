package dockercache

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want retryAction
	}{
		{"clean dockerfile error", "ERROR: process \"/bin/sh -c false\" did not complete successfully: exit code: 1", retryNone},
		{"corrupt snapshot", "failed to solve: parent snapshot sha256:abc does not exist: not found", retryPrune},
		{"corrupt ingest", "failed to resume the status from path /var/lib/docker/buildkit/ingest/abc", retryPrune},
		{"disk full", "failed to copy: write /cache/blobs: no space left on device", retryWithoutExport},
		{"lock contention", "could not lock /home/u/.cache/discobox/buildkit/index.json.lock", retryBackoff},
		{"empty", "", retryNone},
	} {
		if got := classify(tc.out); got != tc.want {
			t.Errorf("%s: classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A build can fail to export while the lock message is also present; the
// contention case is the actionable one and must win.
func TestClassifyPrefersLockContention(t *testing.T) {
	out := "exporting cache to client directory\ncould not lock /c/index.json.lock"
	if got := classify(out); got != retryBackoff {
		t.Fatalf("classify = %v, want retryBackoff", got)
	}
}

func TestStripCacheTo(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{
			in:   []string{"docker", "buildx", "build", "--cache-from", "type=local,src=/c", "--cache-to", "type=local,dest=/c,mode=max", "."},
			want: []string{"docker", "buildx", "build", "--cache-from", "type=local,src=/c", "."},
		},
		{
			in:   []string{"docker", "buildx", "build", "--cache-to=type=local,dest=/c", "."},
			want: []string{"docker", "buildx", "build", "."},
		},
	} {
		if got := stripCacheTo(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("stripCacheTo(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// --cache-from must survive: a retry should still read cache even when it
	// has stopped writing it.
	got := stripCacheTo([]string{"docker", "build", "--cache-from", "type=local,src=/c", "--cache-to", "x", "."})
	if !slices.Contains(got, "--cache-from") {
		t.Fatalf("stripCacheTo dropped --cache-from: %v", got)
	}
}

func TestTailBufferKeepsTail(t *testing.T) {
	tb := &tailBuffer{max: 8}
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		if _, err := tb.Write([]byte(s)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := tb.String(); got != "bbbbcccc" {
		t.Fatalf("tail = %q, want %q", got, "bbbbcccc")
	}
}

// runRelayed must report the child's status faithfully and capture its stderr
// while still writing it through.
func TestRunRelayedCapturesStderrAndExitCode(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	code, out := runRelayed([]string{sh, "-c", "echo to-stderr >&2; exit 3"})
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(out, "to-stderr") {
		t.Fatalf("captured stderr = %q, want it to contain %q", out, "to-stderr")
	}
}

// stdout must stay on its own stream: `docker build -q` writes the image ID
// there and callers capture it.
func TestRunRelayedLeavesStdoutUncaptured(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	code, out := runRelayed([]string{sh, "-c", "echo on-stdout; echo on-stderr >&2; exit 1"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(out, "on-stdout") {
		t.Fatalf("stdout leaked into the stderr capture: %q", out)
	}
	if !strings.Contains(out, "on-stderr") {
		t.Fatalf("stderr not captured: %q", out)
	}
}

func TestRunRelayedSucceeds(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	if code, _ := runRelayed([]string{sh, "-c", "exit 0"}); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

// An empty home must never create a cache directory relative to the cwd.
func TestRunWithoutHomeDoesNotCreateRelativeCache(t *testing.T) {
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if got := Rewrite([]string{"build", "."}, ""); got.Injected {
		t.Fatalf("must not inject without a home directory: %v", got.Argv)
	}
	if _, err := os.Stat(".cache"); !os.IsNotExist(err) {
		t.Fatalf("a relative .cache directory was created: %v", err)
	}
}
