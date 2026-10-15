package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// mergeSettleTries is how many more times a full read asks GitHub whether a
// pull request can merge, when it has yet to say.
const mergeSettleTries = 2

// gitHubCanceled is a canceled check run's conclusion, in the API's spelling.
const gitHubCanceled = "cancelled" //nolint:misspell // GitHub's own word

// checkPages bounds how many of a commit's check runs are read, 100 a page.
const checkPages = 5

// Pull is what a pull request's page shows beyond the issue it also is: every
// pull request is an issue to this API, so its title, description, labels and
// timeline are the Issue's, and this is the rest.
type Pull struct {
	Draft    bool
	Merged   bool
	MergedBy string
	MergedAt time.Time
	// Head and Base are the branches, as the page names them: the branch
	// alone, or owner:branch for a head in a fork.
	Head, Base string
	// HeadSHA is the commit the checks are about, and BaseSHA where the base
	// branch was when the pull request was read: the two its diff is between.
	HeadSHA      string
	BaseSHA      string
	Commits      int
	Additions    int
	Deletions    int
	ChangedFiles int
	// Mergeable is GitHub's mergeable_state: clean, dirty (conflicts),
	// behind, blocked, unstable, draft, or unknown while it works it out.
	Mergeable string
	// MergeMethods are the ways the repository lets a pull request merge, of
	// merge, squash and rebase, in the page's order. Empty when the repository
	// would not say, which it does not to someone who cannot push to it.
	MergeMethods []string
	// Reviewers are the people the page's sidebar lists: who was asked, and
	// what the last review of each who answered said.
	Reviewers []Reviewer
	// Checks are the head commit's check runs and commit statuses, and
	// ChecksPartial is set when not all of them could be read: a request for
	// them failed, or there are more than are read.
	Checks        []Check
	ChecksPartial bool
}

// Reviewer is one person or team in the page's Reviewers list. State is
// "requested" for one asked and yet to answer, and otherwise what their last
// review said: approved, changes_requested, commented, or dismissed.
type Reviewer struct {
	Name  string
	State string
}

// Check is one check run or commit status on the head commit. State is
// pending, success, failure, canceled, skipped, or neutral.
type Check struct {
	Name  string
	State string
	URL   string
}

// Thread is one conversation a review started on a line of the diff: the
// comment that opened it and the replies under it.
type Thread struct {
	Path string
	// Line is the line it is on, in the file as the pull request leaves it.
	// Outdated is set when the diff has moved on from under it, and Line is
	// then where it was.
	Line     int
	Outdated bool
	// Hunk is the diff it was written against, ending at its line.
	Hunk     string
	Comments []ThreadComment
}

// ThreadComment is one comment of a thread.
type ThreadComment struct {
	Author    string
	At        time.Time
	Body      string
	Reactions Reactions
}

// checksMove reports whether the checks are worth asking about again while
// nothing else has changed: a check finishing does not move the pull
// request's updated_at, and one that is merged or closed has stopped running
// them.
func (i Issue) checksMove() bool {
	return i.Pull != nil && i.State == "open"
}

// MergeMethods are the ways a pull request merges, as the API names them, in
// the order the page offers them.
var MergeMethods = []string{"merge", "squash", "rebase"}

