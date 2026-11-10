package console

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The issue tab's rules (876 §1): the GitHub issue or pull request a box is
// tagged with, read in the console rather than a browser. Which issue a box
// has is its tags' (Sandbox.WorkNumber); reading, commenting, closing and
// merging are the data source's, as the person at this machine; what the tab
// says of what it read — a state's badge, whether it can merge and why not,
// an event's words, the checks, who reviewed, the questions a close and a
// merge ask — is here, for the TUI's tab and the desktop's alike. How it is
// laid out and colored is each renderer's: a tone names what a word is, and
// a renderer draws it in its own palette.

const (
	// IssueRefreshEvery is how often an open issue tab reads its issue again,
	// so a comment posted from the page shows up without asking.
	IssueRefreshEvery = time.Minute
	// IssueTimeout bounds one read, one post or one change.
	IssueTimeout = 30 * time.Second
	// PullSettleAfter and PullSettleTries are how soon, and how many times, a
	// pull request GitHub has yet to say can merge is read again: it works
	// that out when first asked, and answers "unknown" until it has.
	PullSettleAfter = 3 * time.Second
	PullSettleTries = 3
	// ThreadHunkLines is how much of the diff a review thread is shown under:
	// the lines leading up to the one it is about, as the page shows them.
	ThreadHunkLines = 4
	// ReviewStartTimeout is how long a pull request's files changed are given
	// to start (DataSource.PullReview): one in a box that is waking can take a
	// while, and one that never comes must not leave a console waiting with no
	// way to say so.
	ReviewStartTimeout = 2 * time.Minute
)

// ErrReviewNotStarted is a pull request's files changed that did not start
// within ReviewStartTimeout, in the words every console says it in.
var ErrReviewNotStarted = fmt.Errorf("discobox-review did not start in the box within %s", ReviewStartTimeout)

// IssueTone is what a word or a mark on the issue tab is, for a renderer to
// draw in its own palette.
type IssueTone string

// The tones.
const (
	// IssueToneOpen is open: an open issue, an approval, a check that passed.
	IssueToneOpen IssueTone = "open"
	// IssueToneShut is closed, a draft, or something said and done with.
	IssueToneShut IssueTone = "shut"
	// IssueToneMerged is merged.
	IssueToneMerged IssueTone = "merged"
	// IssueToneGood is a good outcome said as a sentence: ready to merge,
	// checks passed.
	IssueToneGood IssueTone = "good"
	// IssueToneWarn is something to look at: out of date, pending, partial.
	IssueToneWarn IssueTone = "warn"
	// IssueToneBad is in the way: conflicts, failing, changes requested.
	IssueToneBad IssueTone = "bad"
	// IssueToneBusy is under way: a check still running.
	IssueToneBusy IssueTone = "busy"
	// IssueToneQuiet is said about the rest, beside it.
	IssueToneQuiet IssueTone = "quiet"
)

// IssueMark is a state's mark: filled while there is something to do about
// it, hollow once there is not, in its tone. How it is drawn is each
// renderer's.
type IssueMark struct {
	Filled bool
	Tone   IssueTone
}

// WorkLabel is what the tab on a box's issue or pull request is called, and
// what the header calls it.
func WorkLabel(number int, pull bool) string {
	if pull {
		return "PR #" + strconv.Itoa(number)
	}
	return "issue #" + strconv.Itoa(number)
}

// WorkNoun is what it is, in a sentence.
func WorkNoun(pull bool) string {
	if pull {
		return "pull request"
	}
	return "issue"
}

// WorkAddress is its page on GitHub: repository is the web address
// Sandbox.Repository carries.
func WorkAddress(repository string, number int, pull bool) string {
	if pull {
		return repository + "/pull/" + strconv.Itoa(number)
	}
	return repository + "/issues/" + strconv.Itoa(number)
}

// FilesAddress is the page of a pull request's files changed.
func FilesAddress(repository string, number int) string {
	return WorkAddress(repository, number, true) + "/files"
}

// Work is the number of a box's issue, or of its pull request, on its GitHub
// repository, which its `issue=` or `pr=` tag gives (WorkNumber); or why it
// has none, in the words every console and the registry say it in.
func (s Sandbox) Work(pull bool) (int, error) {
	if n, ok := s.WorkNumber(pull); ok {
		return n, nil
	}
	if pull {
		return 0, errors.New("no pull request to show — tag this discobox pr=N, on a GitHub repository")
	}
	return 0, errors.New("no issue to show — tag this discobox issue=N, on a GitHub repository")
}

