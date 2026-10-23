package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/discobox-ai/discobox/cli/internal/github"
	"github.com/discobox-ai/discobox/cli/internal/tui"
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
		Head: safe(in.Head), Base: safe(in.Base), HeadSHA: safe(in.HeadSHA), BaseSHA: safe(in.BaseSHA), HeadRef: safe(in.HeadRef),
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
