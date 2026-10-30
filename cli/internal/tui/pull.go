package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// What a pull request's page has that an issue's does not, drawn into the
// issue tab (issue.go): the badge and the branches under the title, the
// reviewers, the size of the change, the checks and whether it can merge among
// the facts; the pushes, the reviews and their threads on the diff in the
// timeline; and the files changed, as a page of its own behind f.

const (
	// threadHunkLines is how much of the diff a review thread is drawn under:
	// the lines leading up to the one it is about, as the page shows them.
	threadHunkLines = 4
	// threadIndent sets a thread in from the review it belongs to.
	threadIndent = 2
	// pullMergeKey merges a pull request, behind a question.
	pullMergeKey = "m"
	// mergeBandHint is what the ready band says beside that key, at its right
	// end: the two together are a control (viewPaneBox marks them).
	mergeBandHint = " merges it "
	// pullSettleAfter and pullSettleTries are how soon, and how many times, a
	// pull request GitHub has yet to say can merge is read again.
	pullSettleAfter = 3 * time.Second
	pullSettleTries = 3
	// diffTab is how wide a tab in a diff is drawn. A real one would move the
	// cursor by a rule the frame's arithmetic does not know.
	diffTab = "    "
)

// pullState is the badge beside a pull request's title: the dot the list draws
// a discobox's state with, filled while there is something to do about it,
// and the word.
func pullState(st *styles, issue Issue) string {
	switch pull := issue.Pull; {
	case pull.Merged:
		return st.info.Render("● Merged")
	case issue.State == "closed":
		return st.stateOff.Render("○ Closed")
	case pull.Draft:
		return st.stateOff.Render("○ Draft")
	}
	return st.stateRun.Render("● Open")
}

// pullByline is the line under the title, which says what the page says: who
// wants what merged into where, or who merged it.
func pullByline(st *styles, issue Issue, now time.Time) string {
	pull := issue.Pull
	who, verb, when := issue.Author, "wants to merge", issue.CreatedAt
	if pull.Merged {
		who, verb, when = pull.MergedBy, "merged", pull.MergedAt
	}
	return pullState(st, issue) + "  " + st.emphasis.Render(who) + " " +
		st.dimText.Render(verb+" "+plural(pull.Commits, "commit", "commits")+" into ") + pull.Base +
		st.dimText.Render(" from ") + pull.Head +
		st.dimText.Render(" · "+ago(when, now)+" · "+plural(issue.Comments, "comment", "comments"))
}

// reviewerRows is the page's Reviewers list, a reviewer a row.
func reviewerRows(st *styles, reviewers []PullReviewer, width int) []string {
	rows := make([]string, 0, len(reviewers))
	for _, reviewer := range reviewers {
		glyph, said := st.dimText.Render("○"), "review requested"
		switch reviewer.State {
		case "approved":
			glyph, said = st.stateRun.Render("●"), "approved"
		case "changes_requested":
			glyph, said = st.stateErr.Render("●"), "requested changes"
		case "commented":
			glyph, said = st.stateOff.Render("○"), "commented"
		case "dismissed":
			glyph, said = st.stateOff.Render("○"), "review dismissed"
		}
		rows = append(rows, truncate(glyph+" "+reviewer.Name+" "+st.dimText.Render(said), width))
	}
	return rows
}

// pullChanges is the size of the change: the counts over the page's tabs.
func pullChanges(st *styles, pull PullRequest) string {
	return plural(pull.ChangedFiles, "file", "files") + st.dimText.Render(" · ") +
		plural(pull.Commits, "commit", "commits") + st.dimText.Render(" · ") + plusMinus(st, pull.Additions, pull.Deletions)
}

// plusMinus is a count of lines added and removed, in the colors the header's
// own diffstat is drawn in.
func plusMinus(st *styles, additions, deletions int) string {
	return st.add.Render("+"+itoa(additions)) + " " + st.del.Render("−"+itoa(deletions))
}

