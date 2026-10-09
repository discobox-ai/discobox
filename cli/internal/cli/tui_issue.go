package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/cli/internal/github"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// Issue reads a GitHub issue for the console's issue tab, as this machine's
// `gh` is logged in (github.LocalToken), and hands it over in the console's
// own types. Everything in it was written by whoever can comment on the
// issue, so every string is made safe to draw on the way: a title or a
// comment carrying an escape sequence is shown, not obeyed — the same rule
// the audit screen's records follow (terminalSafe).
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

// tuiIssue is the issue as the tab draws it, every string made safe.
func tuiIssue(in github.Issue) tui.Issue {
	safe := terminalSafe
	out := tui.Issue{
		Repository:    safe(in.Repository),
		Number:        in.Number,
		Title:         safe(in.Title),
		State:         safe(in.State),
		StateReason:   safe(in.StateReason),
		Type:          safe(in.Type),
		Author:        safe(in.Author),
		Body:          safeBlock(in.Body),
		CreatedAt:     in.CreatedAt,
		Milestone:     safe(in.Milestone),
		Locked:        in.Locked,
		Comments:      in.Comments,
		Reactions:     tui.IssueReactions(in.Reactions),
		PullRequest:   in.PullRequest,
		SubIssuesDone: in.SubIssuesDone,
		Truncated:     in.Truncated,
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
			Kind:        safe(e.Kind),
			Actor:       safe(e.Actor),
			At:          e.At,
			Body:        safeBlock(e.Body),
			Reactions:   tui.IssueReactions(e.Reactions),
			Subject:     safe(e.Subject),
			From:        safe(e.From),
			To:          safe(e.To),
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
		out.Timeline = append(out.Timeline, event)
	}
	return out
}

func tuiLabel(in github.Label) tui.IssueLabel {
	return tui.IssueLabel{Name: terminalSafe(in.Name), Color: terminalSafe(in.Color)}
}

func tuiRef(in github.Ref) tui.IssueRef {
	return tui.IssueRef{
		Repository: terminalSafe(in.Repository), Number: in.Number, Title: terminalSafe(in.Title),
		State: terminalSafe(in.State), PullRequest: in.PullRequest, Merged: in.Merged,
	}
}

// safeBlock is a comment or a description: lines kept, everything else a
// terminal would act on escaped. GitHub stores the line breaks a browser sent,
// which are \r\n.
func safeBlock(body string) string {
	return terminalSafeMultiline(strings.ReplaceAll(body, "\r\n", "\n"))
}
