// Package github reads an issue the way its page on github.com shows it — the
// issue, its sub-issues, and its timeline of comments and events — posts a
// comment on one, and closes or reopens it. It is the console's issue tab's
// whole reach into GitHub.
//
// It talks to the REST API as the person at this machine: the token is the
// one `gh` is logged in with, or GH_TOKEN / GITHUB_TOKEN, the variables `gh`
// itself reads first. With none, reading a public issue still works,
// anonymously; commenting says how to log in.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is the REST API github.com answers on.
const DefaultAPI = "https://api.github.com"

// AnonymousRefresh is how often an issue is read again with no token. GitHub
// allows 60 requests an hour without one, and a re-read is at least one.
const AnonymousRefresh = 5 * time.Minute

// keptFor is how long a full read is trusted on the issue's updated_at alone.
// Not everything moves it — a reaction does not — so past this a re-read is a
// full one.
const keptFor = 10 * time.Minute

// tokenRetry is how long an empty token is believed before `gh` is asked
// again: a `gh auth login` in another terminal is how one is supplied.
const tokenRetry = time.Minute

// timelinePages bounds how much of a timeline is read: 100 items a page, so an
// issue has to be very long indeed to reach it, and a runaway one stops rather
// than reading forever.
const timelinePages = 30

// Client reads and comments on issues. The zero value is not usable; see New.
type Client struct {
	// API is the REST API's base URL, without a trailing slash.
	API  string
	HTTP *http.Client
	// Token finds the token to send, and is asked again after a refusal: a
	// `gh auth login` in another terminal is how a refusal is answered.
	Token func(context.Context) string

	mu    sync.Mutex
	token string
	// asked is when Token last came back empty: with no token, asking on
	// every request would start a `gh` per request.
	asked time.Time
	// read is each issue as it was last read in full, so a re-read of one
	// nothing has happened to costs one request rather than the timeline.
	read map[issueKey]cachedIssue
	// now is the clock the two above are kept by.
	now func() time.Time
}

type issueKey struct {
	owner, name string
	number      int
}

// cachedIssue is a full read: when the issue said it was last updated, when
// it was last asked about (at), and when it was last read in full.
type cachedIssue struct {
	issue   Issue
	updated time.Time
	at      time.Time
	full    time.Time
}

// New is a client for github.com as this machine's `gh` is logged in to it.
func New() *Client {
	return &Client{API: DefaultAPI, HTTP: &http.Client{Timeout: 30 * time.Second}, Token: LocalToken, now: time.Now}
}