// checkRows is the head commit's checks: how they stand, in one row, and then
// every one that has not passed, since those are the ones to go and read. Each
// is a link to its own page, where it has one. A list that is not all of them
// says so first, so a row of passes is not read as the whole.
func checkRows(st *styles, checks []PullCheck, partial bool, width int) []string {
	if len(checks) == 0 {
		if partial {
			return []string{st.statusWA.Render(truncate("could not be read — o opens them on GitHub", width))}
		}
		return nil
	}
	var failing, pending, passed int
	for _, check := range checks {
		switch check.State {
		case "failure", "canceled":
			failing++
		case "pending":
			pending++
		default:
			passed++
		}
	}
	var parts []string
	if failing > 0 {
		parts = append(parts, st.statusER.Render(itoa(failing)+" failing"))
	}
	if pending > 0 {
		parts = append(parts, st.statusWA.Render(itoa(pending)+" pending"))
	}
	if passed > 0 {
		parts = append(parts, st.statusOK.Render(itoa(passed)+" passed"))
	}
	if partial {
		parts = append(parts, st.statusWA.Render("not all could be read"))
	}
	rows := []string{truncate(strings.Join(parts, st.dimText.Render(" · ")), width)}
	for _, check := range checks {
		var glyph string
		switch check.State {
		case "failure":
			glyph = st.stateErr.Render("●")
		case "canceled":
			glyph = st.stateOff.Render("○")
		case "pending":
			glyph = st.stateBusy.Render("●")
		default:
			continue
		}
		name := truncate(check.Name, max(width-2, 1))
		if webLink(check.URL) {
			name = hyperlink(check.URL, name)
		}
		rows = append(rows, glyph+" "+name)
	}
	return rows
}

// pullReady reports whether a pull request is ready to merge: open, not a
// draft, and what GitHub calls clean — no conflicts, its checks passed, and
// every review and rule the branch requires satisfied.
func pullReady(issue Issue) bool {
	pull := issue.Pull
	return pull != nil && issue.State == "open" && !pull.Draft && (pull.Mergeable == "clean" || pull.Mergeable == "has_hooks")
}

// mergeBanner is the page's merge box, as one bar across the top of an open
// pull request: whether it can merge now, in GitHub's own terms, and when it
// cannot, the reason. It is the first thing under the title because it is the
// question the tab is opened to answer; ready is a band across the tab, so it
// is seen without being read.
func mergeBanner(st *styles, issue Issue, width int) string {
	pull := issue.Pull
	if pull == nil || issue.State != "open" {
		return ""
	}
	if pullReady(issue) {
		// A band the width of the tab, in the field a group's title bar is
		// drawn on, with the three things the window's bands carry: the mark
		// that catches the eye, the sentence, and the key that acts.
		left := " " + st.statusOK.Bold(true).Render("● Ready to merge") + st.attentionText.Render(" into "+pull.Base)
		right := st.key.Render(pullMergeKey) + st.attentionHint.Render(mergeBandHint)
		return highlight(st, padANSI(spread(truncate(left, max(width-lipgloss.Width(right)-1, 1)), right, width), width), colInactive)
	}
	paint, glyph := st.statusWA, "●"
	var said string
	switch {
	case pull.Draft || pull.Mergeable == "draft":
		paint, glyph, said = st.dimText, "○", "a draft, to be marked ready for review first"
	case pull.Mergeable == "dirty":
		paint, glyph, said = st.statusER, "●", "conflicts with "+pull.Base+" must be resolved"
	case pull.Mergeable == "behind":
		said = "out of date with " + pull.Base
	case pull.Mergeable == "blocked":
		said = "a required review or check is missing"
	case pull.Mergeable == "unstable":
		said = "its checks have not passed"
	default:
		return st.dimText.Render(truncate("○ GitHub has not said yet whether this can merge — r asks again", width))
	}
	return paint.Bold(true).Render(truncate(glyph+" Not ready to merge — "+said, width))
}

// mergeBandKeyFits reports whether a ready band that wide has its key on it:
// narrower, the band gives the key up for the sentence (spread), and there is
// nothing there to mark or light.
func mergeBandKeyFits(width int) bool {
	return width >= lipgloss.Width(pullMergeKey+mergeBandHint)+2
}

// pushedNotice says the branch was pushed to since the tab last showed the
// person its head: what was read before then is not what would be merged.
func pushedNotice(was, now string) string {
	return "commits were pushed since " + shortCommit(was) + " was read here: the head is now " + shortCommit(now)
}

// pullSettle reads a pull request again soon when GitHub had yet to say
// whether it can merge: it works that out when first asked, and answers
// "unknown" until it has. A few times only — the minute's tick is there after.
func (m *Model) pullSettle(p *pane) tea.Cmd {
	v := p.issue
	pull := v.issue.Pull
	if v.issue.State != "open" || (pull.Mergeable != "unknown" && pull.Mergeable != "") || v.settles >= pullSettleTries {
		return nil
	}
	v.settles++
	id := p.id
	return tick(pullSettleAfter, func(time.Time) tea.Msg { return pullSettleMsg{id: id} })
}