// Badge is the state the page draws beside the title: open, closed as done
// or as something else; for a pull request, merged or a draft.
func (i Issue) Badge() (string, IssueMark) {
	if pull := i.Pull; pull != nil {
		switch {
		case pull.Merged:
			return "Merged", IssueMark{Filled: true, Tone: IssueToneMerged}
		case i.State == "closed":
			return "Closed", IssueMark{Tone: IssueToneShut}
		case pull.Draft:
			return "Draft", IssueMark{Tone: IssueToneShut}
		}
		return "Open", IssueMark{Filled: true, Tone: IssueToneOpen}
	}
	switch {
	case i.State == "open":
		return "Open", IssueMark{Filled: true, Tone: IssueToneOpen}
	case i.StateReason == "not_planned":
		return "Closed as not planned", IssueMark{Tone: IssueToneShut}
	case i.StateReason == "duplicate":
		return "Closed as duplicate", IssueMark{Tone: IssueToneShut}
	}
	return "Closed", IssueMark{Tone: IssueToneShut}
}

// Ready reports whether a pull request is ready to merge: open, not a draft,
// and what GitHub calls clean — no conflicts, its checks passed, and every
// review and rule the branch requires satisfied.
func (i Issue) Ready() bool {
	pull := i.Pull
	return pull != nil && i.State == "open" && !pull.Draft && (pull.Mergeable == "clean" || pull.Mergeable == "has_hooks")
}

// Settling reports whether GitHub has yet to say whether an open pull request
// can merge: it is read again soon (PullSettleAfter, PullSettleTries).
func (i Issue) Settling() bool {
	return i.Pull != nil && i.State == "open" && (i.Pull.Mergeable == "unknown" || i.Pull.Mergeable == "")
}

// MergeStanding is the page's merge box for an open pull request: whether it
// can merge now, in GitHub's own terms, and when it cannot, why.
type MergeStanding struct {
	// Shown is whether there is one: a pull request that is open.
	Shown bool
	// Ready is a pull request with nothing in its way (Issue.Ready).
	Ready bool
	// Unknown is GitHub not having said yet: asking again is the answer.
	Unknown bool
	// Why is what is in the way of one that is not ready, said after "Not
	// ready to merge — ".
	Why  string
	Mark IssueMark
}

// Merge is the pull request's merge box.
func (i Issue) Merge() MergeStanding {
	pull := i.Pull
	if pull == nil || i.State != "open" {
		return MergeStanding{}
	}
	m := MergeStanding{Shown: true, Mark: IssueMark{Filled: true, Tone: IssueToneWarn}}
	switch {
	case i.Ready():
		m.Ready, m.Mark.Tone = true, IssueToneGood
	case pull.Draft || pull.Mergeable == "draft":
		m.Why, m.Mark = "a draft, to be marked ready for review first", IssueMark{Tone: IssueToneQuiet}
	case pull.Mergeable == "dirty":
		m.Why, m.Mark.Tone = "conflicts with "+pull.Base+" must be resolved", IssueToneBad
	case pull.Mergeable == "behind":
		m.Why = "out of date with " + pull.Base
	case pull.Mergeable == "blocked":
		m.Why = "a required review or check is missing"
	case pull.Mergeable == "unstable":
		m.Why = "its checks have not passed"
	default:
		m.Unknown, m.Mark = true, IssueMark{Tone: IssueToneQuiet}
	}
	return m
}

// Said is the merge box as one sentence; its mark is the renderer's to draw.
func (m MergeStanding) Said(base string) string {
	switch {
	case !m.Shown:
		return ""
	case m.Ready:
		return "Ready to merge into " + base
	case m.Unknown:
		return "GitHub has not said yet whether this can merge"
	}
	return "Not ready to merge — " + m.Why
}

// PushedNotice says the branch was pushed to since the tab last showed the
// person its head: what was read before then is not what would be merged.
func PushedNotice(was, now string) string {
	return "commits were pushed since " + ShortCommit(was) + " was read here: the head is now " + ShortCommit(now)
}