// LocalToken is the token this machine would send to GitHub: GH_TOKEN, then
// GITHUB_TOKEN, then what `gh auth token` prints. Empty when there is none,
// which reads public repositories anonymously.
func LocalToken(ctx context.Context) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Repository is the owner and name a repository's web address names:
// https://github.com/discobox-ai/discobox is discobox-ai and discobox.
func Repository(webURL string) (owner, name string, ok bool) {
	parsed, err := url.Parse(webURL)
	if err != nil || !strings.EqualFold(parsed.Host, "github.com") {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

// Issue is one issue as its page shows it.
type Issue struct {
	// Repository is owner/name.
	Repository string
	Number     int
	URL        string
	Title      string
	// State is "open" or "closed"; StateReason says how it closed — completed,
	// not_planned, duplicate — and is empty for an open one.
	State       string
	StateReason string
	// Type is the issue's type, where the organization uses them: Bug,
	// Feature, Task.
	Type      string
	Author    string
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
	ClosedAt  time.Time
	ClosedBy  string
	Labels    []Label
	Assignees []string
	Milestone string
	Locked    bool
	// Comments is how many comments the issue has, whether or not the
	// timeline was read as far as all of them.
	Comments  int
	Reactions Reactions
	// PullRequest is set when the number is a pull request's: every pull
	// request is an issue to this API, and its page is a different one.
	PullRequest bool
	// SubIssues are the issues this one is broken into, and SubIssuesDone
	// how many of them are closed.
	SubIssues     []Ref
	SubIssuesDone int
	// Timeline is everything that happened after it was opened, oldest first:
	// the comments and the events between them.
	Timeline []Event
	// Truncated is set when the timeline was longer than was read.
	Truncated bool
}

// Label is one label, with the color its page draws it in.
type Label struct {
	Name string
	// Color is six hex digits, without the #.
	Color string
}

// Ref is another issue or pull request, as a timeline event or a sub-issue
// list names it.
type Ref struct {
	// Repository is owner/name, which is this issue's own for most.
	Repository  string
	Number      int
	Title       string
	State       string
	URL         string
	PullRequest bool
	// Merged is a pull request's state beyond closed.
	Merged bool
}

// Reactions are the counts under a comment, keyed by GitHub's names: +1, -1,
// laugh, hooray, confused, heart, rocket, eyes.
type Reactions map[string]int

// Event is one item of the timeline. Kind is GitHub's own event name —
// "commented", "labeled", "closed", "cross-referenced" — so a kind nothing
// here describes still says what it was.
type Event struct {
	Kind  string
	Actor string
	At    time.Time
	// Body and Reactions are a comment's, and URL is its permalink.
	Body      string
	Reactions Reactions
	URL       string
	// Label is what a labeled or unlabeled event moved.
	Label *Label
	// Subject is who an assignment was about, the milestone a milestoning
	// moved, the reason a lock gave, or the commit a reference or a close came
	// from.
	Subject string
	// From and To are a rename's titles.
	From, To string
	// StateReason is how a close closed it.
	StateReason string
	// Source is the issue or pull request a cross-reference came from.
	Source *Ref
}

// Error is an answer GitHub refused with, carrying its status and message.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub answered %d", e.Status)
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

// ErrNoToken is commenting with nothing to comment as.
var ErrNoToken = errors.New("not logged in to GitHub: run gh auth login, or set GH_TOKEN")

// Issue reads one issue and everything its page shows: the issue itself, its
// sub-issues, and the whole timeline.
//
// A re-read is cheap when nothing happened: the issue is read first, and when
// GitHub says it has not been updated since a full read less than keptFor ago,
// that read is the answer. With no token an issue read less than
// AnonymousRefresh ago is not read again at all. full asks for a full read
// whatever was kept — someone asked to see it now — and a comment or a change
// of state made here drops what was kept, so the next read is full anyway.
func (c *Client) Issue(ctx context.Context, owner, name string, number int, full bool) (Issue, error) {
	key := issueKey{owner, name, number}
	cached, ok := c.cached(key)
	if full || c.clock().Sub(cached.full) >= keptFor {
		ok = false
	}
	if ok && c.bearer(ctx) == "" && c.clock().Sub(cached.at) < AnonymousRefresh {
		return cached.issue, nil
	}
	base := fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(name), number)
	var raw rawIssue
	if _, err := c.get(ctx, base, &raw); err != nil {
		return Issue{}, err
	}
	if ok && raw.UpdatedAt.Equal(cached.updated) {
		cached.at = c.clock()
		c.keep(key, cached)
		return cached.issue, nil
	}
	issue, err := c.readIssue(ctx, base, owner, name, raw)
	if err != nil {
		return Issue{}, err
	}
	now := c.clock()
	c.keep(key, cachedIssue{issue: issue, updated: raw.UpdatedAt, at: now, full: now})
	return issue, nil
}

func (c *Client) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *Client) cached(key issueKey) (cachedIssue, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.read[key]
	return cached, ok
}

func (c *Client) keep(key issueKey, cached cachedIssue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read == nil {
		c.read = map[issueKey]cachedIssue{}
	}
	c.read[key] = cached
}

func (c *Client) forget(key issueKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.read, key)
}

// readIssue is the full read: the sub-issues and every page of the timeline.
func (c *Client) readIssue(ctx context.Context, base, owner, name string, raw rawIssue) (Issue, error) {
	issue := raw.issue()
	issue.Repository = owner + "/" + name

	if raw.SubIssuesSummary.Total > 0 {
		// The list is not the issue: a page that fails leaves what was read
		// and the summary's count standing rather than the whole page
		// unreadable.
		next := base + "/sub_issues?per_page=100"
		for page := 0; next != "" && page < timelinePages; page++ {
			var subs []rawIssue
			link, err := c.get(ctx, next, &subs)
			if err != nil {
				break
			}
			for _, sub := range subs {
				issue.SubIssues = append(issue.SubIssues, sub.ref(issue.Repository))
			}
			next = nextPage(link)
		}
		issue.SubIssuesDone = raw.SubIssuesSummary.Completed
	}

	next := base + "/timeline?per_page=100"
	for page := 0; next != ""; page++ {
		if page == timelinePages {
			issue.Truncated = true
			break
		}
		var items []rawEvent
		link, err := c.get(ctx, next, &items)
		if err != nil {
			return Issue{}, err
		}
		for _, item := range items {
			if event, ok := item.event(issue.Repository); ok {
				issue.Timeline = append(issue.Timeline, event)
			}
		}
		next = nextPage(link)
	}
	return issue, nil
}

