package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPromptDraftRoundTrips(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if got := promptDraftFor("/src/disco2"); got != "" {
		t.Fatalf("a fresh state dir has a draft: %q", got)
	}
	if err := savePromptDraft("/src/disco2", "finish the reaper\nand test it"); err != nil {
		t.Fatalf("savePromptDraft: %v", err)
	}
	if err := savePromptDraft("/src/other", "something else"); err != nil {
		t.Fatalf("savePromptDraft: %v", err)
	}
	// Every folder keeps its own: a prompt written in one checkout has no
	// business coming back in another.
	if got, want := promptDraftFor("/src/disco2"), "finish the reaper\nand test it"; got != want {
		t.Fatalf("draft = %q, want %q", got, want)
	}
	if got := promptDraftFor("/src/other"); got != "something else" {
		t.Fatalf("draft = %q, want the other folder's", got)
	}
	if got := promptDraftFor("/src/never-visited"); got != "" {
		t.Fatalf("draft for an unknown folder = %q, want empty", got)
	}

	// An emptied prompt drops the entry rather than storing nothing under it.
	if err := savePromptDraft("/src/disco2", ""); err != nil {
		t.Fatalf("savePromptDraft: %v", err)
	}
	if got := promptDraftFor("/src/disco2"); got != "" {
		t.Fatalf("draft = %q, want it dropped", got)
	}
	if _, ok := loadPromptDrafts()["/src/disco2"]; ok {
		t.Error("an emptied draft should leave no entry behind")
	}
	if got := promptDraftFor("/src/other"); got != "something else" {
		t.Fatalf("dropping one folder's draft took another's: %q", got)
	}
}

func TestPromptDraftsLiveUnderXDGStateHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := savePromptDraft("/src/disco2", "a prompt"); err != nil {
		t.Fatalf("savePromptDraft: %v", err)
	}

	path := filepath.Join(state, "discobox", "cli", "prompt-drafts.json")
	// A prompt is the user's own writing, so the file it sits in is theirs.
	// Asserted through the helper each platform defines, because Windows has no
	// Unix mode to report: writeStateFile restricts the file by ACL there, and
	// Stat still says -rw-rw-rw- whatever the ACL says.
	assertPrivateToUser(t, path)
}

// A broken or missing file means the window opens empty, which is where it used
// to open every time. It is never a reason to fail.
func TestACorruptDraftFileIsIgnored(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "discobox", "cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt-drafts.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := promptDraftFor("/src/disco2"); got != "" {
		t.Fatalf("draft = %q, want empty", got)
	}
	// And writing over it puts the file back in a state that reads.
	if err := savePromptDraft("/src/disco2", "a prompt"); err != nil {
		t.Fatalf("savePromptDraft: %v", err)
	}
	if got := promptDraftFor("/src/disco2"); got != "a prompt" {
		t.Fatalf("draft = %q, want it saved over the corrupt file", got)
	}
}

// The file is bounded, so a machine that has been through a lot of checkouts
// does not carry an entry for each of them forever. The oldest go first.
func TestPromptDraftsAreTrimmed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	drafts := map[string]promptDraft{}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	for i := range promptDraftLimit + 10 {
		drafts["/src/"+strconv.Itoa(i)] = promptDraft{Text: "x", At: now.Add(time.Duration(i) * time.Minute)}
	}
	trimPromptDrafts(drafts)

	if len(drafts) != promptDraftLimit {
		t.Fatalf("%d drafts kept, want %d", len(drafts), promptDraftLimit)
	}
	if _, ok := drafts["/src/0"]; ok {
		t.Error("the oldest draft should be the first to go")
	}
	if _, ok := drafts["/src/"+strconv.Itoa(promptDraftLimit+9)]; !ok {
		t.Error("the newest draft should be kept")
	}
}

// A composer holds whatever is pasted into it, and a state file is not where a
// megabyte of pasted log belongs.
func TestALongPromptIsCutToTheLimit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Multi-byte, so a cut in the wrong place would leave half a character.
	if err := savePromptDraft("/src/disco2", strings.Repeat("é", promptDraftMax)); err != nil {
		t.Fatalf("savePromptDraft: %v", err)
	}
	draft := promptDraftFor("/src/disco2")
	if len(draft) > promptDraftMax || len(draft) < promptDraftMax-4 {
		t.Fatalf("draft is %d bytes, want it cut to about %d", len(draft), promptDraftMax)
	}
	if !utf8.ValidString(draft) {
		t.Error("the cut left half a character behind")
	}
}
