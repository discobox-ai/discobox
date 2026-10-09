package tui

import (
	"strings"
	"testing"
)

// readySandboxes is the fixture with its first discobox holding committed work
// that no apply has landed: a clean tree whose head has moved off the commit it
// was spawned from, which is the state the list spells "ready".
func readySandboxes() []Sandbox {
	all := testSandboxes()
	all[0].Dirty = false
	all[0].Git = GitState{Known: true, Branch: "main", Commit: "b7d0f11"}
	all[0].Diff = DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7}
	return all
}

// Work that is ready to apply gets no band: the header and the list already say
// `ready`, and the workspace's one exception bar is for things that need
// somebody.
func TestReadyWorkRaisesNoBand(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(readySandboxes()...)
	_, m, _ := openWorkspace(t, ds, "enter")

	// The fixture is the state the band was for: committed work no apply has
	// landed, which the header still says.
	if !m.currentBox().ahead() {
		t.Fatalf("the fixture is not ready: %+v", m.currentBox().Git)
	}
	if !strings.Contains(plainFrame(m), "ready") {
		t.Fatalf("the header does not say ready:\n%s", plainFrame(m))
	}
	if m.bannerShowing() != bannerNone || m.bannerTop() != 0 {
		t.Fatalf("a band is up for ready work:\n%s", plainFrame(m))
	}
	if strings.Contains(plainFrame(m), "ready to apply") {
		t.Fatalf("the offer is drawn:\n%s", plainFrame(m))
	}
}

// The key is deliberate — a leader chord — so it runs the apply, the same as it
// does from the list.
func TestTheApplyKeyGoesStraightToTheApply(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(readySandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(applyKey)
	d.wait("the command", func() bool { return m.overlay != nil })

	if m.dialog != nil {
		t.Fatalf("the key asked a question: %q", dialogText(m))
	}
	if m.overlay.action != InteractApply {
		t.Fatalf("overlay = %s, want apply", m.overlay.action)
	}
	if len(ds.opens) != 1 || !strings.HasPrefix(ds.opens[0], "apply sbx_one ") {
		t.Fatalf("opens = %v, want apply on the discobox the workspace shows", ds.opens)
	}
}

// An apply can succeed with a source that never had a local directory: one
// cloned from a remote that the discobox committed nothing to needs nowhere to
// land. The success dialog has to say that where it says a repository for every
// other source, rather than drawing a label with nothing after it — an empty
// value still takes a row (wrap returns one empty line for it).
func TestTheSuccessDialogSaysWhyASourceHasNoRepository(t *testing.T) {
	sections := appliedSourceSections(ApplyResult{Sources: []AppliedSource{
		{
			Slug:       "primary",
			Status:     "applied",
			Repository: "/home/ada/src/disco2",
			Branch:     "main",
			Commits:    []AppliedCommit{{Commit: "11aa22bb33cc44dd", Subject: "Fix the parser"}},
		},
		{
			Slug:            "hooks",
			Status:          "up-to-date",
			RepositoryError: `source "hooks" has no local directory recorded; pass --dir hooks=PATH`,
		},
	}})

	if len(sections) != 2 {
		t.Fatalf("got %d sections, want one per source: %+v", len(sections), sections)
	}
	if got := sections[0].fields[0]; got.label != "repository" || got.value != "/home/ada/src/disco2" {
		t.Fatalf("applied source names %q = %q, want its repository", got.label, got.value)
	}
	unplaced := sections[1]
	if len(unplaced.fields) != 1 {
		t.Fatalf("source with no repository has %d fields, want only the repository row: %+v", len(unplaced.fields), unplaced.fields)
	}
	if got := unplaced.fields[0]; got.label != "repository" || !strings.HasPrefix(got.value, "none — ") {
		t.Fatalf("repository row = %q: %q, want it to say there is none and why", got.label, got.value)
	}
	if !strings.Contains(unplaced.fields[0].value, "pass --dir hooks=PATH") {
		t.Fatalf("repository row drops the reason: %q", unplaced.fields[0].value)
	}
}

// A report that says neither where a source landed nor why it did not draws no
// repository row at all, rather than an empty one.
func TestTheSuccessDialogDrawsNoEmptyRepositoryRow(t *testing.T) {
	sections := appliedSourceSections(ApplyResult{Sources: []AppliedSource{{Slug: "primary", Status: "up-to-date"}}})

	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1: %+v", len(sections), sections)
	}
	for _, f := range sections[0].fields {
		if strings.TrimSpace(f.value) == "" {
			t.Fatalf("field %q drawn with no value: %+v", f.label, sections[0].fields)
		}
	}
}
