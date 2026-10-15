package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/cli/internal/github"
	"github.com/discobox-ai/review/source"
	"github.com/discobox-ai/review/ui"
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

// The files changed are drawn by discobox-review's window, which shows the C0
// and C1 controls as pictures of them and lets the rest of what is invisible
// through: so everything the source hands it — the diff, a file, a commit's
// subject and author, the branches — is made safe on the way, a bidirectional
// override included, and a diff keeps its lines and its tabs.
func TestAPullRequestsFilesAreMadeSafeToDraw(t *testing.T) {
	t.Parallel()
	evil := "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07\x1b]52;c;cm0=\x07\x1b[2J\u202e\u2066"
	src := newSafeSource(evilSource{evil: evil})
	scan, err := src.Scan("", 3)
	if err != nil {
		t.Fatal(err)
	}
	diff, _ := src.CommitDiff("abc", 3)
	text, _ := src.FileContent("", "f.go")
	values := map[string]string{
		"branch": scan.Branch, "base": scan.Base.Ref, "incomplete": scan.Incomplete,
		"diff": scan.Diff, "commit diff": diff, "file": text,
		"subject": scan.Commits[0].Subject, "author": scan.Commits[0].Author, "when": scan.Commits[0].When,
	}
	for name, value := range values {
		if strings.ContainsAny(value, "\x1b\x07\u202e\u2066\r") {
			t.Errorf("the %s reaches the screen raw: %q", name, value)
		}
	}
	if !strings.HasPrefix(scan.Diff, "+\tone\n-two ") || !strings.HasPrefix(text, "\tone\ntwo ") {
		t.Errorf("the diff or the file lost its lines or its tabs: %q, %q", scan.Diff, text)
	}
	if strings.Count(text, "\n") != strings.Count("\tone\r\ntwo "+evil, "\n") {
		t.Errorf("the file's lines are not the source's: %q", text)
	}
}

// A file whose name carries an invisible character is drawn with it escaped,
// and read by the name GitHub has: the window asks by the name it was shown.
func TestAFileWithAnInvisibleNameIsStillRead(t *testing.T) {
	t.Parallel()
	name := "a\u200bb.go"
	src := newSafeSource(evilSource{name: name})
	scan, err := src.Scan("", 3)
	if err != nil {
		t.Fatal(err)
	}
	shown := terminalSafe(name)
	if !strings.Contains(scan.Diff, "+++ b/"+shown) || strings.Contains(scan.Diff, name) {
		t.Fatalf("the name is not drawn escaped: %q", scan.Diff)
	}
	text, err := src.FileContent("", shown)
	if err != nil || text != "the file" {
		t.Errorf("reading %q = %q, %v; want the file GitHub has as %q", shown, text, err, name)
	}
}

// Git quotes a path with a byte past ASCII in a header, C-style, and a diff's
// reader unquotes it: so a quoted path is unquoted and made safe here, never
// left for the window to turn back into a bidirectional override.
func TestAQuotedNameIsMadeSafe(t *testing.T) {
	t.Parallel()
	raw := "a\u202eb.go"
	quoted := `"b/a\342\200\256b.go"`
	src := newSafeSource(quotedSource{
		diff: "diff --git " + `"a/a\342\200\256b.go" ` + quoted + "\n--- " + `"a/a\342\200\256b.go"` + "\n+++ " + quoted + "\n@@ -1 +1 @@\n-x\n+y\n",
		name: raw,
	})
	scan, err := src.Scan("", 3)
	if err != nil {
		t.Fatal(err)
	}
	shown := terminalSafe(raw)
	for _, want := range []string{"diff --git a/" + shown + " b/" + shown, "--- a/" + shown, "+++ b/" + shown} {
		if !strings.Contains(scan.Diff, want+"\n") {
			t.Errorf("the header is not %q:\n%s", want, scan.Diff)
		}
	}
	if strings.Contains(scan.Diff, `"`) || strings.Contains(scan.Diff, `\342`) {
		t.Errorf("a quoted path was left for the window to unquote:\n%s", scan.Diff)
	}
	if text, err := src.FileContent("", shown); err != nil || text != "the file" {
		t.Errorf("reading %q = %q, %v; want GitHub's %q", shown, text, err, raw)
	}

	// And what the window draws of it, through its own reading of a diff.
	window := ui.New(src, ui.Options{Embedded: true, Poll: -1}).SetSize(100, 12)
	for _, msg := range scanned(t, window.Init()) {
		window, _ = window.Step(msg)
	}
	frame := window.View().Content
	if !strings.Contains(ansi.Strip(frame), shown) || strings.Contains(frame, "\u202e") {
		t.Errorf("the window draws the name as %q:\n%s", raw, ansi.Strip(frame))
	}
}

// scanned runs a window's first commands far enough for its scan to land:
// every message they send but the text field's blink, which never ends.
func scanned(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- cmd() }()
		select {
		case msg := <-done:
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, sub := range batch {
					run(sub)
				}
			} else if msg != nil {
				out = append(out, msg)
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	run(cmd)
	return out
}

// Only a file's header names paths. A changed line that begins as a header
// does — a removed SQL comment is `--- …` — is the file's text, and a quoted
// path's newline is escaped rather than made a line of the diff.
func TestOnlyAHeaderIsReadForPaths(t *testing.T) {
	t.Parallel()
	body := "@@ -1,2 +1,2 @@\n-- \"TODO\"\n++ \"a\\nb\"\n"
	diff := "diff --git a/q.sql b/q.sql\n--- a/q.sql\n+++ b/q.sql\n" + body +
		"diff --git \"a/x\\ny\" \"b/x\\ny\"\n--- \"a/x\\ny\"\n+++ \"b/x\\ny\"\n@@ -1 +1 @@\n-1\n+2\n"
	src := newSafeSource(quotedSource{diff: diff})
	scan, err := src.Scan("", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scan.Diff, body) {
		t.Errorf("a changed line was read as a header:\n%s", scan.Diff)
	}
	if !strings.Contains(scan.Diff, "+++ b/x\\ny\n") || strings.Count(scan.Diff, "\n") != strings.Count(diff, "\n") {
		t.Errorf("a quoted path's newline became a line of the diff:\n%s", scan.Diff)
	}
}

type quotedSource struct{ diff, name string }

func (q quotedSource) Scan(string, int) (source.Scan, error) {
	return source.Scan{Diff: q.diff}, nil
}

func (q quotedSource) CommitDiff(string, int) (string, error) { return q.diff, nil }

func (q quotedSource) FileContent(_, path string) (string, error) {
	if path != q.name {
		return "", errors.New("GitHub answered 404: Not Found")
	}
	return "the file", nil
}

type evilSource struct{ evil, name string }

func (s evilSource) Scan(string, int) (source.Scan, error) {
	return source.Scan{
		Branch: s.evil, Base: source.Base{Ref: s.evil}, Incomplete: s.evil,
		Commits: []source.Commit{{Hash: "abc", Subject: s.evil, Author: s.evil, When: s.evil}},
		Diff:    "+\tone\r\n-two " + s.evil + "\ndiff --git a/" + s.name + " b/" + s.name + "\n--- a/" + s.name + "\n+++ b/" + s.name + "\n",
	}, nil
}

func (s evilSource) CommitDiff(string, int) (string, error) { return "+" + s.evil, nil }

func (s evilSource) FileContent(_, path string) (string, error) {
	if s.name != "" {
		if path != s.name {
			return "", errors.New("GitHub answered 404: Not Found")
		}
		return "the file", nil
	}
	return "\tone\r\ntwo " + s.evil, nil
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
