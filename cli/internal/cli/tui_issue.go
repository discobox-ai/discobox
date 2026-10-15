package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/discobox-ai/discobox/cli/internal/github"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/review/source"
)

// Issue reads a GitHub issue or pull request for the console's issue tab, as this machine's
// `gh` is logged in (github.LocalToken), and hands it over in the console's
// own types. Everything in it was written by whoever can comment on the
// issue, so every string is made safe to draw on the way: a title or a
// comment carrying an escape sequence is shown, not obeyed — the rule the
// audit screen's records follow, less what a page is written with (pageSafe).
func (d *apiDataSource) Issue(ctx context.Context, repository string, number int, full bool) (tui.Issue, error) {
	owner, name, err := githubRepository(repository)
	if err != nil {
		return tui.Issue{}, err
	}
	issue, err := githubClient().Issue(ctx, owner, name, number, full)
	if err != nil {
		return tui.Issue{}, err
	}
	return tuiIssue(issue), nil
}

// CommentOnIssue posts the issue tab's comment.
func (d *apiDataSource) CommentOnIssue(ctx context.Context, repository string, number int, body string) error {
	owner, name, err := githubRepository(repository)
	if err != nil {
		return err
	}
	return githubClient().Comment(ctx, owner, name, number, body)
}

// SetIssueState closes or reopens the issue tab's issue.
func (d *apiDataSource) SetIssueState(ctx context.Context, repository string, number int, state, reason string) error {
	owner, name, err := githubRepository(repository)
	if err != nil {
		return err
	}
	return githubClient().SetState(ctx, owner, name, number, state, reason)
}

// MergePullRequest merges the issue tab's pull request.
func (d *apiDataSource) MergePullRequest(ctx context.Context, repository string, number int, method, sha string) error {
	owner, name, err := githubRepository(repository)
	if err != nil {
		return err
	}
	return githubClient().Merge(ctx, owner, name, number, method, sha)
}

// PullSource is the issue tab's files changed, read from GitHub between the two
// commits the tab last read. A diff is as much a stranger's text as a comment
// is, so every string the source hands the window is made safe on the way, as
// tuiIssue makes the rest of the tab: discobox-review's window draws the C0 and
// C1 controls as pictures of them, but lets the other invisible characters
// through — a bidirectional override among them, which would draw a line of a
// diff in another order than it reads to the compiler.
func (d *apiDataSource) PullSource(ctx context.Context, repository string, number int, pull tui.PullRequest) (source.Source, error) {
	owner, name, err := githubRepository(repository)
	if err != nil {
		return nil, err
	}
	if pull.BaseSHA == "" || pull.HeadSHA == "" {
		return nil, fmt.Errorf("#%d has not said what it is between", number)
	}
	return newSafeSource(githubClient().PullSource(ctx, owner, name, number, pull.BaseSHA, pull.HeadSHA, pull.Base, pull.Head)), nil
}

// safeSource is a pull request's files changed with every string made safe to
// draw: diffs and files keep their lines and tabs (codeBlock), names are one
// line (terminalSafe), and a commit's subject is written to be read (pageSafe).
// It is the boundary between GitHub's text and the console that tuiIssue is
// for the conversation.
//
// The window asks for a file by the path it read in a diff, which is the path
// as it was made safe. paths maps each one back to the path GitHub has, so a
// file whose name carries an invisible character is drawn escaped and still
// read.
//
// It reads a diff's file headers itself, which the window also does, because
// the window lets a bidirectional override through what it draws of a path.
// It is a stopgap: once the pinned discobox-review escapes format characters
// itself, this goes back to escaping nothing the window does not draw, and the
// header reading goes with it.
type safeSource struct {
	src source.Source

	mu    sync.Mutex
	paths map[string]string
}

func newSafeSource(src source.Source) *safeSource {
	return &safeSource{src: src, paths: map[string]string{}}
}

