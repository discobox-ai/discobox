package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// What a pull request's page has that an issue's does not, drawn into the
// issue tab (issue.go): the badge and the branches under the title, the
// reviewers, the size of the change, the checks and whether it can merge among
// the facts; the pushes, the reviews and their threads on the diff in the
// timeline; and the files changed, as a page of its own behind f.

const (
	// threadIndent sets a thread in from the review it belongs to.
	threadIndent = 2
	// pullMergeKey merges a pull request, behind a question.
	pullMergeKey = "m"
	// mergeBandHint is what the ready band says beside that key, at its right
	// end: the two together are a control (viewPaneBox marks them).
	mergeBandHint = " merges it "
	// diffTab is how wide a tab in a diff is drawn. A real one would move the
	// cursor by a rule the frame's arithmetic does not know.
	diffTab = "    "
)

// pullByline is the line under the title, which says what the page says: who
// wants what merged into where, or who merged it.
func pullByline(st *styles, issue console.Issue, now time.Time) string {
	pull := issue.Pull
	who, verb, when := issue.Author, "wants to merge", issue.CreatedAt
	if pull.Merged {
		who, verb, when = pull.MergedBy, "merged", pull.MergedAt
	}
	return issueBadge(st, issue) + "  " + st.emphasis.Render(who) + " " +
		st.dimText.Render(verb+" "+plural(pull.Commits, "commit", "commits")+" into ") + pull.Base +
		st.dimText.Render(" from ") + pull.Head +
		st.dimText.Render(" · "+console.Ago(when, now)+" · "+plural(issue.Comments, "comment", "comments"))
}

// reviewerRows is the page's Reviewers list, a reviewer a row, each with what
// they said as the core words it (console.ReviewerSaid).
func reviewerRows(st *styles, reviewers []console.PullReviewer, width int) []string {
	rows := make([]string, 0, len(reviewers))
	for _, reviewer := range reviewers {
		said, mark := console.ReviewerSaid(reviewer.State)
		rows = append(rows, truncate(markStyle(st, mark.Tone).Render(markGlyph(mark))+" "+reviewer.Name+" "+st.dimText.Render(said), width))
	}
	return rows
}

// pullChanges is the size of the change: the counts over the page's tabs.
func pullChanges(st *styles, pull console.PullRequest) string {
	return plural(pull.ChangedFiles, "file", "files") + st.dimText.Render(" · ") +
		plural(pull.Commits, "commit", "commits") + st.dimText.Render(" · ") + plusMinus(st, pull.Additions, pull.Deletions)
}

// plusMinus is a count of lines added and removed, in the colors the header's
// own diffstat is drawn in.
func plusMinus(st *styles, additions, deletions int) string {
	return st.add.Render("+"+itoa(additions)) + " " + st.del.Render("−"+itoa(deletions))
}

// checkRows is the head commit's checks as the core sums them up
// (console.PullRequest.CheckSummary): how they stand, in one row, and then every
// one that has not passed, since those are the ones to go and read. Each is a
// link to its own page, where it has one. A list that is not all of them
// says so first, so a row of passes is not read as the whole.
func checkRows(st *styles, pull console.PullRequest, width int) []string {
	if len(pull.Checks) == 0 {
		if pull.ChecksPartial {
			return []string{st.statusWA.Render(truncate("could not be read — o opens them on GitHub", width))}
		}
		return nil
	}
	sum := pull.CheckSummary()
	var parts []string
	if sum.Failing > 0 {
		parts = append(parts, st.statusER.Render(itoa(sum.Failing)+" failing"))
	}
	if sum.Pending > 0 {
		parts = append(parts, st.statusWA.Render(itoa(sum.Pending)+" pending"))
	}
	if sum.Passed > 0 {
		parts = append(parts, st.statusOK.Render(itoa(sum.Passed)+" passed"))
	}
	if sum.Partial {
		parts = append(parts, st.statusWA.Render("not all could be read"))
	}
	rows := []string{truncate(strings.Join(parts, st.dimText.Render(" · ")), width)}
	for _, check := range sum.Unpassed {
		mark := console.CheckMark(check.State)
		name := truncate(check.Name, max(width-2, 1))
		if webLink(check.URL) {
			name = hyperlink(check.URL, name)
		}
		rows = append(rows, markStyle(st, mark.Tone).Render(markGlyph(mark))+" "+name)
	}
	return rows
}

// mergeBanner is the page's merge box, as one bar across the top of an open
// pull request: whether it can merge now, as the core says it
// (console.Issue.Merge). It is the first thing under the title because it is
// the question the tab is opened to answer; ready is a band across the tab,
// so it is seen without being read.
func mergeBanner(st *styles, issue console.Issue, width int) string {
	m := issue.Merge()
	switch {
	case !m.Shown:
		return ""
	case m.Ready:
		// A band the width of the tab, in the field a group's title bar is
		// drawn on, with the three things the window's bands carry: the mark
		// that catches the eye, the sentence, and the key that acts.
		left := " " + st.statusOK.Bold(true).Render(markGlyph(m.Mark)+" Ready to merge") + st.attentionText.Render(" into "+issue.Pull.Base)
		right := st.key.Render(pullMergeKey) + st.attentionHint.Render(mergeBandHint)
		return highlight(st, padANSI(spread(truncate(left, max(width-lipgloss.Width(right)-1, 1)), right, width), width), colInactive)
	case m.Unknown:
		return st.dimText.Render(truncate(markGlyph(m.Mark)+" "+m.Said(issue.Pull.Base)+" — r asks again", width))
	}
	paint := st.statusWA
	switch m.Mark.Tone {
	case console.IssueToneBad:
		paint = st.statusER
	case console.IssueToneQuiet:
		paint = st.dimText
	}
	return paint.Bold(true).Render(truncate(markGlyph(m.Mark)+" "+m.Said(issue.Pull.Base), width))
}