// Comment posts a comment on an issue, as whoever the token is.
func (c *Client) Comment(ctx context.Context, owner, name string, number int, body string) error {
	if c.bearer(ctx) == "" {
		return ErrNoToken
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", url.PathEscape(owner), url.PathEscape(name), number)
	c.forget(issueKey{owner, name, number})
	_, err = c.do(ctx, http.MethodPost, path, payload, nil)
	return err
}

// SetState closes an issue or reopens it, as whoever the token is: state is
// "closed" or "open", and reason how it closed — completed, not_planned — or
// empty to leave it to GitHub.
func (c *Client) SetState(ctx context.Context, owner, name string, number int, state, reason string) error {
	if c.bearer(ctx) == "" {
		return ErrNoToken
	}
	fields := map[string]string{"state": state}
	if reason != "" {
		fields["state_reason"] = reason
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(name), number)
	c.forget(issueKey{owner, name, number})
	_, err = c.do(ctx, http.MethodPatch, path, payload, nil)
	return err
}

func (c *Client) get(ctx context.Context, path string, into any) (string, error) {
	return c.do(ctx, http.MethodGet, path, nil, into)
}

// do sends one request and decodes the answer into into, returning its Link
// header. A path that is already a whole URL — the next page a Link named —
// is sent as it is.
func (c *Client) do(ctx context.Context, method, path string, body []byte, into any) (string, error) {
	target := path
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		target = c.API + path
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := c.bearer(ctx); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized {
			// A token refused is asked for again next time: it has been
			// revoked, or replaced by a login since.
			c.mu.Lock()
			c.token, c.asked = "", time.Time{}
			c.mu.Unlock()
		}
		return "", refusal(resp)
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			return "", fmt.Errorf("reading GitHub's answer: %w", err)
		}
	}
	return resp.Header.Get("Link"), nil
}

// bearer is the token, found once and kept until GitHub refuses it. No token
// is believed for tokenRetry before it is asked for again.
func (c *Client) bearer(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" && c.Token != nil && (c.asked.IsZero() || c.clock().Sub(c.asked) >= tokenRetry) {
		c.token = c.Token(ctx)
		c.asked = c.clock()
	}
	return c.token
}

// refusal reads GitHub's error message out of a refused answer, and says what
// a 404 from a private repository usually means: GitHub answers "not found"
// rather than "forbidden" to someone who may not know it exists.
func refusal(resp *http.Response) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&payload)
	err := &Error{Status: resp.StatusCode, Message: payload.Message}
	if resp.StatusCode == http.StatusNotFound && resp.Request != nil && resp.Request.Header.Get("Authorization") == "" {
		err.Message = strings.TrimSpace(err.Message + " — a private repository needs gh auth login, or GH_TOKEN")
	}
	return err
}

// nextPage is the rel="next" URL of a Link header, or empty on the last page.
func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, ok := strings.Cut(part, ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(target), "<>")
	}
	return ""
}

type rawUser struct {
	Login string `json:"login"`
}

func (u *rawUser) login() string {
	if u == nil {
		return ""
	}
	return u.Login
}

type rawLabel struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type rawIssue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	State       string     `json:"state"`
	StateReason string     `json:"state_reason"`
	HTMLURL     string     `json:"html_url"`
	User        *rawUser   `json:"user"`
	Body        string     `json:"body"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	ClosedBy    *rawUser   `json:"closed_by"`
	Labels      []rawLabel `json:"labels"`
	Assignees   []rawUser  `json:"assignees"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Type *struct {
		Name string `json:"name"`
	} `json:"type"`
	Locked      bool         `json:"locked"`
	Comments    int          `json:"comments"`
	Reactions   rawReactions `json:"reactions"`
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	SubIssuesSummary struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
	} `json:"sub_issues_summary"`
}

