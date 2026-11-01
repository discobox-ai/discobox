package githubrepo

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
)

// searchPage is how many issues and PRs one page of a pass reads. Each brings
// its timeline and reviews, so a page stays a few points of GraphQL cost.
const searchPage = 25

const activityFields = `
  id number title url state createdAt updatedAt body
  author { login }`

// issueTimeline is an issue's timeline items as read; prTimeline adds the
// events only a PR has, which an issue's timeline cannot be asked for.
const issueTimeline = `
  pageInfo { hasNextPage endCursor }
  nodes {
    __typename
    ... on IssueComment { id createdAt url body author { login } }
    ... on ClosedEvent { id createdAt actor { login } }
    ... on ReopenedEvent { id createdAt actor { login } }
  }`

const prTimeline = `
  pageInfo { hasNextPage endCursor }
  nodes {
    __typename
    ... on IssueComment { id createdAt url body author { login } }
    ... on ClosedEvent { id createdAt actor { login } }
    ... on ReopenedEvent { id createdAt actor { login } }
    ... on MergedEvent { id createdAt actor { login } }
    ... on ReadyForReviewEvent { id createdAt actor { login } }
  }`

var searchQuery = `query($q: String!, $since: DateTime!, $after: String) {
  search(query: $q, type: ISSUE, first: ` + fmt.Sprint(searchPage) + `, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      __typename
      ... on Issue {` + activityFields + `
        timelineItems(since: $since, first: 100, itemTypes: [ISSUE_COMMENT, CLOSED_EVENT, REOPENED_EVENT]) {` + issueTimeline + `}
      }
      ... on PullRequest {` + activityFields + `
        headRefName
        timelineItems(since: $since, first: 100, itemTypes: [ISSUE_COMMENT, CLOSED_EVENT, REOPENED_EVENT, MERGED_EVENT, READY_FOR_REVIEW_EVENT]) {` + prTimeline + `}
        reviews(last: ` + fmt.Sprint(reviewPage) + `) {` + reviewFields + `}
      }
    }
  }
}`

// reviewPage is how many of a PR's latest reviews a page reads. A reply in a
// review thread is a review of its own, so a busy PR's are paged back
// through until one is older than the pass.
const reviewPage = 20

// reviewFields leaves out a review's inline comments, which are read only
// for the reviews a pass reports (reviewComments).
const reviewFields = `
  pageInfo { hasPreviousPage startCursor }
  nodes { id state body url submittedAt author { login } comments { totalCount } }`

var reviewsQuery = `query($id: ID!, $before: String) {
  node(id: $id) { ... on PullRequest { reviews(last: ` + fmt.Sprint(reviewPage) + `, before: $before) {` + reviewFields + `} } }
}`

const commentsQuery = `query($ids: [ID!]!) {
  nodes(ids: $ids) { ... on PullRequestReview { id comments(first: 20) { totalCount nodes { path line originalLine body } } } }
}`

var timelineQuery = `query($id: ID!, $since: DateTime!, $after: String) {
  node(id: $id) {
    ... on Issue { timelineItems(since: $since, first: 100, after: $after, itemTypes: [ISSUE_COMMENT, CLOSED_EVENT, REOPENED_EVENT]) {` + issueTimeline + `} }
    ... on PullRequest { timelineItems(since: $since, first: 100, after: $after, itemTypes: [ISSUE_COMMENT, CLOSED_EVENT, REOPENED_EVENT, MERGED_EVENT, READY_FOR_REVIEW_EVENT]) {` + prTimeline + `} }
  }
}`

type login struct {
	Login string `json:"login"`
}