// ShownHead is the head of a pull request a tab has shown the person: the
// one its first read brought, and then only the one a read they asked for
// brings. A read on the tab's own clock that brings a newer head does not
// move it: what was read here is no longer what would be merged, and the tab
// says so (PushedNotice) until they ask to see the new one. Both consoles
// follow an issue through one.
type ShownHead struct{ head string }

// NewShownHead is a tab that has shown head already, or nothing for "".
func NewShownHead(head string) ShownHead { return ShownHead{head: head} }

// Read takes one read, asked for by the person or not, and is the head they
// were shown before a push moved it — empty when what they are shown is
// what was read.
func (s *ShownHead) Read(i Issue, asked bool) string {
	if i.Pull == nil {
		s.head = ""
		return ""
	}
	if asked || s.head == "" {
		s.head = i.Pull.HeadSHA
	}
	if s.head == i.Pull.HeadSHA {
		return ""
	}
	return s.head
}

// Head is the head the person was shown.
func (s ShownHead) Head() string { return s.head }

// ReviewerSaid is what one of the page's reviewers said, and its mark.
func ReviewerSaid(state string) (string, IssueMark) {
	switch state {
	case "approved":
		return "approved", IssueMark{Filled: true, Tone: IssueToneOpen}
	case "changes_requested":
		return "requested changes", IssueMark{Filled: true, Tone: IssueToneBad}
	case "commented":
		return "commented", IssueMark{Tone: IssueToneShut}
	case "dismissed":
		return "review dismissed", IssueMark{Tone: IssueToneShut}
	}
	return "review requested", IssueMark{Tone: IssueToneQuiet}
}

// ReviewSaid is what a review in the timeline said of the change, in its tone.
func ReviewSaid(review string) (string, IssueTone) {
	switch review {
	case "approved":
		return "approved these changes", IssueToneGood
	case "changes_requested":
		return "requested changes", IssueToneBad
	case "dismissed":
		return "reviewed — since dismissed", IssueToneQuiet
	}
	return "reviewed", IssueToneQuiet
}

// CheckSummary is how a pull request's head commit's checks stand: how many
// failed, are pending and passed, whether that is all of them, and the ones
// that have not passed, which are the ones to go and read.
type CheckSummary struct {
	Failing, Pending, Passed int
	Partial                  bool
	Unpassed                 []PullCheck
}

// CheckSummary is the head commit's checks as the tab sums them up.
func (p PullRequest) CheckSummary() CheckSummary {
	s := CheckSummary{Partial: p.ChecksPartial}
	for _, check := range p.Checks {
		switch check.State {
		case "failure", "canceled":
			s.Failing++
		case "pending":
			s.Pending++
		default:
			s.Passed++
			continue
		}
		s.Unpassed = append(s.Unpassed, check)
	}
	return s
}

// CheckMark is a check that has not passed, as its row marks it.
func CheckMark(state string) IssueMark {
	switch state {
	case "failure":
		return IssueMark{Filled: true, Tone: IssueToneBad}
	case "canceled":
		return IssueMark{Tone: IssueToneShut}
	case "pending":
		return IssueMark{Filled: true, Tone: IssueToneBusy}
	}
	return IssueMark{Filled: true, Tone: IssueToneOpen}
}

// RefMark is another issue or pull request's state, as a line naming it marks
// it.
func RefMark(ref IssueRef) IssueMark {
	switch {
	case ref.Merged:
		return IssueMark{Filled: true, Tone: IssueToneMerged}
	case ref.State == "closed":
		return IssueMark{Tone: IssueToneShut}
	}
	return IssueMark{Filled: true, Tone: IssueToneOpen}
}

// RefNumber is another issue or pull request's number as a line names it:
// with its repository when that is not this one's.
func RefNumber(ref IssueRef, repository string) string {
	number := "#" + strconv.Itoa(ref.Number)
	if ref.Repository != "" && ref.Repository != repository {
		number = ref.Repository + number
	}
	return number
}

// TimelineShows reports whether the timeline's event at i is drawn: merging
// a pull request closes it, and the page says the one thing.
func (i Issue) TimelineShows(at int) bool {
	e := i.Timeline[at]
	return e.Kind != "closed" || i.Pull == nil || at == 0 || i.Timeline[at-1].Kind != "merged"
}