func (s *safeSource) Scan(base string, context int) (source.Scan, error) {
	scan, err := s.src.Scan(base, context)
	safe := terminalSafe
	scan.Branch, scan.Base.Ref, scan.Base.Err = safe(scan.Branch), safe(scan.Base.Ref), safe(scan.Base.Err)
	scan.Base.SHA, scan.Base.Commit = safe(scan.Base.SHA), safe(scan.Base.Commit)
	for i, c := range scan.Commits {
		scan.Commits[i] = source.Commit{Hash: safe(c.Hash), Short: safe(c.Short), Subject: pageSafe(c.Subject), Author: safe(c.Author), When: safe(c.When)}
	}
	scan.Diff, scan.Pending, scan.Incomplete = s.diff(scan.Diff), s.diff(scan.Pending), safe(scan.Incomplete)
	return scan, err
}

func (s *safeSource) CommitDiff(hash string, context int) (string, error) {
	diff, err := s.src.CommitDiff(hash, context)
	return s.diff(diff), err
}

// FileContent is the file made safe as the diff is, so the window's lines of
// it line up with the diff's: escaping adds characters to a line, never lines.
func (s *safeSource) FileContent(commit, path string) (string, error) {
	s.mu.Lock()
	if raw, ok := s.paths[path]; ok {
		path = raw
	}
	s.mu.Unlock()
	text, err := s.src.FileContent(commit, path)
	return codeBlock(text), err
}

// diff is a diff made safe, with the paths its file headers name kept for
// FileContent to find again.
//
// Git writes a path with a control character or a byte past ASCII in a header
// quoted, C-style (`+++ "b/a\342\200\256b.go"`), which is printable as it
// stands and which a diff's reader unquotes back into the raw name. So a quoted
// path is unquoted first and written back made safe, unquoted, the way the
// window will read it.
func (s *safeSource) diff(diff string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Only a file's header names paths: from its `diff --git` line to its first
	// hunk. A changed line can begin as a header does — a removed SQL comment
	// is `--- …` — and is the file's text, not a path.
	lines := strings.Split(diff, "\n")
	inHeader := false
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHeader = true
		case strings.HasPrefix(line, "@@"):
			inHeader = false
		}
		if inHeader {
			lines[i] = s.header(line)
		}
	}
	return codeBlock(strings.Join(lines, "\n"))
}

// headerPaths are the lines of a diff that name a path, by how they begin.
var headerPaths = []string{"--- ", "+++ ", "rename from ", "rename to ", "copy from ", "copy to "}

// header is one line of a diff with any path it names made safe: unchanged
// when it names none.
func (s *safeSource) header(line string) string {
	if rest, ok := strings.CutPrefix(line, "diff --git "); ok && strings.Contains(rest, `"`) {
		from, rest, ok := s.headerPath(rest)
		if !ok || !strings.HasPrefix(rest, " ") {
			return line
		}
		to, rest, ok := s.headerPath(rest[1:])
		if !ok || rest != "" {
			return line
		}
		return "diff --git " + from + " " + to
	}
	for _, lead := range headerPaths {
		if rest, ok := strings.CutPrefix(line, lead); ok {
			if path, tail, ok := s.headerPath(rest); ok && tail == "" {
				return lead + path
			}
			return line
		}
	}
	return line
}

// headerPath reads one path off the front of rest — quoted, or the rest of the
// line — and returns it made safe, with what follows it. A path is one line
// however it was quoted, so a newline or a tab in one is escaped too
// (terminalSafe): unquoted into the diff, it would be a line of its own.
func (s *safeSource) headerPath(rest string) (shown, tail string, ok bool) {
	raw := strings.TrimSuffix(rest, "\r")
	if strings.HasPrefix(raw, `"`) {
		quoted, err := strconv.QuotedPrefix(raw)
		if err != nil {
			return "", "", false
		}
		if raw, err = strconv.Unquote(quoted); err != nil {
			return "", "", false
		}
		tail = strings.TrimSuffix(rest, "\r")[len(quoted):]
	}
	shown = terminalSafe(raw)
	// The window asks for a file without its side's a/ or b/.
	for _, side := range []string{"a/", "b/"} {
		if name, ok := strings.CutPrefix(raw, side); ok && terminalSafe(name) != name {
			s.paths[terminalSafe(name)] = name
		}
	}
	return shown, tail, true
}