func (l *login) name() string {
	if l == nil || l.Login == "" {
		return "someone"
	}
	return l.Login
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type timelineItem struct {
	Type      string    `json:"__typename"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	URL       string    `json:"url"`
	Body      string    `json:"body"`
	Author    *login    `json:"author"`
	Actor     *login    `json:"actor"`
}

type timeline struct {
	PageInfo pageInfo       `json:"pageInfo"`
	Nodes    []timelineItem `json:"nodes"`
}

type reviewComment struct {
	Path         string `json:"path"`
	Line         *int   `json:"line"`
	OriginalLine *int   `json:"originalLine"`
	Body         string `json:"body"`
}

type review struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	URL         string     `json:"url"`
	SubmittedAt *time.Time `json:"submittedAt"`
	Author      *login     `json:"author"`
	Comments    struct {
		TotalCount int             `json:"totalCount"`
		Nodes      []reviewComment `json:"nodes"`
	} `json:"comments"`
}

type activityNode struct {
	Type        string    `json:"__typename"`
	ID          string    `json:"id"`
	Number      int64     `json:"number"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Body        string    `json:"body"`
	Author      *login    `json:"author"`
	HeadRefName string    `json:"headRefName"`
	Timeline    timeline  `json:"timelineItems"`
	Reviews     reviews   `json:"reviews"`
}

type reviews struct {
	PageInfo struct {
		HasPreviousPage bool   `json:"hasPreviousPage"`
		StartCursor     string `json:"startCursor"`
	} `json:"pageInfo"`
	Nodes []review `json:"nodes"`
}

// reportable reports whether a review is one a pass reports: submitted after
// cutoff, and not reported yet.
func (p *poller) reportable(r review, cutoff time.Time) bool {
	if r.SubmittedAt == nil || r.State == "PENDING" || !r.SubmittedAt.After(cutoff) {
		return false
	}
	_, done := p.st.Reported[r.ID]
	return !done
}

func (n activityNode) isPR() bool { return n.Type == "PullRequest" }

// walkActivity is a pass over the issues and PRs updated since the last one,
// ascending by update, a page at a time, each saved as it is done: a pass
// cut short by a rate limit resumes at the page it reached.
func (p *poller) walkActivity(ctx context.Context, until time.Time) error {
	if p.st.Walk == nil {
		p.st.Walk = &walk{From: p.st.cutoff(), Until: until}
	}
	cutoff := p.st.cutoff()
	for {
		w := p.st.Walk
		vars := map[string]any{"q": p.searchString(w.From), "since": cutoff.UTC().Format(time.RFC3339)}
		if w.After != "" {
			vars["after"] = w.After
		}
		var out struct {
			Search struct {
				PageInfo pageInfo       `json:"pageInfo"`
				Nodes    []activityNode `json:"nodes"`
			} `json:"search"`
		}
		if err := p.c.graphql(ctx, searchQuery, vars, &out); err != nil {
			return err
		}
		nodes := out.Search.Nodes
		var events []timedEvent
		for i := range nodes {
			if nodes[i].Timeline.PageInfo.HasNextPage {
				if err := p.moreTimeline(ctx, &nodes[i], cutoff); err != nil {
					return err
				}
			}
			if err := p.moreReviews(ctx, &nodes[i], cutoff); err != nil {
				return err
			}
		}
		if err := p.reviewComments(ctx, nodes, cutoff); err != nil {
			return err
		}
		for i := range nodes {
			events = append(events, p.activity(nodes[i], cutoff)...)
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
		for _, ev := range events {
			if err := p.emit(ev.Event); err != nil {
				return err
			}
			p.st.Reported[ev.id] = ev.at
		}
		if !out.Search.PageInfo.HasNextPage || len(nodes) == 0 {
			break
		}
		// The next page starts at this one's last update. A page whose items
		// all share one second goes on by cursor instead, so a pass cannot
		// stall on them; the overlap's items are already reported.
		if last := nodes[len(nodes)-1].UpdatedAt; last.After(w.From) {
			w.From, w.After = last, ""
		} else {
			w.After = out.Search.PageInfo.EndCursor
		}
		if err := p.save(); err != nil {
			return err
		}
	}
	p.st.Since = p.st.Walk.Until
	p.st.Walk = nil
	p.st.pruneReported()
	return p.save()
}

// searchString is the search for what this instance watches, updated from
// from on, oldest first.
func (p *poller) searchString(from time.Time) string {
	q := []string{"repo:" + p.cfg.repo(), "updated:>=" + from.UTC().Format(time.RFC3339), "sort:updated-asc"}
	switch {
	case p.cfg.Issues && !p.cfg.PRs:
		q = append(q, "is:issue")
	case p.cfg.PRs && !p.cfg.Issues:
		q = append(q, "is:pr")
		if p.cfg.Author != "" && len(p.cfg.PRIDs) == 0 {
			q = append(q, "author:"+p.cfg.Author)
		}
	}
	return strings.Join(q, " ")
}

// moreTimeline reads the rest of a node's timeline past its first page.
func (p *poller) moreTimeline(ctx context.Context, n *activityNode, cutoff time.Time) error {
	after := n.Timeline.PageInfo.EndCursor
	for after != "" {
		var out struct {
			Node struct {
				Timeline timeline `json:"timelineItems"`
			} `json:"node"`
		}
		vars := map[string]any{"id": n.ID, "since": cutoff.UTC().Format(time.RFC3339), "after": after}
		if err := p.c.graphql(ctx, timelineQuery, vars, &out); err != nil {
			return err
		}
		n.Timeline.Nodes = append(n.Timeline.Nodes, out.Node.Timeline.Nodes...)
		after = ""
		if out.Node.Timeline.PageInfo.HasNextPage {
			after = out.Node.Timeline.PageInfo.EndCursor
		}
	}
	return nil
}

// moreReviews pages back through a PR's reviews while the oldest read is
// still one the pass reports.
func (p *poller) moreReviews(ctx context.Context, n *activityNode, cutoff time.Time) error {
	for n.isPR() && n.Reviews.PageInfo.HasPreviousPage && len(n.Reviews.Nodes) > 0 {
		if oldest := n.Reviews.Nodes[0].SubmittedAt; oldest != nil && !oldest.After(cutoff) {
			return nil
		}
		var out struct {
			Node struct {
				Reviews reviews `json:"reviews"`
			} `json:"node"`
		}
		vars := map[string]any{"id": n.ID, "before": n.Reviews.PageInfo.StartCursor}
		if err := p.c.graphql(ctx, reviewsQuery, vars, &out); err != nil {
			return err
		}
		n.Reviews.Nodes = append(out.Node.Reviews.Nodes, n.Reviews.Nodes...)
		n.Reviews.PageInfo = out.Node.Reviews.PageInfo
	}
	return nil
}

// reviewComments reads the inline comments of the reviews a page reports,
// in one query.
func (p *poller) reviewComments(ctx context.Context, nodes []activityNode, cutoff time.Time) error {
	byID := map[string]*review{}
	var ids []string
	for i := range nodes {
		for j := range nodes[i].Reviews.Nodes {
			r := &nodes[i].Reviews.Nodes[j]
			if r.Comments.TotalCount > 0 && p.reportable(*r, cutoff) {
				byID[r.ID] = r
				ids = append(ids, r.ID)
			}
		}
	}
	for chunk := range slices.Chunk(ids, 50) {
		var out struct {
			Nodes []*struct {
				ID       string `json:"id"`
				Comments struct {
					TotalCount int             `json:"totalCount"`
					Nodes      []reviewComment `json:"nodes"`
				} `json:"comments"`
			} `json:"nodes"`
		}
		if err := p.c.graphql(ctx, commentsQuery, map[string]any{"ids": chunk}, &out); err != nil {
			return err
		}
		for _, node := range out.Nodes {
			if r := byID[node.ID]; node != nil && r != nil {
				r.Comments.TotalCount, r.Comments.Nodes = node.Comments.TotalCount, node.Comments.Nodes
			}
		}
	}
	return nil
}

// timedEvent is an event, with the activity it reports and when.
type timedEvent struct {
	monitors.Event
	id string
	at time.Time
}

// activity is what one issue or PR reports: its opening, comments, state
// changes and reviews after cutoff that are not reported yet. A PR selected
// by head and author is also watched from here on.
func (p *poller) activity(n activityNode, cutoff time.Time) []timedEvent {
	if n.isPR() {
		if p.cfg.selects(n.HeadRefName, n.Author.name()) && n.State == "OPEN" {
			p.watchPR(n.Number, false)
		}
		if !p.cfg.wantsPR(n.Number, n.HeadRefName, n.Author.name()) {
			return nil
		}
	} else if !p.cfg.wantsIssue(n.Number) {
		return nil
	}
	kind, keyPrefix := "issue", "issue-"
	if n.isPR() {
		kind, keyPrefix = "pr", "pr-"
	}
	var out []timedEvent
	add := func(id string, at time.Time, ev monitors.Event) {
		if !at.After(cutoff) {
			return
		}
		if _, done := p.st.Reported[id]; done {
			return
		}
		out = append(out, timedEvent{Event: ev, id: id, at: at})
	}
	add("opened:"+n.ID, n.CreatedAt, monitors.Event{
		Kind:  kind + ".opened",
		Title: fmt.Sprintf("#%d opened by %s: %s", n.Number, n.Author.name(), n.Title),
		URL:   n.URL,
		Body:  n.Body,
	})
	// A merge is also a close, which says nothing more and would replace the
	// merge under their shared key.
	merged := false
	for _, item := range n.Timeline.Nodes {
		if item.Type == "MergedEvent" {
			merged = true
		}
	}
	for _, item := range n.Timeline.Nodes {
		switch item.Type {
		case "IssueComment":
			add(item.ID, item.CreatedAt, monitors.Event{
				Kind:  kind + ".comment",
				Title: fmt.Sprintf("%s commented on #%d: %s", item.Author.name(), n.Number, firstLine(item.Body, 120)),
				URL:   item.URL,
				Body:  item.Body,
			})
		case "ClosedEvent", "ReopenedEvent", "MergedEvent", "ReadyForReviewEvent":
			if item.Type == "ClosedEvent" && merged {
				continue
			}
			event := map[string]string{
				"ClosedEvent":         "closed",
				"ReopenedEvent":       "reopened",
				"MergedEvent":         "merged",
				"ReadyForReviewEvent": "ready_for_review",
			}[item.Type]
			add(item.ID, item.CreatedAt, monitors.Event{
				Kind:  kind + "." + event,
				Key:   fmt.Sprintf("%s%d-state", keyPrefix, n.Number),
				Title: fmt.Sprintf("#%d %s by %s: %s", n.Number, strings.ReplaceAll(event, "_", " "), item.Actor.name(), n.Title),
				URL:   n.URL,
			})
		}
	}
	for _, r := range n.Reviews.Nodes {
		if !p.reportable(r, cutoff) {
			continue
		}
		add(r.ID, *r.SubmittedAt, monitors.Event{
			Kind:    "pr.review",
			Title:   fmt.Sprintf("%s %s #%d", r.Author.name(), reviewVerb(r.State), n.Number),
			Summary: firstLine(r.Body, 160),
			URL:     r.URL,
			Body:    reviewBody(r),
		})
	}
	return out
}

func reviewVerb(state string) string {
	switch state {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "requested changes on"
	case "DISMISSED":
		return "dismissed a review on"
	}
	return "reviewed"
}

// reviewBody is a review's text and its inline comments, each at its line.
func reviewBody(r review) string {
	parts := []string{}
	if body := strings.TrimSpace(r.Body); body != "" {
		parts = append(parts, body)
	}
	var inline []string
	for _, c := range r.Comments.Nodes {
		line := "-"
		switch {
		case c.Line != nil:
			line = fmt.Sprint(*c.Line)
		case c.OriginalLine != nil:
			line = fmt.Sprint(*c.OriginalLine)
		}
		inline = append(inline, fmt.Sprintf("%s:%s: %s", c.Path, line, c.Body))
	}
	if more := r.Comments.TotalCount - len(r.Comments.Nodes); more > 0 {
		inline = append(inline, fmt.Sprintf("(%d more inline comments: %s)", more, r.URL))
	}
	if len(inline) > 0 {
		parts = append(parts, strings.Join(inline, "\n\n"))
	}
	return strings.Join(parts, "\n\n")
}