// EventText is what the page says an event did, after the name of who did it:
// a pull request's events in its own words, and a kind it has no words for
// named as GitHub names it. A comment, a review and a pushed commit are drawn
// as themselves, not said.
func (i Issue) EventText(e IssueEvent) string {
	if pull := i.Pull; pull != nil {
		switch e.Kind {
		case "closed":
			return "closed this"
		case "merged":
			return "merged commit " + e.Subject + " into " + pull.Base
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
			return "force-pushed the " + pull.Head + " branch"
		case "head_ref_deleted":
			return "deleted the " + pull.Head + " branch"
		case "head_ref_restored":
			return "restored the " + pull.Head + " branch"
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
	}
	label := ""
	if e.Label != nil {
		label = e.Label.Name
	}
	switch e.Kind {
	case "labeled":
		return "added the " + label + " label"
	case "unlabeled":
		return "removed the " + label + " label"
	case "assigned":
		if e.Subject == e.Actor {
			return "self-assigned this"
		}
		return "assigned " + e.Subject
	case "unassigned":
		if e.Subject == e.Actor {
			return "removed their assignment"
		}
		return "unassigned " + e.Subject
	case "milestoned":
		return "added this to the " + e.Subject + " milestone"
	case "demilestoned":
		return "removed this from the " + e.Subject + " milestone"
	case "renamed":
		return "changed the title from “" + e.From + "” to “" + e.To + "”"
	case "closed":
		how := "closed this as completed"
		switch e.StateReason {
		case "not_planned":
			how = "closed this as not planned"
		case "duplicate":
			how = "closed this as a duplicate"
		}
		if e.Subject != "" {
			how += " in " + e.Subject
		}
		return how
	case "reopened":
		return "reopened this"
	case "locked":
		if e.Subject != "" {
			return "locked this as " + strings.ReplaceAll(e.Subject, "_", " ") + " and limited the conversation"
		}
		return "locked and limited the conversation"
	case "unlocked":
		return "unlocked this conversation"
	case "referenced":
		return "referenced this in commit " + e.Subject
	case "cross-referenced":
		if e.Source == nil {
			return "mentioned this"
		}
		return "mentioned this in " + RefNumber(*e.Source, i.Repository) + " " + e.Source.Title
	case "pinned":
		return "pinned this issue"
	case "unpinned":
		return "unpinned this issue"
	case "transferred":
		return "transferred this issue"
	case "marked_as_duplicate":
		return "marked this as a duplicate"
	case "unmarked_as_duplicate":
		return "marked this as not a duplicate"
	case "connected":
		return "linked a pull request that will close this issue"
	case "disconnected":
		return "removed a link to a pull request"
	case "sub_issue_added":
		return "added a sub-issue"
	case "sub_issue_removed":
		return "removed a sub-issue"
	case "parent_issue_added":
		return "added a parent issue"
	case "parent_issue_removed":
		return "removed a parent issue"
	case "issue_type_added", "issue_type_changed":
		return "set the issue type"
	case "issue_type_removed":
		return "removed the issue type"
	}
	return strings.ReplaceAll(e.Kind, "_", " ")
}

// ReactionOrder is the page's order for the reactions, which are said by
// GitHub's names for them: an emoji is two cells in one terminal and one in
// the next.
var ReactionOrder = []string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}

// ReactionText is the reactions under a comment, in the page's order.
func ReactionText(reactions IssueReactions) string {
	var parts []string
	for _, name := range ReactionOrder {
		if n := reactions[name]; n > 0 {
			parts = append(parts, name+" "+strconv.Itoa(n))
		}
	}
	return strings.Join(parts, " · ")
}

// ThreadWhere is where a review thread is on the diff: its file, its line,
// and whether the diff has moved on from under it.
func ThreadWhere(t ReviewThread) string {
	where := t.Path
	if t.Line > 0 {
		where += ":" + strconv.Itoa(t.Line)
	}
	if t.Outdated {
		where += " · outdated"
	}
	return where
}

