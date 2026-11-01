package githubrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
)

const (
	// maxCheckProbes bounds the PRs whose head checks are probed one by one,
	// two requests each; past it, a check finishing is seen when an Actions
	// run or the PR does.
	maxCheckProbes = 20
	// maxRecheck is how many runs in a row ask again for a mergeable state
	// GitHub has not computed yet, before waiting for the next change.
	maxRecheck = 5
	// watchedPerQuery is how many PRs one watched query reads.
	watchedPerQuery = 25
	// openPages bounds the open PRs read to select by head and author.
	openPages = 5
)

const contextFields = `
  totalCount
  pageInfo { hasNextPage endCursor }
  nodes {
    __typename
    ... on CheckRun { name status conclusion }
    ... on StatusContext { context state }
  }`

const contextsQuery = `query($id: ID!, $after: String) {
  node(id: $id) { ... on Commit { statusCheckRollup { contexts(first: 100, after: $after) {` + contextFields + `} } } }
}`

var watchedFragment = `
fragment watched on PullRequest {
  number title url state headRefName headRefOid mergeable
  author { login }
  commits(last: 1) {
    nodes { commit { id oid statusCheckRollup {
      state
      contexts(first: 50) {` + contextFields + `}
    } } }
  }
}`

type checkContext struct {
	Type       string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

func (c checkContext) label() string {
	if c.Type == "StatusContext" {
		return c.Context
	}
	return c.Name
}

// result is the context's outcome, lowercase: its conclusion, its status
// while it runs, or a commit status's state.
func (c checkContext) result() string {
	switch {
	case c.Type == "StatusContext":
		return strings.ToLower(c.State)
	case c.Conclusion != "":
		return strings.ToLower(c.Conclusion)
	}
	return strings.ToLower(c.Status)
}

func (c checkContext) pending() bool {
	if c.Type == "StatusContext" {
		return c.State == "PENDING" || c.State == "EXPECTED"
	}
	return c.Status != "COMPLETED"
}

func (c checkContext) failed() bool {
	if c.Type == "StatusContext" {
		return c.State == "FAILURE" || c.State == "ERROR"
	}
	switch c.Conclusion {
	//nolint:misspell // GitHub's CheckConclusionState spells it so.
	case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE":
		return true
	}
	return false
}

type watchedPR struct {
	Number      int64  `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	Mergeable   string `json:"mergeable"`
	Author      *login `json:"author"`
	Commits     struct {
		Nodes []headCommit `json:"nodes"`
	} `json:"commits"`
}

type headCommit struct {
	Commit struct {
		ID     string       `json:"id"`
		Rollup *checkRollup `json:"statusCheckRollup"`
	} `json:"commit"`
}

type checkRollup struct {
	State    string   `json:"state"`
	Contexts contexts `json:"contexts"`
}

type contexts struct {
	TotalCount int            `json:"totalCount"`
	PageInfo   pageInfo       `json:"pageInfo"`
	Nodes      []checkContext `json:"nodes"`
}

// conclusion is the head's checks reduced to one word: pending while any
// runs, else failure if any failed, else success; none when nothing reports
// on it. It is read from the checks themselves, every page of them
// (moreContexts): the rollup turns FAILURE as soon as one fails, with others
// still running, and a failure reported then would name only some of them.
func (w watchedPR) conclusion() (string, []checkContext) {
	if len(w.Commits.Nodes) == 0 || w.Commits.Nodes[0].Commit.Rollup == nil {
		return "none", nil
	}
	contexts := w.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes
	failed := false
	for _, c := range contexts {
		if c.pending() {
			return "pending", contexts
		}
		failed = failed || c.failed()
	}
	switch {
	case len(contexts) == 0:
		return "none", nil
	case failed:
		return "failure", contexts
	}
	return "success", contexts
}

// moreContexts reads the rest of a PR's head checks past the first page.
func (p *poller) moreContexts(ctx context.Context, pr *watchedPR) error {
	if len(pr.Commits.Nodes) == 0 || pr.Commits.Nodes[0].Commit.Rollup == nil {
		return nil
	}
	commit := &pr.Commits.Nodes[0].Commit
	list := &commit.Rollup.Contexts
	for list.PageInfo.HasNextPage {
		var out struct {
			Node struct {
				Rollup struct {
					Contexts contexts `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"node"`
		}
		vars := map[string]any{"id": commit.ID, "after": list.PageInfo.EndCursor}
		if err := p.c.graphql(ctx, contextsQuery, vars, &out); err != nil {
			return err
		}
		list.Nodes = append(list.Nodes, out.Node.Rollup.Contexts.Nodes...)
		list.PageInfo = out.Node.Rollup.Contexts.PageInfo
	}
	return nil
}

// watchPR starts following a PR's checks and mergeability. A quiet one was
// already there: what it is when first read is recorded, not reported.
func (p *poller) watchPR(n int64, quiet bool) {
	if _, ok := p.st.PRs[key(n)]; ok {
		return
	}
	p.st.PRs[key(n)] = &prState{Quiet: quiet}
}

// selectOpenPRs watches the open PRs head and author select, at the
// baseline. Those opened later are found by the activity pass.
func (p *poller) selectOpenPRs(ctx context.Context) error {
	const query = `query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: 100, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { number headRefName author { login } }
    }
  }
}`
	vars := map[string]any{"owner": p.cfg.Owner, "name": p.cfg.Name}
	for range openPages {
		var out struct {
			Repository struct {
				PullRequests struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []struct {
						Number      int64  `json:"number"`
						HeadRefName string `json:"headRefName"`
						Author      *login `json:"author"`
					} `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		if err := p.c.graphql(ctx, query, vars, &out); err != nil {
			return err
		}
		for _, pr := range out.Repository.PullRequests.Nodes {
			if p.cfg.selects(pr.HeadRefName, pr.Author.name()) {
				p.watchPR(pr.Number, true)
			}
		}
		if !out.Repository.PullRequests.PageInfo.HasNextPage {
			break
		}
		vars["after"] = out.Repository.PullRequests.PageInfo.EndCursor
	}
	return nil
}