// pullSettleMsg is time to ask again whether a pull request can merge.
type pullSettleMsg struct{ id int }

func (m *Model) pullSettled(msg pullSettleMsg) tea.Cmd {
	p, v := m.issueOf(msg.id)
	if v == nil || v.loading || v.posting || v.changing {
		return nil
	}
	return m.loadIssue(p, false)
}

// pullMergedMsg is a merge done, or why it was not.
type pullMergedMsg struct {
	id     int
	method string
	err    error
}

// mergeMethods are the ways to merge as the page offers them, each on the key
// that picks it. None of them is the key that asks (pullMergeKey): a second
// press of that one, or its repeat, would otherwise be an answer.
var mergeMethods = []action{
	{key: "squash", press: "s", label: "squash and merge", detail: "its commits become one commit on the base branch"},
	{key: "merge", press: "c", label: "create a merge commit", detail: "every commit is kept, joined by a merge commit"},
	{key: "rebase", press: "r", label: "rebase and merge", detail: "every commit is replayed onto the base branch"},
}

// askMerge is m on a pull request: which way to merge it, of the ways the
// repository allows. Asked, like a close — it is public and it is not undone
// — and refused here only where GitHub could not do it either: a merge GitHub
// would refuse for a rule is GitHub's to refuse, with its own reason.
func (m *Model) askMerge(p *pane) tea.Cmd {
	v := p.issue
	switch {
	case v.issue == nil:
		return status("the pull request has not been read yet")
	case v.issue.Pull == nil || v.changing:
		return nil
	case v.issue.Pull.Merged:
		return status("%s is already merged", v.label())
	case v.issue.State != "open":
		return status("%s is closed — x reopens it", v.label())
	case v.issue.Pull.Draft:
		return status("%s is a draft — it is marked ready for review on GitHub first", v.label())
	case v.issue.Pull.Mergeable == "dirty":
		return status("%s conflicts with %s — the conflicts are resolved first", v.label(), v.issue.Pull.Base)
	}
	pull := v.issue.Pull
	items := make([]action, len(mergeMethods))
	for i, item := range mergeMethods {
		// A repository that would not say which it allows is asked by trying.
		item.enabled = len(pull.MergeMethods) == 0 || slices.Contains(pull.MergeMethods, item.key)
		if !item.enabled {
			item.why = "not allowed in this repository"
		}
		items[i] = item
	}
	body := v.issue.Title
	if !pullReady(*v.issue) {
		body += "\n\n" + ansi.Strip(mergeBanner(m.st, *v.issue, 200))
	}
	// The commit that will be merged is named: the tab reads itself again on
	// its own clock, so the head here may be newer than the diff that was read.
	sha := pull.HeadSHA
	body += "\n\nat " + shortCommit(sha)
	if v.pushed != "" {
		body += " — " + pushedNotice(v.pushed, sha)
	}
	d := actionsDialog("Merge #"+itoa(v.number)+" into "+pull.Base, body, items, func(method string) tea.Cmd {
		return m.mergePull(p, method, sha)
	})
	d.over = p
	m.dialog = d
	return nil
}

// mergePull sends the merge.
func (m *Model) mergePull(p *pane, method, sha string) tea.Cmd {
	v := p.issue
	v.changing = true
	ctx, ds, id, repository, number := m.ctx, m.ds, p.id, v.repository, v.number
	merge := func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, issueTimeout)
		defer cancel()
		return pullMergedMsg{id: id, method: method, err: ds.MergePullRequest(ctx, repository, number, method, sha)}
	}
	return tea.Batch(merge, status("merging #%d…", number))
}

// pullMerged reads the pull request again once it is merged, so the badge and
// the timeline say so. A refusal says GitHub's reason, and nothing has changed.
func (m *Model) pullMerged(msg pullMergedMsg) tea.Cmd {
	p, v := m.issueOf(msg.id)
	if v == nil {
		return nil
	}
	v.changing = false
	if msg.err != nil {
		return m.report(true, "#%d was not merged: %v", v.number, msg.err)
	}
	v.toEnd, v.files = true, false
	return tea.Batch(m.loadIssue(p, true), status("merged #%d (%s)", v.number, msg.method))
}

