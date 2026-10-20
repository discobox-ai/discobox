package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/internal/privatefs"
)

// The draft, the view and the last source are one of each on the machine, and
// saving one leaves the others as they were.
func TestConsoleStateRoundTrips(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if got := loadConsoleState(); got.Draft != "" || !got.View.listView().IsZero() || got.LastSource != "" {
		t.Fatalf("a fresh state dir has state: %+v", got)
	}
	view := tui.ListView{Server: "beta", FolderKey: "k", FolderLabel: "/src/foo @ main", FolderSource: "/src/foo", FolderLocal: true, Tags: []string{"ticket=ENG-12", "wip"}}
	for _, save := range []func() error{
		func() error { return savePromptDraft("finish the reaper\nand test it") },
		func() error { return saveConsoleView(view) },
		func() error { return saveLastSource("https://github.com/acme/widgets") },
	} {
		if err := save(); err != nil {
			t.Fatal(err)
		}
	}
	got := loadConsoleState()
	if got.Draft != "finish the reaper\nand test it" {
		t.Errorf("draft = %q", got.Draft)
	}
	if !got.View.listView().Equal(view) {
		t.Errorf("view = %+v, want %+v", got.View.listView(), view)
	}
	if got.LastSource != "https://github.com/acme/widgets" {
		t.Errorf("last source = %q", got.LastSource)
	}

	// Clearing the draft and the view drops only them.
	if err := savePromptDraft(""); err != nil {
		t.Fatal(err)
	}
	if err := saveConsoleView(tui.ListView{}); err != nil {
		t.Fatal(err)
	}
	got = loadConsoleState()
	if got.Draft != "" || !got.View.listView().IsZero() || got.LastSource != "https://github.com/acme/widgets" {
		t.Fatalf("state = %+v, want only the last source left", got)
	}
}

func TestConsoleStateLivesUnderXDGStateHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := savePromptDraft("a prompt"); err != nil {
		t.Fatal(err)
	}
	// A prompt is the user's own writing, so the file it sits in is theirs.
	// Asserted through privatefs.Check, because Windows has no Unix mode to
	// report: writeStateFile restricts the file by ACL there.
	if err := privatefs.Check(filepath.Join(state, "discobox", "cli", "console.json")); err != nil {
		t.Fatal(err)
	}
}

// A broken file opens the window empty, which is where it opens without one,
// and the next save puts it back in a state that reads.
func TestACorruptConsoleStateIsIgnored(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(cliStateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consoleStatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadConsoleState(); got.Draft != "" {
		t.Fatalf("draft = %q, want empty", got.Draft)
	}
	if err := savePromptDraft("a prompt"); err != nil {
		t.Fatal(err)
	}
	if got := loadConsoleState().Draft; got != "a prompt" {
		t.Fatalf("draft = %q, want it saved over the corrupt file", got)
	}
}

// A composer holds whatever is pasted into it, and a state file is not where a
// megabyte of pasted log belongs.
func TestALongPromptIsCutToTheLimit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Multi-byte, and one ASCII byte ahead of it, so the cut lands inside a
	// character.
	if err := savePromptDraft("a" + strings.Repeat("é", promptDraftMax)); err != nil {
		t.Fatal(err)
	}
	draft := loadConsoleState().Draft
	if len(draft) > promptDraftMax || len(draft) < promptDraftMax-4 {
		t.Fatalf("draft is %d bytes, want it cut to about %d", len(draft), promptDraftMax)
	}
	if !utf8.ValidString(draft) {
		t.Error("the cut left half a character behind")
	}
}

// A save waits for another process's read-modify-write to finish rather than
// writing over what that process is about to write.
func TestASaveWaitsForAnotherProcesssLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := ensureStateDir(cliStateDir()); err != nil {
		t.Fatal(err)
	}
	held, err := filelock.TryAcquire(filepath.Join(cliStateDir(), consoleStateLockName))
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = held.Release()
		close(released)
	}()
	if err := savePromptDraft("after the other"); err != nil {
		t.Fatalf("save = %v, want it to wait for the lock", err)
	}
	<-released
	if got := loadConsoleState().Draft; got != "after the other" {
		t.Fatalf("draft = %q", got)
	}
}