// Merge merges a pull request, as whoever the token is: method is one of
// MergeMethods, and sha the head commit that was read — GitHub refuses when
// the branch has moved on since, so what is merged is what was looked at.
func (c *Client) Merge(ctx context.Context, owner, name string, number int, method, sha string) error {
	if c.bearer(ctx) == "" {
		return ErrNoToken
	}
	fields := map[string]string{"merge_method": method}
	if sha != "" {
		fields["sha"] = sha
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	c.forget(issueKey{owner, name, number})
	_, err = c.do(ctx, http.MethodPut, fmt.Sprintf("%s/pulls/%d/merge", repoPath(owner, name), number), payload, nil)
	return err
}

// mergeState reads the pull request alone, for whether it can merge now: that
// moves with the base branch and with GitHub's own working it out, and neither
// moves the pull request's updated_at. A failure leaves what was known.
func (c *Client) mergeState(ctx context.Context, owner, name string, number int, pull *Pull) {
	var raw rawPull
	if _, err := c.get(ctx, fmt.Sprintf("%s/pulls/%d", repoPath(owner, name), number), &raw); err != nil {
		return
	}
	pull.Mergeable, pull.Draft = raw.Mergeable, raw.Draft
}

// mergeMethods asks the repository which ways it merges. It is not the pull
// request: a repository that will not say leaves the question to the merge.
func (c *Client) mergeMethods(ctx context.Context, repo string) []string {
	var raw struct {
		Merge  *bool `json:"allow_merge_commit"`
		Squash *bool `json:"allow_squash_merge"`
		Rebase *bool `json:"allow_rebase_merge"`
	}
	if _, err := c.get(ctx, repo, &raw); err != nil {
		return nil
	}
	var methods []string
	for i, allowed := range []*bool{raw.Merge, raw.Squash, raw.Rebase} {
		if allowed != nil && *allowed {
			methods = append(methods, MergeMethods[i])
		}
	}
	return methods
}

// readPull is the rest of a full read of a pull request: the pull request
// itself, the review threads — put under the reviews of the timeline already
// read — the head commit's checks, and the diff.
func (c *Client) readPull(ctx context.Context, owner, name string, issue *Issue, reviews map[int64]int) error {
	repo := repoPath(owner, name)
	base := fmt.Sprintf("%s/pulls/%d", repo, issue.Number)
	var raw rawPull
	if _, err := c.get(ctx, base, &raw); err != nil {
		return err
	}
	// GitHub works out whether a pull request can merge when it is first
	// asked, and answers "unknown" until it has: asked again in a moment, it
	// usually knows. Not with no token: it answers "unknown" to a reader it
	// does not know however often it is asked, and there are 60 requests an
	// hour to ask with.
	tries := mergeSettleTries
	if c.bearer(ctx) == "" {
		tries = 0
	}
	for try := 0; try < tries && issue.State == "open" && (raw.Mergeable == "unknown" || raw.Mergeable == ""); try++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.Settle):
		}
		var again rawPull
		if _, err := c.get(ctx, base, &again); err != nil {
			break
		}
		raw = again
	}
	pull := raw.pull()
	if issue.State == "open" {
		pull.MergeMethods = c.mergeMethods(ctx, repo)
	}

	if raw.ReviewComments > 0 {
		var comments []rawReviewComment
		next := base + "/comments?per_page=100"
		for page := 0; next != "" && page < timelinePages; page++ {
			var items []rawReviewComment
			link, err := c.get(ctx, next, &items)
			if err != nil {
				return err
			}
			comments = append(comments, items...)
			next = nextPage(link)
		}
		// More review comments than are read: the threads past here, and a
		// review that only said something in one of them, are not on the page.
		if next != "" {
			issue.Truncated = true
		}
		for review, threads := range reviewThreads(comments) {
			if at, ok := reviews[review]; ok {
				issue.Timeline[at].Threads = threads
			}
		}
	}
	issue.Timeline = spokenReviews(issue.Timeline)
	pull.Reviewers = reviewers(issue.Author, issue.Timeline, raw)

	pull.Checks, pull.ChecksPartial, _ = c.checks(ctx, repo, pull.HeadSHA)

	issue.Pull = &pull
	return nil
}

// checks are a commit's check runs and then its commit statuses, the two ways
// a check reports. They are not the pull request: a token that may not read
// them — a fine-grained one without the permission — leaves the page without
// them rather than unreadable. So a failure is not the read's: partial says
// the list is not all of them — a request failed, or there are more pages than
// are read — and a caller that had them before keeps what it had (ok false)
// when not one could be read now.
func (c *Client) checks(ctx context.Context, repo, sha string) (checks []Check, partial, ok bool) {
	if sha == "" {
		return nil, false, true
	}
	commit := repo + "/commits/" + url.PathEscape(sha)
	next := commit + "/check-runs?per_page=100"
	read := 0
	for page := 0; next != ""; page++ {
		if page == checkPages {
			partial = true
			break
		}
		var runs struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HTMLURL    string `json:"html_url"`
			} `json:"check_runs"`
		}
		link, err := c.get(ctx, next, &runs)
		if err != nil {
			partial = true
			break
		}
		read++
		for _, run := range runs.CheckRuns {
			state := "pending"
			if run.Status == "completed" {
				state = checkState(run.Conclusion)
			}
			checks = append(checks, Check{Name: run.Name, State: state, URL: run.HTMLURL})
		}
		next = nextPage(link)
	}
	var combined struct {
		Statuses []struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	link, err := c.get(ctx, commit+"/status?per_page=100", &combined)
	if err != nil {
		return checks, true, read > 0
	}
	for _, status := range combined.Statuses {
		checks = append(checks, Check{Name: status.Context, State: checkState(status.State), URL: status.TargetURL})
	}
	// The statuses are read as one page of a hundred.
	return checks, partial || nextPage(link) != "", true
}

// checkState is a check run's conclusion or a commit status's state as one of
// Check's: the ways of not passing are one failure, and a run GitHub gave up
// waiting for was canceled.
func checkState(state string) string {
	switch state {
	case "success", "pending", "skipped", "neutral":
		return state
	case gitHubCanceled:
		return "canceled"
	case "stale":
		return "canceled"
	case "":
		return "pending"
	}
	return "failure"
}