// pullEventText is eventText for a pull request's timeline: the events only a
// pull request has, and the ones it words differently.
func pullEventText(st *styles, e IssueEvent, issue Issue) string {
	switch e.Kind {
	case "closed":
		return "closed this"
	case "merged":
		return "merged commit " + e.Subject + " into " + issue.Pull.Base
	case "review_requested":
		if e.Subject == e.Actor {
			return "self-requested a review"
		}
		return "requested a review from " + e.Subject
	case "review_request_removed":
		return "removed the request for a review from " + e.Subject
	case "review_dismissed":
		return "dismissed a review"
	case "ready_for_review":
		return "marked this ready for review"
	case "convert_to_draft":
		return "marked this as a draft"
	case "head_ref_force_pushed":
		return "force-pushed the " + issue.Pull.Head + " branch"
	case "head_ref_deleted":
		return "deleted the " + issue.Pull.Head + " branch"
	case "head_ref_restored":
		return "restored the " + issue.Pull.Head + " branch"
	case "base_ref_changed":
		return "changed the base branch"
	case "auto_merge_enabled":
		return "enabled auto-merge"
	case "auto_merge_disabled":
		return "disabled auto-merge"
	case "copilot_work_started":
		return "started a Copilot review"
	case "pinned":
		return "pinned this pull request"
	case "unpinned":
		return "unpinned this pull request"
	}
	return eventText(st, e, issue.Repository)
}

// reviewLines is one review: who, and what they said of the change, the
// summary they wrote, and each conversation they started on the diff under the
// lines it is about, with its replies.
func (m *Model) reviewLines(v *issuePane, e IssueEvent, width int, now time.Time) []string {
	st := m.st
	said := st.attentionHint.Render("reviewed")
	switch e.Review {
	case "approved":
		said = st.statusOK.Render("approved these changes")
	case "changes_requested":
		said = st.statusER.Render("requested changes")
	case "dismissed":
		said = st.attentionHint.Render("reviewed — since dismissed")
	}
	out := groupBar(st, st.attentionText.Render(e.Actor)+" "+said, st.attentionHint.Render(ago(e.At, now)), width)
	if strings.TrimSpace(e.Body) != "" {
		out = append(out, m.markdown(v, e.Body, width)...)
	}
	indent := strings.Repeat(" ", threadIndent)
	room := max(width-threadIndent, 1)
	for _, thread := range e.Threads {
		where := thread.Path
		if thread.Line > 0 {
			where += ":" + itoa(thread.Line)
		}
		if thread.Outdated {
			where += " · outdated"
		}
		out = append(out, "", indent+st.info.Render(truncateMiddle(where, room)))
		hunk := strings.Split(strings.TrimRight(thread.Hunk, "\n"), "\n")
		if len(hunk) > threadHunkLines {
			hunk = hunk[len(hunk)-threadHunkLines:]
		}
		for _, line := range hunk {
			if line != "" {
				out = append(out, indent+diffLine(st, truncate(untab(line), room)))
			}
		}
		for _, comment := range thread.Comments {
			out = append(out, indent+truncate(st.emphasis.Render(comment.Author)+" "+st.dimText.Render(ago(comment.At, now)), room))
			for _, line := range m.markdown(v, comment.Body, room) {
				out = append(out, indent+line)
			}
			if len(comment.Reactions) > 0 {
				out = append(out, indent+reactionText(st, comment.Reactions))
			}
		}
	}
	return out
}

// untab draws a diff's tabs as spaces.
func untab(line string) string { return strings.ReplaceAll(line, "\t", diffTab) }

// diffLine colors one line of a unified diff by what its first column says:
// added, removed, or the hunk's own header. Without color the column says it.
func diffLine(st *styles, line string) string { return diffPart(st, line, line) }

// diffPart colors text as part of the diff line it was wrapped out of.
func diffPart(st *styles, line, text string) string {
	if style := diffStyle(line); style != nil {
		return style(st).Render(text)
	}
	return text
}

// diffStyle is the style a unified diff line is drawn in, by what its first
// column says; nil for a line of context, which is drawn as it is.
func diffStyle(line string) func(*styles) lipgloss.Style {
	switch {
	case strings.HasPrefix(line, "@@"):
		return func(st *styles) lipgloss.Style { return st.info }
	case strings.HasPrefix(line, "+"):
		return func(st *styles) lipgloss.Style { return st.add }
	case strings.HasPrefix(line, "-"):
		return func(st *styles) lipgloss.Style { return st.del }
	case strings.HasPrefix(line, `\`):
		// "\ No newline at end of file"
		return func(st *styles) lipgloss.Style { return st.dimText }
	}
	return nil
}

// wheel is the wheel over the tab's conversation.
func (v *issuePane) wheel(lines int) { v.scroll(lines) }