// mergeBandKeyFits reports whether a ready band that wide has its key on it:
// narrower, the band gives the key up for the sentence (spread), and there is
// nothing there to mark or light.
func mergeBandKeyFits(width int) bool {
	return width >= lipgloss.Width(pullMergeKey+mergeBandHint)+2
}

// pullSettle reads a pull request again soon when GitHub had yet to say
// whether it can merge: it works that out when first asked, and answers
// "unknown" until it has. A few times only — the minute's tick is there after.
func (m *Model) pullSettle(p *pane) tea.Cmd {
	v := p.issue
	if !v.issue.Settling() || v.settles >= console.PullSettleTries {
		return nil
	}
	v.settles++
	id := p.id
	return tick(console.PullSettleAfter, func(time.Time) tea.Msg { return pullSettleMsg{id: id} })
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

// pullMergedMsg is a merge done, as the core words it, or why it was not.
type pullMergedMsg struct {
	id   int
	note string
	err  error
}

// askMerge is m on a pull request: the core's question of which way to merge
// it (console.Issue.MergeQuestion), of the ways the repository allows, and
// refused where GitHub could not merge it either. The commit merged is the
// one that was read.
func (m *Model) askMerge(p *pane) tea.Cmd {
	v := p.issue
	switch {
	case v.issue == nil:
		return status("the pull request has not been read yet")
	case v.issue.Pull == nil || v.changing:
		return nil
	}
	sha := v.issue.Pull.HeadSHA
	q, err := v.issue.MergeQuestion(sha, v.pushed)
	if err != nil {
		return status("%v", err)
	}
	issue := *v.issue
	d := questionDialog(q, func(key string) tea.Cmd {
		if _, ok := issue.MergeMethod(key); !ok {
			return nil
		}
		return m.mergePull(p, issue, key, sha)
	})
	d.over = p
	m.dialog = d
	return nil
}

// mergePull sends the merge the answer to the question asked of issue chose,
// of the commit that was read (console.MergeIssue).
func (m *Model) mergePull(p *pane, issue console.Issue, answer, sha string) tea.Cmd {
	v := p.issue
	v.changing = true
	ctx, ds, id, repository, number := m.ctx, m.ds, p.id, v.repository, v.number
	merge := func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, console.IssueTimeout)
		defer cancel()
		note, err := console.MergeIssue(ctx, ds, repository, issue, answer, sha)
		return pullMergedMsg{id: id, note: note, err: err}
	}
	// Install progress before starting the merge: a fast refusal must not be
	// overwritten by a sibling start notice. Keep the normal status expiry.
	expires := m.update(status("merging #%d…", number)())
	return tea.Batch(merge, expires)
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
		return m.report(true, "%v", msg.err)
	}
	v.toEnd, v.files = true, false
	return tea.Batch(m.loadIssue(p, true), status("%s", msg.note))
}

// reviewLines is one review: who, and what they said of the change, the
// summary they wrote, and each conversation they started on the diff under the
// lines it is about, with its replies.
func (m *Model) reviewLines(v *issuePane, e console.IssueEvent, width int, now time.Time) []string {
	st := m.st
	words, tone := console.ReviewSaid(e.Review)
	said := st.attentionHint.Render(words)
	switch tone {
	case console.IssueToneGood:
		said = st.statusOK.Render(words)
	case console.IssueToneBad:
		said = st.statusER.Render(words)
	}
	out := groupBar(st, st.attentionText.Render(e.Actor)+" "+said, st.attentionHint.Render(console.Ago(e.At, now)), width)
	if strings.TrimSpace(e.Body) != "" {
		out = append(out, m.markdown(v, e.Body, width)...)
	}
	indent := strings.Repeat(" ", threadIndent)
	room := max(width-threadIndent, 1)
	for _, thread := range e.Threads {
		out = append(out, "", indent+st.info.Render(truncateMiddle(console.ThreadWhere(thread), room)))
		for _, line := range console.ThreadHunk(thread) {
			out = append(out, indent+diffLine(st, truncate(untab(line), room)))
		}
		for _, comment := range thread.Comments {
			out = append(out, indent+truncate(st.emphasis.Render(comment.Author)+" "+st.dimText.Render(console.Ago(comment.At, now)), room))
			for _, line := range m.markdown(v, comment.Body, room) {
				out = append(out, indent+line)
			}
			if len(comment.Reactions) > 0 {
				out = append(out, indent+st.dimText.Render(console.ReactionText(comment.Reactions)))
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