// reviewThreads are the review comments as the threads the page draws, by the
// review that opened each: a reply is its own review to this API, and belongs
// under the comment it answers.
func reviewThreads(comments []rawReviewComment) map[int64][]Thread {
	type place struct {
		review int64
		at     int
	}
	threads := map[int64][]Thread{}
	roots := map[int64]place{}
	for _, comment := range comments {
		said := ThreadComment{Author: comment.User.login(), At: comment.CreatedAt, Body: comment.Body, Reactions: comment.Reactions.counts()}
		if root, ok := roots[comment.InReplyTo]; ok && comment.InReplyTo != 0 {
			threads[root.review][root.at].Comments = append(threads[root.review][root.at].Comments, said)
			// A reply to a reply is in the thread of the first.
			roots[comment.ID] = root
			continue
		}
		thread := Thread{Path: comment.Path, Hunk: comment.DiffHunk, Comments: []ThreadComment{said}}
		switch {
		case comment.Line != nil:
			thread.Line = *comment.Line
		case comment.OriginalLine != nil:
			thread.Line, thread.Outdated = *comment.OriginalLine, true
		}
		review := comment.Review
		threads[review] = append(threads[review], thread)
		roots[comment.ID] = place{review, len(threads[review]) - 1}
	}
	return threads
}

// spokenReviews drops the reviews that say nothing themselves: a reply in a
// thread is a review with no body and no thread of its own, drawn where its
// thread is.
func spokenReviews(timeline []Event) []Event {
	kept := timeline[:0]
	for _, event := range timeline {
		if event.Kind == "reviewed" && event.Review == "commented" && strings.TrimSpace(event.Body) == "" && len(event.Threads) == 0 {
			continue
		}
		kept = append(kept, event)
	}
	return kept
}

// reviewers is the page's Reviewers list: everyone who reviewed, with what
// their last review said — a comment does not take back an approval — and
// everyone asked who has yet to answer. The author answering their own review
// threads is not a reviewer.
func reviewers(author string, timeline []Event, raw rawPull) []Reviewer {
	var out []Reviewer
	// By who they are, not by what they are called: a person and a team are
	// different namespaces, and one of each may share a name.
	type who struct {
		team bool
		name string
	}
	at := map[who]int{}
	set := func(team bool, name, state string, weak bool) {
		if name == "" {
			return
		}
		i, ok := at[who{team, name}]
		if !ok {
			at[who{team, name}] = len(out)
			out = append(out, Reviewer{Name: name, State: state})
			return
		}
		if !weak {
			out[i].State = state
		}
	}
	for _, event := range timeline {
		if event.Kind == "reviewed" && event.Actor != author {
			set(false, event.Actor, event.Review, event.Review == "commented")
		}
	}
	for _, user := range raw.RequestedReviewers {
		set(false, user.Login, "requested", false)
	}
	for _, team := range raw.RequestedTeams {
		set(true, team.Name, "requested", false)
	}
	return out
}

type rawPull struct {
	Draft     bool       `json:"draft"`
	Merged    bool       `json:"merged"`
	MergedAt  *time.Time `json:"merged_at"`
	MergedBy  *rawUser   `json:"merged_by"`
	Mergeable string     `json:"mergeable_state"`
	Head      rawBranch  `json:"head"`
	Base      rawBranch  `json:"base"`

	Commits        int `json:"commits"`
	Additions      int `json:"additions"`
	Deletions      int `json:"deletions"`
	ChangedFiles   int `json:"changed_files"`
	ReviewComments int `json:"review_comments"`

	RequestedReviewers []rawUser `json:"requested_reviewers"`
	RequestedTeams     []struct {
		Name string `json:"name"`
	} `json:"requested_teams"`
}

type rawBranch struct {
	Ref   string `json:"ref"`
	Label string `json:"label"`
	SHA   string `json:"sha"`
	Repo  *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

func (r rawPull) pull() Pull {
	pull := Pull{
		Draft: r.Draft, Merged: r.Merged, MergedBy: r.MergedBy.login(),
		Head: r.Head.Ref, Base: r.Base.Ref, HeadSHA: r.Head.SHA, BaseSHA: r.Base.SHA,
		Commits: r.Commits, Additions: r.Additions, Deletions: r.Deletions, ChangedFiles: r.ChangedFiles,
		Mergeable: r.Mergeable,
	}
	if r.MergedAt != nil {
		pull.MergedAt = *r.MergedAt
	}
	// A head in another repository — a fork, or one since deleted — is named
	// with its owner, as the page names it.
	if r.Head.Label != "" && (r.Head.Repo == nil || r.Base.Repo == nil || r.Head.Repo.FullName != r.Base.Repo.FullName) {
		pull.Head = r.Head.Label
	}
	return pull
}

type rawReviewComment struct {
	ID           int64        `json:"id"`
	Review       int64        `json:"pull_request_review_id"`
	InReplyTo    int64        `json:"in_reply_to_id"`
	Path         string       `json:"path"`
	Line         *int         `json:"line"`
	OriginalLine *int         `json:"original_line"`
	DiffHunk     string       `json:"diff_hunk"`
	User         *rawUser     `json:"user"`
	Body         string       `json:"body"`
	CreatedAt    time.Time    `json:"created_at"`
	Reactions    rawReactions `json:"reactions"`
}

type rawFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
}

// rawGitUser is who a commit says wrote it: a name, not an account.
type rawGitUser struct {
	Name string    `json:"name"`
	Date time.Time `json:"date"`
}

// firstLine is a commit message's subject.
func firstLine(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	return strings.TrimSpace(line)
}
