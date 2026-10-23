package cli

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/github"
)

// Anyone who can comment on an issue writes its text, so none of it reaches
// the console's screen as something a terminal would act on.
func TestAnIssueIsMadeSafeToDraw(t *testing.T) {
	t.Parallel()
	evil := "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07\x1b[2J\u202e"
	issue := tuiIssue(github.Issue{
		Title:  "title " + evil,
		Body:   "line one\r\nline two " + evil,
		Labels: []github.Label{{Name: evil, Color: "ffffff"}},
		Timeline: []github.Event{
			{Kind: "commented", Actor: evil, Body: "comment\r\n" + evil},
			{Kind: "cross-referenced", Source: &github.Ref{Title: evil}},
		},
	})
	for name, value := range map[string]string{
		"title": issue.Title, "body": issue.Body, "label": issue.Labels[0].Name,
		"actor": issue.Timeline[0].Actor, "comment": issue.Timeline[0].Body,
		"reference": issue.Timeline[1].Source.Title,
	} {
		if strings.ContainsAny(value, "\x1b\x07\u202e\r") {
			t.Errorf("the %s reaches the screen raw: %q", name, value)
		}
	}
	if !strings.Contains(issue.Body, "line one\nline two") {
		t.Errorf("the body lost its lines: %q", issue.Body)
	}
}

// A pull request's own text is a stranger's too: a branch name, a check's name
// and its URL, a review thread.
func TestAPullRequestIsMadeSafeToDraw(t *testing.T) {
	t.Parallel()
	evil := "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07\x1b[2J\u202e"
	issue := tuiIssue(github.Issue{
		PullRequest: true,
		Pull: &github.Pull{
			Head: evil, Base: evil, MergedBy: evil, Mergeable: evil,
			Reviewers: []github.Reviewer{{Name: evil, State: evil}},
			Checks:    []github.Check{{Name: evil, State: evil, URL: "https://ci.example/\x1b[2J"}},
		},
		Timeline: []github.Event{{
			Kind: "reviewed", Review: evil, Message: evil,
			Threads: []github.Thread{{Path: evil, Hunk: "@@\n+" + evil, Comments: []github.ThreadComment{{Author: evil, Body: evil}}}},
		}},
	})
	pull, review := issue.Pull, issue.Timeline[0]
	if pull == nil || len(review.Threads) != 1 {
		t.Fatalf("the pull request did not come through: %+v", issue)
	}
	thread := review.Threads[0]
	for name, value := range map[string]string{
		"head": pull.Head, "base": pull.Base, "merger": pull.MergedBy, "mergeable": pull.Mergeable,
		"reviewer": pull.Reviewers[0].Name, "reviewer state": pull.Reviewers[0].State,
		"check": pull.Checks[0].Name, "check state": pull.Checks[0].State, "check URL": pull.Checks[0].URL,
		"review": review.Review, "message": review.Message,
		"thread path": thread.Path, "hunk": thread.Hunk,
		"thread author": thread.Comments[0].Author, "thread comment": thread.Comments[0].Body,
	} {
		if strings.ContainsAny(value, "\x1b\x07\u202e\r") {
			t.Errorf("the %s reaches the screen raw: %q", name, value)
		}
	}
}

// A page's text carries invisible characters a terminal has nothing to do
// with: the hints at where a line may break are dropped rather than shown as
// escapes, and the joiners an emoji sequence is made with are kept. That is
// for what is written to be read. A path, a branch and a diff are code, where
// an invisible character is shown: two that differ are not drawn as one.
func TestAnIssueKeepsWhatAPageIsWrittenWith(t *testing.T) {
	t.Parallel()
	path := "`server/\u200binternal/\u200bpools/\u200bDESIGN.md`"
	family := "\U0001F468\u200d\U0001F469\u200d\U0001F467"
	issue := tuiIssue(github.Issue{
		Title:  "soft\u00adhyphen " + family,
		Labels: []github.Label{{Name: "b\u200bug"}},
		Body:   "\ufeff" + path + " " + family + " word\u2060joiner \u202e",
		Pull: &github.Pull{
			Head: "fix\u200b/x",
		},
		Timeline: []github.Event{{
			Kind: "reviewed", Body: path, Subject: "bo\u200bb", From: "old\u200b title", To: "new\u200b title",
			Threads: []github.Thread{{Path: "a/\u200bb.go", Hunk: "+x\u200by", Comments: []github.ThreadComment{{Body: path}}}},
		}},
	})
	want := "`server/internal/pools/DESIGN.md`"
	for name, value := range map[string]string{
		"body": issue.Body, "review": issue.Timeline[0].Body,
		"thread comment": issue.Timeline[0].Threads[0].Comments[0].Body,
	} {
		if !strings.Contains(value, want) {
			t.Errorf("the %s shows its break hints: %q", name, value)
		}
	}
	if renamed := issue.Timeline[0]; renamed.From != "old title" || renamed.To != "new title" {
		t.Errorf("a rename's titles = %q, %q; want them as the title is", renamed.From, renamed.To)
	}
	if issue.Title != "softhyphen "+family {
		t.Errorf("title = %q", issue.Title)
	}
	if !strings.Contains(issue.Body, family) || !strings.Contains(issue.Body, "wordjoiner") {
		t.Errorf("body = %q", issue.Body)
	}
	if !strings.Contains(issue.Body, `\u202e`) {
		t.Errorf("a bidirectional override is no longer escaped: %q", issue.Body)
	}
	thread := issue.Timeline[0].Threads[0]
	for name, got := range map[string][2]string{
		"branch":      {issue.Pull.Head, `fix\u200b/x`},
		"thread path": {thread.Path, `a/\u200bb.go`},
		"thread hunk": {thread.Hunk, `+x\u200by`},
		"subject":     {issue.Timeline[0].Subject, `bo\u200bb`},
		"label":       {issue.Labels[0].Name, `b\u200bug`},
	} {
		if got[0] != got[1] {
			t.Errorf("the %s hides what is invisible in it: %q, want %q", name, got[0], got[1])
		}
	}
}