// ThreadHunk is the diff a review thread is shown under: its last
// ThreadHunkLines lines, blank ones left out.
func ThreadHunk(t ReviewThread) []string {
	hunk := strings.Split(strings.TrimRight(t.Hunk, "\n"), "\n")
	if len(hunk) > ThreadHunkLines {
		hunk = hunk[len(hunk)-ThreadHunkLines:]
	}
	out := make([]string, 0, len(hunk))
	for _, line := range hunk {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// The answers a close asks for, by key: the page's two ways to close an
// issue. A pull request closes one way, and reopening is a yes or no.
const (
	CloseCompleted  = "completed"
	CloseNotPlanned = "not_planned"
)

// StateQuestion is what closing or reopening the issue asks: an open issue is
// closed — as done, or as not planned, the page's two ways — and a closed one
// reopened; a pull request closes one way, unmerged, and a merged one is past
// both, which is refused. It is asked rather than done: it is public, and
// everyone watching the issue hears about it.
func (i Issue) StateQuestion() (Question, error) {
	number := "#" + strconv.Itoa(i.Number)
	switch {
	case i.Pull != nil && i.Pull.Merged:
		return Question{}, fmt.Errorf("%s is merged — there is nothing to close or reopen", WorkLabel(i.Number, true))
	case i.State == "closed":
		return Question{Title: "Reopen " + number + "?", Body: i.Title}, nil
	case i.Pull != nil:
		return Question{Title: "Close " + number + " without merging it?", Body: i.Title}, nil
	}
	return Question{Title: "Close " + number, Body: i.Title, Choices: []Choice{
		{Key: CloseCompleted, Label: "close as completed", Hint: "done, fixed, or answered"},
		{Key: CloseNotPlanned, Label: "close as not planned", Hint: "won't fix, can't reproduce, duplicate, or stale"},
	}}, nil
}

// StateAnswer is the change StateQuestion's answer asks for: the state to set
// and the reason GitHub is given, and false for an answer that changes
// nothing.
func (i Issue) StateAnswer(key string) (state, reason string, ok bool) {
	switch {
	case i.State == "closed" && key == AnswerYes:
		if i.Pull != nil {
			return "open", "", true
		}
		return "open", "reopened", true
	case i.State == "closed":
		return "", "", false
	case i.Pull != nil && key == AnswerYes:
		return "closed", "", true
	case i.Pull == nil && (key == CloseCompleted || key == CloseNotPlanned):
		return "closed", key, true
	}
	return "", "", false
}

// StateChanged is what the status line says of a close or a reopen done, or
// of one GitHub refused.
func StateChanged(number int, state, reason string, err error) string {
	n := "#" + strconv.Itoa(number)
	if err != nil {
		verb := "reopened"
		if state == "closed" {
			verb = "closed"
		}
		return n + " was not " + verb + ": " + err.Error()
	}
	if state != "closed" {
		return "reopened " + n
	}
	if reason != "" {
		return "closed " + n + " as " + strings.ReplaceAll(reason, "_", " ")
	}
	return "closed " + n
}

// Commented is what the status line says of a comment posted, or refused.
func Commented(number int, err error) string {
	if err != nil {
		return "the comment was not posted: " + err.Error()
	}
	return "commented on #" + strconv.Itoa(number)
}

// ErrEmptyComment is a comment with nothing in it, which is not posted.
var ErrEmptyComment = errors.New("nothing to post — the comment is empty")

// CommentBody is a comment as it is posted: as written — an indented first
// line is a code block in markdown, and trimming it would change what it
// says — unless it says nothing at all.
func CommentBody(body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", ErrEmptyComment
	}
	return body, nil
}

// MergeMethods are the ways to merge as the page offers them, by the key
// GitHub knows each by.
var MergeMethods = []Choice{
	{Key: "squash", Label: "squash and merge", Hint: "its commits become one commit on the base branch"},
	{Key: "merge", Label: "create a merge commit", Hint: "every commit is kept, joined by a merge commit"},
	{Key: "rebase", Label: "rebase and merge", Hint: "every commit is replayed onto the base branch"},
}

// MergeQuestion is what merging a pull request asks: which way, of the ways
// the repository allows — one it does not is offered with why not. It is
// refused here only where GitHub could not merge it either; a merge GitHub
// would refuse for a rule is GitHub's to refuse, with its own reason. head is
// the commit that would be merged — the one the person was shown — and
// pushed the head they were shown before a push moved it, when one did.
func (i Issue) MergeQuestion(head, pushed string) (Question, error) {
	label := WorkLabel(i.Number, true)
	pull := i.Pull
	switch {
	case pull == nil:
		return Question{}, fmt.Errorf("#%d is not a pull request", i.Number)
	case pull.Merged:
		return Question{}, fmt.Errorf("%s is already merged", label)
	case i.State != "open":
		return Question{}, fmt.Errorf("%s is closed — it is reopened first", label)
	case pull.Draft:
		return Question{}, fmt.Errorf("%s is a draft — it is marked ready for review on GitHub first", label)
	case pull.Mergeable == "dirty":
		return Question{}, fmt.Errorf("%s conflicts with %s — the conflicts are resolved first", label, pull.Base)
	}
	choices := make([]Choice, len(MergeMethods))
	for n, c := range MergeMethods {
		// A repository that would not say which it allows is asked by trying.
		if len(pull.MergeMethods) > 0 && !slices.Contains(pull.MergeMethods, c.Key) {
			c.Why = "not allowed in this repository"
		}
		choices[n] = c
	}
	body := i.Title
	if m := i.Merge(); !m.Ready {
		body += "\n\n" + m.Said(pull.Base)
	}
	// The commit that will be merged is named: a tab reads itself again on its
	// own clock, so the head may be newer than the diff that was read.
	body += "\n\nat " + ShortCommit(head)
	if pushed != "" {
		body += " — " + PushedNotice(pushed, head)
	}
	return Question{Title: "Merge #" + strconv.Itoa(i.Number) + " into " + pull.Base, Body: body, Choices: choices}, nil
}

// MergeMethod is the way MergeQuestion's answer merges, and false for an
// answer that is not one of the ways the repository allows.
func (i Issue) MergeMethod(key string) (string, bool) {
	q, err := i.MergeQuestion("", "")
	if err != nil {
		return "", false
	}
	for _, c := range q.Choices {
		if c.Key == key && c.Why == "" {
			return key, true
		}
	}
	return "", false
}

// Merged is what the status line says of a merge done, or refused.
func Merged(number int, method string, err error) string {
	if err != nil {
		return "#" + strconv.Itoa(number) + " was not merged: " + err.Error()
	}
	return "merged #" + strconv.Itoa(number) + " (" + method + ")"
}

// The acts on a box's issue, as the person at this machine: each takes the
// answer its question was given — however its console asked it — refuses
// what the question would, does it, and words what came of it for the
// status line. An answer that changes nothing is context.Canceled, as a
// question declined is.

// PostComment posts a comment on an issue or pull request, as written
// (CommentBody).
func PostComment(ctx context.Context, src DataSource, repository string, number int, body string) (string, error) {
	body, err := CommentBody(body)
	if err != nil {
		return "", err
	}
	if err := src.CommentOnIssue(ctx, repository, number, body); err != nil {
		return "", errors.New(Commented(number, err))
	}
	return Commented(number, nil), nil
}

// ChangeIssueState closes or reopens the issue StateQuestion asked about, as
// its answer says.
func ChangeIssueState(ctx context.Context, src DataSource, repository string, i Issue, answer string) (string, error) {
	if _, err := i.StateQuestion(); err != nil {
		return "", err
	}
	state, reason, ok := i.StateAnswer(answer)
	if !ok {
		return "", context.Canceled
	}
	if err := src.SetIssueState(ctx, repository, i.Number, state, reason); err != nil {
		return "", errors.New(StateChanged(i.Number, state, reason, err))
	}
	return StateChanged(i.Number, state, reason, nil), nil
}

// MergeIssue merges the pull request MergeQuestion asked about, the way its
// answer chose, at head: the commit the person was shown, so a branch pushed
// to since is GitHub's to refuse rather than merged unseen.
func MergeIssue(ctx context.Context, src DataSource, repository string, i Issue, answer, head string) (string, error) {
	if _, err := i.MergeQuestion(head, ""); err != nil {
		return "", err
	}
	method, ok := i.MergeMethod(answer)
	if !ok {
		return "", context.Canceled
	}
	if err := src.MergePullRequest(ctx, repository, i.Number, method, head); err != nil {
		return "", errors.New(Merged(i.Number, method, err))
	}
	return Merged(i.Number, method, nil), nil
}
