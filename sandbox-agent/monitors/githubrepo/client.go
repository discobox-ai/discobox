package githubrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// reserve is how much of a rate limit the monitor leaves to the agent: below
// it, the monitor waits for the reset rather than spend what the agent's own
// `gh` needs.
var reserve = map[string]int{"core": 200, "graphql": 200}

// rateLimited is a request the monitor will not make, or GitHub refused,
// until a time: a run ends there, keeping what it did, and the next run
// before then does nothing.
type rateLimited struct {
	resource string
	until    time.Time
}

func (e *rateLimited) Error() string {
	return fmt.Sprintf("GitHub's %s rate limit: waiting until %s", e.resource, e.until.UTC().Format(time.RFC3339))
}

// apiError is a response GitHub refused for a reason waiting will not fix.
type apiError struct {
	status  int
	path    string
	message string
}

func (e *apiError) Error() string {
	text := fmt.Sprintf("GitHub %s: %d", e.path, e.status)
	if e.message != "" {
		text += " " + e.message
	}
	switch e.status {
	case http.StatusUnauthorized:
		text += ": the box's GitHub credential was refused; ask for one with discobox-access (GH_TOKEN, for api.github.com)"
	case http.StatusForbidden, http.StatusNotFound:
		text += ": no such repository, or the credential cannot read it; ask for one that can with discobox-access"
	}
	return text
}

// client is the GitHub API as this monitor uses it: conditional REST GETs,
// GraphQL queries, and the rate limits both report.
type client struct {
	base  string
	token string
	http  *http.Client
	// limits is each resource's last-reported remaining and reset.
	limits map[string]limit
	// date is the Date of the last response: GitHub's clock.
	date time.Time
	// pollAfter is the latest X-Poll-Interval asked for, from when it was.
	pollAfter time.Time
	// counted is how many requests cost rate limit: every one but a 304.
	counted int
}

type limit struct {
	remaining int
	reset     time.Time
}

func newClient(base, token string, hc *http.Client) *client {
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &client{base: strings.TrimRight(base, "/"), token: token, http: hc, limits: map[string]limit{}}
}

// response is a REST GET's result. NotModified is a 304: nothing changed
// since etag, and nothing was counted.
type response struct {
	NotModified bool
	ETag        string
	Body        []byte
}

// get is a GET of path, conditional on etag when it is not empty.
func (c *client) get(ctx context.Context, path, etag string) (response, error) {
	if err := c.check("core"); err != nil {
		return response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.do(req)
	if err != nil {
		return response{}, err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return response{NotModified: true, ETag: etag}, nil
	case resp.StatusCode/100 == 2:
		return response{ETag: resp.Header.Get("ETag"), Body: resp.Body}, nil
	}
	return response{}, c.refused(resp, "core", path)
}

// getJSON is an unconditional GET of path into v.
func (c *client) getJSON(ctx context.Context, path string, v any) error {
	resp, err := c.get(ctx, path, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(resp.Body, v)
}

type graphqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// graphql runs query with vars, decoding its data into v.
func (c *client) graphql(ctx context.Context, query string, vars map[string]any, v any) error {
	if err := c.check("graphql"); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/graphql", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return c.refused(resp, "graphql", "/graphql")
	}
	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphqlError  `json:"errors"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return fmt.Errorf("GitHub /graphql: %w", err)
	}
	if len(out.Errors) > 0 {
		for _, e := range out.Errors {
			if e.Type == "RATE_LIMITED" {
				return &rateLimited{resource: "graphql", until: c.resetOf("graphql")}
			}
		}
		messages := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			messages = append(messages, e.Message)
		}
		return fmt.Errorf("GitHub /graphql: %s", strings.Join(messages, "; "))
	}
	return json.Unmarshal(out.Data, v)
}

// reply is a response, read whole.
type reply struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func (c *client) do(req *http.Request) (reply, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "discobox-monitor-github-repo")
	resp, err := c.http.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return reply{}, err
	}
	if resp.StatusCode != http.StatusNotModified {
		c.counted++
	}
	r := reply{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
	c.observe(r)
	return r, nil
}

// observe takes what a response says of GitHub's clock and limits.
func (c *client) observe(resp reply) {
	if date, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		c.date = date
	}
	resource := resp.Header.Get("X-RateLimit-Resource")
	remaining, errR := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	reset, errT := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if resource != "" && errR == nil && errT == nil {
		c.limits[resource] = limit{remaining: remaining, reset: time.Unix(reset, 0)}
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("X-Poll-Interval")); err == nil && seconds > 0 {
		if after := c.now().Add(time.Duration(seconds) * time.Second); after.After(c.pollAfter) {
			c.pollAfter = after
		}
	}
}

// check refuses a request the resource's limit should not be spent on.
func (c *client) check(resource string) error {
	l, ok := c.limits[resource]
	if ok && l.remaining < reserve[resource] && l.reset.After(c.now()) {
		return &rateLimited{resource: resource, until: l.reset}
	}
	return nil
}

func (c *client) resetOf(resource string) time.Time {
	if l, ok := c.limits[resource]; ok && l.reset.After(c.now()) {
		return l.reset
	}
	return c.now().Add(time.Minute)
}

// refused is the error a non-2xx response is: a wait when it is a rate limit,
// primary or secondary, and an apiError otherwise.
func (c *client) refused(resp reply, resource, path string) error {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(resp.Body, &msg)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			return &rateLimited{resource: resource, until: c.now().Add(time.Duration(seconds) * time.Second)}
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return &rateLimited{resource: resource, until: c.resetOf(resource)}
		}
		if strings.Contains(strings.ToLower(msg.Message), "rate limit") {
			// A secondary limit with no Retry-After: GitHub asks for a minute.
			return &rateLimited{resource: resource, until: c.now().Add(time.Minute)}
		}
	}
	return &apiError{status: resp.StatusCode, path: path, message: msg.Message}
}

// now is GitHub's clock when a response has said it, else the box's.
func (c *client) now() time.Time {
	if !c.date.IsZero() {
		return c.date
	}
	return time.Now()
}

func isRateLimited(err error) (*rateLimited, bool) {
	var rl *rateLimited
	ok := errors.As(err, &rl)
	return rl, ok
}