func githubRepository(repository string) (owner, name string, err error) {
	owner, name, ok := github.Repository(repository)
	if !ok {
		return "", "", fmt.Errorf("%s is not a GitHub repository", repository)
	}
	return owner, name, nil
}

// githubClient is one client for the life of the process, so the token `gh`
// is asked for once rather than on every read: it is this machine's, whichever
// server the discobox is on.
var githubClient = sync.OnceValue(github.New)

// tuiIssue is the issue as the tab draws it, every string made safe. What is
// written to be read on the page — a title, a description, a comment, a
// commit's message — is made safe as a page's text (pageSafe,
// pageBlock). An event's subject is not that: it is who was assigned or asked
// to review, or a commit's ID. Everything else is a name or code — a login, a branch, a path, a
// diff, a label — where an invisible character is something to be shown, and keeps the
// audit screen's rule (terminalSafe): two paths that differ are never drawn as
// one on a tab that merges.
func tuiIssue(in github.Issue) tui.Issue {
	safe, page := terminalSafe, pageSafe
	out := tui.Issue{
		Repository:     safe(in.Repository),
		Number:         in.Number,
		Title:          page(in.Title),
		State:          safe(in.State),
		StateReason:    safe(in.StateReason),
		Type:           safe(in.Type),
		Author:         safe(in.Author),
		Body:           pageBlock(in.Body),
		CreatedAt:      in.CreatedAt,
		Milestone:      page(in.Milestone),
		Locked:         in.Locked,
		Comments:       in.Comments,
		Reactions:      tui.IssueReactions(in.Reactions),
		PullRequest:    in.PullRequest,
		SubIssuesTotal: in.SubIssuesTotal,
		SubIssuesDone:  in.SubIssuesDone,
		Truncated:      in.Truncated,
	}
	for _, label := range in.Labels {
		out.Labels = append(out.Labels, tuiLabel(label))
	}
	for _, assignee := range in.Assignees {
		out.Assignees = append(out.Assignees, safe(assignee))
	}
	for _, sub := range in.SubIssues {
		out.SubIssues = append(out.SubIssues, tuiRef(sub))
	}
	for _, e := range in.Timeline {
		event := tui.IssueEvent{
			Kind:      safe(e.Kind),
			Actor:     safe(e.Actor),
			At:        e.At,
			Body:      pageBlock(e.Body),
			Reactions: tui.IssueReactions(e.Reactions),
			Subject:   safe(e.Subject),
			Message:   page(e.Message),
			Review:    safe(e.Review),
			// A rename's titles, old and new: the title's rule.
			From:        page(e.From),
			To:          page(e.To),
			StateReason: safe(e.StateReason),
		}
		if e.Label != nil {
			label := tuiLabel(*e.Label)
			event.Label = &label
		}
		if e.Source != nil {
			source := tuiRef(*e.Source)
			event.Source = &source
		}
		for _, thread := range e.Threads {
			drawn := tui.ReviewThread{Path: safe(thread.Path), Line: thread.Line, Outdated: thread.Outdated, Hunk: codeBlock(thread.Hunk)}
			for _, comment := range thread.Comments {
				drawn.Comments = append(drawn.Comments, tui.ReviewComment{
					Author: safe(comment.Author), At: comment.At, Body: pageBlock(comment.Body),
					Reactions: tui.IssueReactions(comment.Reactions),
				})
			}
			event.Threads = append(event.Threads, drawn)
		}
		out.Timeline = append(out.Timeline, event)
	}
	if in.Pull != nil {
		out.Pull = tuiPull(*in.Pull)
	}
	return out
}