func (r rawIssue) issue() Issue {
	issue := Issue{
		Number:      r.Number,
		URL:         r.HTMLURL,
		Title:       r.Title,
		State:       r.State,
		StateReason: r.StateReason,
		Author:      r.User.login(),
		Body:        r.Body,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
		ClosedBy:    r.ClosedBy.login(),
		Locked:      r.Locked,
		Comments:    r.Comments,
		Reactions:   r.Reactions.counts(),
		PullRequest: r.PullRequest != nil,
	}
	if issue.State == "open" {
		// GitHub keeps "reopened" here after a reopen, which the badge has
		// nothing to say about.
		issue.StateReason = ""
	}
	if r.ClosedAt != nil {
		issue.ClosedAt = *r.ClosedAt
	}
	if r.Milestone != nil {
		issue.Milestone = r.Milestone.Title
	}
	if r.Type != nil {
		issue.Type = r.Type.Name
	}
	for _, label := range r.Labels {
		issue.Labels = append(issue.Labels, Label(label))
	}
	for _, assignee := range r.Assignees {
		issue.Assignees = append(issue.Assignees, assignee.Login)
	}
	return issue
}

// ref is the issue as something else names it. repository is the issue being
// read, for an issue the answer did not say the repository of.
func (r rawIssue) ref(repository string) Ref {
	ref := Ref{Repository: repository, Number: r.Number, Title: r.Title, State: r.State, URL: r.HTMLURL, PullRequest: r.PullRequest != nil}
	if r.Repository != nil && r.Repository.FullName != "" {
		ref.Repository = r.Repository.FullName
	}
	if r.PullRequest != nil && r.PullRequest.MergedAt != nil {
		ref.Merged = true
	}
	return ref
}

type rawReactions map[string]json.RawMessage

// counts keeps the reactions anyone made: the answer also carries the total
// and the URL, which are not reactions.
func (r rawReactions) counts() Reactions {
	counts := Reactions{}
	for _, name := range ReactionOrder {
		raw, ok := r[name]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(string(raw))
		if err == nil && n > 0 {
			counts[name] = n
		}
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
}

// ReactionOrder is the order the page draws reactions in.
var ReactionOrder = []string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}

type rawEvent struct {
	Event       string       `json:"event"`
	Actor       *rawUser     `json:"actor"`
	User        *rawUser     `json:"user"`
	CreatedAt   time.Time    `json:"created_at"`
	Body        string       `json:"body"`
	HTMLURL     string       `json:"html_url"`
	Reactions   rawReactions `json:"reactions"`
	Label       *rawLabel    `json:"label"`
	Assignee    *rawUser     `json:"assignee"`
	CommitID    string       `json:"commit_id"`
	StateReason string       `json:"state_reason"`
	LockReason  string       `json:"lock_reason"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Rename *struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"rename"`
	Source *struct {
		Issue *rawIssue `json:"issue"`
	} `json:"source"`
}

// quiet are the events the page does not draw: they are about who gets
// notifications, not about the issue.
var quiet = map[string]bool{"subscribed": true, "unsubscribed": true, "mentioned": true}

func (r rawEvent) event(repository string) (Event, bool) {
	if r.Event == "" || quiet[r.Event] {
		return Event{}, false
	}
	event := Event{Kind: r.Event, Actor: r.Actor.login(), At: r.CreatedAt, StateReason: r.StateReason}
	switch r.Event {
	case "commented":
		if event.Actor == "" {
			event.Actor = r.User.login()
		}
		event.Body, event.URL, event.Reactions = r.Body, r.HTMLURL, r.Reactions.counts()
	case "labeled", "unlabeled":
		if r.Label != nil {
			label := Label(*r.Label)
			event.Label = &label
		}
	case "assigned", "unassigned":
		event.Subject = r.Assignee.login()
	case "milestoned", "demilestoned":
		if r.Milestone != nil {
			event.Subject = r.Milestone.Title
		}
	case "renamed":
		if r.Rename != nil {
			event.From, event.To = r.Rename.From, r.Rename.To
		}
	case "locked":
		event.Subject = r.LockReason
	case "referenced", "closed":
		event.Subject = shortCommit(r.CommitID)
	case "cross-referenced":
		if r.Source == nil || r.Source.Issue == nil {
			return Event{}, false
		}
		source := r.Source.Issue.ref(repository)
		event.Source = &source
	}
	return event, true
}

func shortCommit(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}
