package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/autostop"
)

var now = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func testDirs(t *testing.T) powerDirs {
	root := t.TempDir()
	return powerDirs{lease: filepath.Join(root, "keepalive"), sleep: filepath.Join(root, "sleep"), wake: filepath.Join(root, "wake"), controlPlane: filepath.Join(root, "control-plane-power")}
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
	if err := power(d, []string{"sleep", "--wake-in", "45m", "--note", "check CI"}, now, io.Discard); err != nil {
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

func TestSleepKeepsANoteWithSpacesAndPunctuationWhole(t *testing.T) {
	const text = "discobox#503: backport pushed (8b9cfbe), blocked on release/0.18 existing"
	d := testDirs(t)
	if err := power(d, []string{"sleep", "--note", text, "--wake-in", "1m"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if note, _ := os.ReadFile(filepath.Join(d.wake, "sleep")); strings.TrimSpace(string(note)) != text {
		t.Fatalf("note = %q, want %q", note, text)
	}
	if _, err := os.Stat(filepath.Join(d.wake, autostop.WakeNextDir, "sleep")); !os.IsNotExist(err) {
		t.Fatalf("a note with a wake was also kept for the next start: %v", err)
	}
}

// A note with no wake is accepted and kept for the next start, out of the
// wake directory's reading, so it asks for no start (ADR 26-11-17-583).
func TestSleepWithANoteAndNoWakeKeepsItForTheNextStart(t *testing.T) {
	const text = "discobox#518: PR 1262 green, ready (a96f8d1)"
	d := testDirs(t)
	var out strings.Builder
	if err := power(d, []string{"sleep", "--note", text}, now, &out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d.wake, autostop.WakeNextDir, "sleep")
	if note, _ := os.ReadFile(path); strings.TrimSpace(string(note)) != text {
		t.Fatalf("note = %q, want %q", note, text)
	}
	if got := mtime(t, path); !got.Equal(now) {
		t.Fatalf("note dated %v, want now", got)
	}
	if wakes := autostop.ReadRequests(d.wake); len(wakes) != 0 {
		t.Fatalf("wakes = %v, want none: a note asks for no start", wakes)
	}
	if got := mtime(t, filepath.Join(d.sleep, "agent")); !got.Equal(now) {
		t.Fatalf("sleep requested at %v, want now", got)
	}
	if strings.Contains(out.String(), "warning") {
		t.Fatalf("out = %q, want no warning that nothing will wake it: nothing was asked to", out.String())
	}
	out.Reset()
	if err := power(d, []string{"status"}, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "note for the next start:\n  sleep\t") {
		t.Fatalf("status = %q, want the note listed", out.String())
	}
}

func TestWakeAtAClockTimeIsTheNextOneToCome(t *testing.T) {
	d := testDirs(t)
	if err := power(d, []string{"wake", "ci", "--at", "09:30"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := mtime(t, filepath.Join(d.wake, "ci")), time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("wake at %v, want tomorrow's %v", got, want)
	}
	if err := power(d, []string{"wake", "rm", "ci"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.wake, "ci")); !os.IsNotExist(err) {
		t.Fatalf("wake still there after rm: %v", err)
	}
}

func TestKeepaliveForADuration(t *testing.T) {
	d := testDirs(t)
	if err := power(d, []string{"keepalive", "build", "--for", "2h"}, now, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := mtime(t, filepath.Join(d.lease, "build")); !got.Equal(now.Add(2 * time.Hour)) {
		t.Fatalf("lease until %v, want 2h on", got)
	}
}

func TestRefusesNamesNothingWouldRead(t *testing.T) {
	d := testDirs(t)
	for _, name := range []string{".hidden", "../escape", "a/b", ""} {
		if err := power(d, []string{"wake", name, "--in", "1h"}, now, io.Discard); err == nil {
			t.Errorf("wake %q accepted", name)
		}
	}
	if err := power(d, []string{"wake", "x", "--in", "1h", "--at", "10:00"}, now, io.Discard); err == nil {
		t.Error("accepted both --in and --at")
	}
}

// Without the control plane holding the power nothing acts on a wake, and an
// agent must not be told otherwise.
func TestAWakeWarnsWhenNothingWillActOnIt(t *testing.T) {
	d := testDirs(t)
	var out strings.Builder
	if err := power(d, []string{"wake", "ci", "--in", "1h"}, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning:") {
		t.Fatalf("output = %q, want a warning that nothing will wake the sandbox", out.String())
	}
	if err := os.WriteFile(d.controlPlane, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := power(d, []string{"wake", "ci", "--in", "1h"}, now, &out); err != nil {
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
	if err := power(d, []string{"status"}, now, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ".hidden") {
		t.Fatalf("status = %q, want the dot-named lease", out.String())
	}
}