// checkProbe is a watched PR's head checks seen to change: the ETags that
// are saved once the change is reported, for the head they were probed at.
type checkProbe struct{ sha, checks, status string }

// retarget follows the PRs and issues the options name now: one newly
// named starts quiet, as at a baseline, and one no longer named or selected
// is let go. It reports whether any target is new, to be read this run.
func (p *poller) retarget() bool {
	fresh := false
	if p.cfg.PRs {
		for _, n := range p.cfg.PRIDs {
			if p.st.PRs[key(n)] == nil {
				p.watchPR(n, true)
				fresh = true
			}
		}
	}
	for n, pr := range p.st.PRs {
		number, _ := strconv.ParseInt(n, 10, 64)
		named := p.cfg.PRs && slices.Contains(p.cfg.PRIDs, number)
		// Head is empty only until the PR is first read.
		selected := p.cfg.PRs && (pr.Head == "" || p.cfg.selects(pr.Head, pr.Author))
		if !named && !selected {
			delete(p.st.PRs, n)
		}
	}
	if p.cfg.Issues {
		for _, n := range p.cfg.IssueIDs {
			if _, ok := p.st.Issues[key(n)]; !ok {
				fresh = true
			}
		}
	}
	return fresh
}

// probeChecks asks, conditionally, whether each open watched PR's head
// checks changed: its check runs, and its commit statuses, which CI that
// reports statuses rather than checks sets.
func (p *poller) probeChecks(ctx context.Context, probes map[string]checkProbe) (bool, error) {
	numbers := make([]string, 0, len(p.st.PRs))
	for n, pr := range p.st.PRs {
		if pr.HeadSHA != "" && pr.State == "OPEN" {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	changed := false
	for i, n := range numbers {
		if i == maxCheckProbes {
			break
		}
		pr := p.st.PRs[n]
		checks, err := p.c.get(ctx, p.repoPath("/commits/"+pr.HeadSHA+"/check-runs?per_page=100"), pr.ChecksETag)
		if err != nil {
			return changed, err
		}
		status, err := p.c.get(ctx, p.repoPath("/commits/"+pr.HeadSHA+"/status?per_page=100"), pr.StatusETag)
		if err != nil {
			return changed, err
		}
		if !checks.NotModified || !status.NotModified {
			probes[n] = checkProbe{sha: pr.HeadSHA, checks: checks.ETag, status: status.ETag}
			changed = true
		}
	}
	return changed, nil
}

// queryWatched reads the named issues and the watched PRs, and reports each
// PR's checks reaching a conclusion and its conflicts with its base, unless
// this is the baseline.
func (p *poller) queryWatched(ctx context.Context, report bool) error {
	var aliases []string
	if p.cfg.Issues {
		for _, n := range p.cfg.IssueIDs {
			aliases = append(aliases, fmt.Sprintf("i%d: issueOrPullRequest(number: %d) { ... on Issue { state } ... on PullRequest { state } }", n, n))
		}
	}
	numbers := make([]string, 0, len(p.st.PRs))
	for n := range p.st.PRs {
		numbers = append(numbers, n)
	}
	slices.Sort(numbers)
	for _, n := range numbers {
		aliases = append(aliases, fmt.Sprintf("p%s: pullRequest(number: %s) { ...watched }", n, n))
	}
	unknown := 0
	for chunk := range slices.Chunk(aliases, watchedPerQuery) {
		query := "query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n    " +
			strings.Join(chunk, "\n    ") + "\n  }\n}\n" + watchedFragment
		var out struct {
			Repository map[string]json.RawMessage `json:"repository"`
		}
		if err := p.c.graphql(ctx, query, map[string]any{"owner": p.cfg.Owner, "name": p.cfg.Name}, &out); err != nil {
			return err
		}
		for alias, raw := range out.Repository {
			if string(raw) == "null" {
				continue
			}
			switch alias[0] {
			case 'i':
				var item struct {
					State string `json:"state"`
				}
				if err := json.Unmarshal(raw, &item); err != nil {
					return err
				}
				p.st.Issues[alias[1:]] = item.State
			case 'p':
				var pr watchedPR
				if err := json.Unmarshal(raw, &pr); err != nil {
					return err
				}
				if pr.State == "OPEN" {
					if err := p.moreContexts(ctx, &pr); err != nil {
						return err
					}
				}
				pending, err := p.updatePR(pr, report)
				if err != nil {
					return err
				}
				if pending {
					unknown++
				}
			}
		}
	}
	if unknown > 0 {
		p.st.Recheck++
	} else {
		p.st.Recheck = 0
	}
	return nil
}

// updatePR records what a watched PR is now, reporting what changed. It
// reports true while GitHub has yet to compute whether it conflicts.
func (p *poller) updatePR(pr watchedPR, report bool) (unknown bool, err error) {
	n := key(pr.Number)
	st := p.st.PRs[n]
	if st == nil {
		return false, nil
	}
	st.State, st.Head, st.Author = pr.State, pr.HeadRefName, pr.Author.name()
	if pr.HeadRefOid != st.HeadSHA {
		st.HeadSHA, st.ChecksETag, st.StatusETag = pr.HeadRefOid, "", ""
	}
	if pr.State != "OPEN" {
		// Closed or merged: nothing more to follow, and one watched only
		// because head and author selected it is no longer watched.
		if !slices.Contains(p.cfg.PRIDs, pr.Number) {
			delete(p.st.PRs, n)
		}
		return false, nil
	}

	conclusion, contexts := pr.conclusion()
	quietChecks := st.Quiet && st.ChecksSHA == ""
	if (conclusion != st.Checks || pr.HeadRefOid != st.ChecksSHA) && report && !quietChecks &&
		(conclusion == "success" || conclusion == "failure") {
		if err := p.emit(checksEvent(pr, conclusion, contexts)); err != nil {
			return false, err
		}
	}
	st.Checks, st.ChecksSHA = conclusion, pr.HeadRefOid

	switch pr.Mergeable {
	case "MERGEABLE", "CONFLICTING":
		was := st.Mergeable
		st.Mergeable = pr.Mergeable
		if !report || (st.Quiet && was == "") || was == pr.Mergeable {
			return false, nil
		}
		if pr.Mergeable == "CONFLICTING" {
			return false, p.emit(monitors.Event{
				Kind:  "pr.conflict",
				Key:   "pr-" + n + "-mergeable",
				Title: fmt.Sprintf("#%d conflicts with its base: %s", pr.Number, pr.Title),
				URL:   pr.URL,
			})
		}
		if was == "CONFLICTING" {
			return false, p.emit(monitors.Event{
				Kind:  "pr.conflict",
				Key:   "pr-" + n + "-mergeable",
				Title: fmt.Sprintf("#%d no longer conflicts with its base: %s", pr.Number, pr.Title),
				URL:   pr.URL,
			})
		}
		return false, nil
	}
	// UNKNOWN: GitHub computes it lazily, once asked. Not a change.
	return true, nil
}

func checksEvent(pr watchedPR, conclusion string, contexts []checkContext) monitors.Event {
	sha := pr.HeadRefOid
	if len(sha) > 7 {
		sha = sha[:7]
	}
	var failed, lines []string
	for _, c := range contexts {
		lines = append(lines, c.label()+": "+c.result())
		if c.failed() {
			failed = append(failed, c.label())
		}
	}
	title := fmt.Sprintf("#%d checks passed on %s", pr.Number, sha)
	if conclusion == "failure" {
		title = fmt.Sprintf("#%d checks failed on %s: %s", pr.Number, sha, strings.Join(failed, ", "))
	}
	return monitors.Event{
		Kind:    "pr.checks",
		Key:     "pr-" + key(pr.Number) + "-checks",
		Title:   title,
		Summary: pr.Title,
		URL:     pr.URL + "/checks",
		Body:    strings.Join(lines, "\n"),
	}
}