// tuiPull is the rest of a pull request's page, made safe: a branch name, a
// check's name and a diff are as much a stranger's text as a comment is, and
// none of them is prose.
func tuiPull(in github.Pull) *tui.PullRequest {
	safe := terminalSafe
	out := &tui.PullRequest{
		Draft: in.Draft, Merged: in.Merged, MergedBy: safe(in.MergedBy), MergedAt: in.MergedAt,
		Head: safe(in.Head), Base: safe(in.Base), HeadSHA: safe(in.HeadSHA), BaseSHA: safe(in.BaseSHA),
		Commits: in.Commits, Additions: in.Additions, Deletions: in.Deletions, ChangedFiles: in.ChangedFiles,
		Mergeable: safe(in.Mergeable), ChecksPartial: in.ChecksPartial,
	}
	for _, method := range in.MergeMethods {
		out.MergeMethods = append(out.MergeMethods, safe(method))
	}
	for _, reviewer := range in.Reviewers {
		out.Reviewers = append(out.Reviewers, tui.PullReviewer{Name: safe(reviewer.Name), State: safe(reviewer.State)})
	}
	for _, check := range in.Checks {
		out.Checks = append(out.Checks, tui.PullCheck{Name: safe(check.Name), State: safe(check.State), URL: safe(check.URL)})
	}
	return out
}

func tuiLabel(in github.Label) tui.IssueLabel {
	// A label is picked by its name: two that differ are not drawn as one.
	return tui.IssueLabel{Name: terminalSafe(in.Name), Color: terminalSafe(in.Color)}
}

func tuiRef(in github.Ref) tui.IssueRef {
	return tui.IssueRef{
		Repository: terminalSafe(in.Repository), Number: in.Number, Title: pageSafe(in.Title),
		State: terminalSafe(in.State), PullRequest: in.PullRequest, Merged: in.Merged,
	}
}

// pageBlock is a comment or a description: a page's text (pageSafe) with its
// lines kept. GitHub stores the line breaks a browser sent, which are \r\n.
//
// It is markdown, and this is not all that stands between it and the screen:
// the renderer decodes character references, so `&#x1b;` is printable here
// and an escape once drawn. The tab neutralizes those where it renders
// (tui's markdownSource).
func pageBlock(body string) string {
	return escapeNonPrinting(dropBreakHints(strings.ReplaceAll(body, "\r\n", "\n")), func(r rune) bool {
		return isLayoutRune(r) || isJoiner(r)
	})
}

// codeBlock is a diff, or the lines of one a review thread is about: lines
// kept, and every invisible character in it shown.
func codeBlock(patch string) string {
	return terminalSafeMultiline(strings.ReplaceAll(patch, "\r\n", "\n"))
}

// pageSafe is terminalSafe for text written to be read on a web page, which carries
// invisible characters that are part of how it reads there and nothing a
// terminal acts on. The audit screen escapes them because a record is shown
// exactly as it was written; here that would put `\u200b` after every slash of
// a path in a Copilot review, and split an emoji made of several in two.
//
// The hints at where a line may break are dropped: the window wraps the text
// itself. The joiners are kept, since an emoji sequence and several scripts are
// not the same text without them, and so are a page's other spaces. Everything else invisible — the controls,
// ESC, the bidirectional overrides and marks — is still escaped.
func pageSafe(value string) string {
	return escapeNonPrinting(dropBreakHints(value), isJoiner)
}

// dropBreakHints removes the zero-width space, the soft hyphen, the word
// joiner and the byte order mark, which say where a line may or may not break
// and show nothing.
func dropBreakHints(value string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u00ad', '\u2060', '\ufeff':
			return -1
		}
		return r
	}, value)
}

// isJoiner reports whether r is one of the invisible characters a page's text
// is written with and a terminal draws as it would any other: the zero-width
// joiner and non-joiner, and the spaces that are not the ASCII one (a
// no-break space is in every other comment pasted from a page).
func isJoiner(r rune) bool { return r == '\u200d' || r == '\u200c' || unicode.Is(unicode.Zs, r) }
