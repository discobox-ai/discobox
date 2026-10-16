package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func testDirs(t *testing.T) dirs {
	root := t.TempDir()
	return dirs{lease: filepath.Join(root, "keepalive"), sleep: filepath.Join(root, "sleep"), wake: filepath.Join(root, "wake"), controlPlane: filepath.Join(root, "control-plane-power")}
}

func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime().UTC()
}

func TestSleepWithAWakeWritesBothDatedAndNothingElse(t *testing.T) {
	d := testDirs(t)
	if err := run(d, []string{"sleep", "--wake-in", "45m", "--note", "check CI"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := mtime(t, filepath.Join(d.wake, "sleep")); !got.Equal(now.Add(45 * time.Minute)) {
		t.Fatalf("wake at %v, want 45m on", got)
	}
	if note, _ := os.ReadFile(filepath.Join(d.wake, "sleep")); strings.TrimSpace(string(note)) != "check CI" {
		t.Fatalf("note = %q", note)
	}
	if got := mtime(t, filepath.Join(d.sleep, "agent")); !got.Equal(now) {
		t.Fatalf("sleep requested at %v, want now", got)
	}
	// The temp file a request is dated under never stays behind.
	for _, dir := range []string{d.wake, d.sleep} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("%s holds %d entries, want the one request", dir, len(entries))
		}
	}
}

func TestWakeAtAClockTimeIsTheNextOneToCome(t *testing.T) {
	d := testDirs(t)
	if err := run(d, []string{"wake", "ci", "--at", "09:30"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := mtime(t, filepath.Join(d.wake, "ci")), time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("wake at %v, want tomorrow's %v", got, want)
	}
	if err := run(d, []string{"wake", "rm", "ci"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.wake, "ci")); !os.IsNotExist(err) {
		t.Fatalf("wake still there after rm: %v", err)
	}
}

func TestKeepaliveForADuration(t *testing.T) {
	d := testDirs(t)
	if err := run(d, []string{"keepalive", "build", "--for", "2h"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := mtime(t, filepath.Join(d.lease, "build")); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("lease until %v, want 2h on", got)
	}
}

func TestRefusesNamesNothingWouldRead(t *testing.T) {
	d := testDirs(t)
	for _, name := range []string{".hidden", "../escape", "a/b", ""} {
		if err := run(d, []string{"wake", name, "--in", "1h"}, now, io.Discard); err == nil {
			t.Errorf("wake %q accepted", name)
		}
	}
	if err := run(d, []string{"wake", "x", "--in", "1h", "--at", "10:00"}, now, io.Discard); err == nil {
		t.Error("accepted both --in and --at")
	}
}

// Without the control plane holding the power nothing acts on a wake, and an
// agent must not be told otherwise.
func TestAWakeWarnsWhenNothingWillActOnIt(t *testing.T) {
	d := testDirs(t)
	var out strings.Builder
	if err := run(d, []string{"wake", "ci", "--in", "1h"}, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning:") {
		t.Fatalf("output = %q, want a warning that nothing will wake the sandbox", out.String())
	}
	if err := os.WriteFile(d.controlPlane, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(d, []string{"wake", "ci", "--in", "1h"}, now, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "warning:") {
		t.Fatalf("output = %q, want no warning once the control plane holds the power", out.String())
	}
}

// status shows what the sandbox agent reads: a dot-named lease holds the box
// up, so it is shown.
func TestStatusShowsADotNamedLease(t *testing.T) {
	d := testDirs(t)
	if err := os.MkdirAll(d.lease, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.lease, ".hidden"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run(d, []string{"status"}, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ".hidden") {
		t.Fatalf("status = %q, want the dot-named lease", out.String())
	}
}
