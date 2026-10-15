package uselines_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/uselines"
)

func TestAUseInTheSkillsShapeIsBrokenAtItsParts(t *testing.T) {
	t.Parallel()
	use := "Fix issue #43 in acme/web as a pull request from branch fix-43 into main. " +
		"Read anything in the repository; push that branch (with --force-with-lease after a rebase), open and update the pull request, and re-run its failed CI. " +
		"Never push main or any other branch, force-push without a lease, delete a branch, or read secrets."
	want := []uselines.Line{
		{Text: "Fix issue #43 in acme/web as a pull request from branch fix-43 into main."},
		{Text: "Read anything in the repository;"},
		{Text: "push that branch (with --force-with-lease after a rebase),", Hang: true},
		{Text: "open and update the pull request,", Hang: true},
		{Text: "and re-run its failed CI.", Hang: true},
		{Text: "Never push main or any other branch,"},
		{Text: "force-push without a lease,", Hang: true},
		{Text: "delete a branch,", Hang: true},
		{Text: "or read secrets.", Hang: true},
	}
	if got := uselines.Split(use); !slices.Equal(got, want) {
		t.Fatalf("Split() =\n%+v\nwant\n%+v", got, want)
	}
}

func TestAReadOnlyUseKeepsWhatItReadsWhole(t *testing.T) {
	t.Parallel()
	use := "Debug the web app in namespace web of this cluster. Read anything in namespace web; change nothing. Never exec into a pod or read secrets."
	want := []uselines.Line{
		{Text: "Debug the web app in namespace web of this cluster."},
		{Text: "Read anything in namespace web;"},
		{Text: "change nothing.", Hang: true},
		{Text: "Never exec into a pod or read secrets."},
	}
	if got := uselines.Split(use); !slices.Equal(got, want) {
		t.Fatalf("Split() =\n%+v\nwant\n%+v", got, want)
	}
}

// A use that is not in the shape is shown as it was written: no sentence
// boundary is guessed inside a parenthesis, a version, or a command.
func TestAUseOutsideTheShapeIsOneLine(t *testing.T) {
	t.Parallel()
	for _, use := range []string{
		"GitHub Copilot CLI's own use of this token: Copilot AI requests (its entitlement, policy and settings checks under api.github.com/copilot_internal. Listing models) and light read-only queries",
		"gh api GET repos/discobox-ai/discobox/issues/42 and its comments, read-only",
		"Post build notifications to the #web-deploys Slack channel (chat.postMessage)",
		"Push tag v1.2.3 to acme/web, never another tag",
	} {
		if got := uselines.Split(use); len(got) != 1 || got[0].Text != use || got[0].Hang {
			t.Errorf("Split(%q) = %+v, want it whole on one line", use, got)
		}
	}
}

// Breaking a use never changes what it says: its lines, joined, are the use.
func TestEveryWordIsKeptInOrder(t *testing.T) {
	t.Parallel()
	for _, use := range []string{
		"For the discoboxes I create: triage issues in org/repo. Read anything in the repository; add and remove issue labels (creating a missing triage label), and post and edit my triage comments. Never delete or rename a label, label another repository's issues, delete a comment, close, assign, or retitle an issue, push, or read secrets.",
		"Push branch release-1.2 to acme/web.  Read anything in the repository;\npush that branch as a fast-forward. Never push main.",
		"Read",
		"Never",
		"Read a; b, ",
		"",
	} {
		var texts []string
		for _, line := range uselines.Split(use) {
			texts = append(texts, line.Text)
		}
		if got, want := strings.Join(texts, " "), strings.Join(strings.Fields(use), " "); got != want {
			t.Errorf("Split(%q) joined = %q, want %q", use, got, want)
		}
	}
}
