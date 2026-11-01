// Package githubrepo is the image's `github-repo` monitor (ADR 26-10-31-073
// §9), a built-in (see builtin): new activity on one repository's issues,
// pull requests and Actions runs.
//
// A run costs nothing while the repository is quiet. It begins with
// conditional probes, sent with the ETag of the last answer, which GitHub
// answers 304 without counting against the rate limit:
//
//   - the issue most recently updated, which changes when any issue or PR
//     does: a comment, a review, a label, a close, a merge;
//   - the newest Actions runs, which change when one is created, moves on or
//     completes;
//   - each watched PR's head check runs, since a check finishing does not
//     update its PR; and each Actions run followed by id.
//
// Only what changed is then read: one GraphQL search for the issues and PRs
// updated since the last pass, with their comments, state changes and
// reviews nested; one GraphQL query for the watched PRs' checks and
// mergeability; and REST for Actions runs, and a failed run's jobs. See
// DESIGN.md.
package githubrepo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/builtin"
)

// Name is the built-in's name, the declaration's id.
const Name = "github-repo"

// apiBase is GitHub's API.
const apiBase = "https://api.github.com"

// overlap is how far each pass's search reaches back before the last one
// began, for a search index that lags behind what it indexes.
const overlap = 2 * time.Minute

// Run is one run of the monitor.
func Run(ctx context.Context, env builtin.Env) error {
	return run(ctx, env, apiBase, nil, time.Now)
}

func run(ctx context.Context, env builtin.Env, base string, hc *http.Client, now func() time.Time) error {
	cfg, err := parseConfig(env.Options)
	if err != nil {
		return err
	}
	var st state
	if _, err := env.ReadState(stateFile, &st); err != nil {
		return err
	}
	st.init()
	if !st.NotBefore.IsZero() && now().Before(st.NotBefore) {
		// Waiting out a rate limit or a poll interval is not a failure.
		return nil
	}
	token := credential(ctx, env.Getenv)
	if token == "" {
		return errors.New("no GitHub credential in this box: ask for one with discobox-access (GH_TOKEN, for api.github.com)")
	}
	p := &poller{cfg: cfg, c: newClient(base, token, hc), env: env, st: &st, first: st.Since.IsZero()}
	st.NotBefore = time.Time{}
	err = p.poll(ctx)
	// The box's clock, for the next run's check: GitHub's waits are
	// lengths of time from GitHub's now.
	if rl, ok := isRateLimited(err); ok {
		st.NotBefore = now().Add(rl.until.Sub(p.c.now()))
		fmt.Fprintln(env.Stderr, rl.Error())
		err = nil
	}
	if !p.c.pollAfter.IsZero() {
		if after := now().Add(p.c.pollAfter.Sub(p.c.now())); after.After(st.NotBefore) {
			st.NotBefore = after
		}
	}
	if saveErr := p.save(); err == nil {
		err = saveErr
	}
	return err
}

// credential is the box's GitHub token: the one the box was granted, else
// whatever `gh` is logged in with.
func credential(ctx context.Context, getenv func(string) string) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(getenv(name)); token != "" {
			return token
		}
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type poller struct {
	cfg   config
	c     *client
	env   builtin.Env
	st    *state
	first bool
}

func (p *poller) save() error { return p.env.WriteState(stateFile, p.st) }

func (p *poller) emit(ev monitors.Event) error { return p.env.Emit(ev) }

func (p *poller) repoPath(rest string) string {
	return "/repos/" + p.cfg.Owner + "/" + p.cfg.Name + rest
}

// poll is one run: probe, then read and report what changed.
func (p *poller) poll(ctx context.Context) error {
	var (
		issues, runs  response
		probedIssues  = p.cfg.Issues || p.cfg.PRs
		probedRuns    = p.cfg.Actions && len(p.cfg.RunIDs) == 0
		start         time.Time
		err           error
		checksChanged bool
		pendingChecks = map[string]checkProbe{}
	)
	if probedIssues {
		if issues, err = p.c.get(ctx, p.repoPath("/issues?state=all&sort=updated&direction=desc&per_page=1"), p.st.IssuesETag); err != nil {
			return err
		}
		start = p.c.now()
	}
	if probedRuns {
		if runs, err = p.c.get(ctx, p.repoPath("/actions/runs?per_page="+fmt.Sprint(newestRuns)+p.branchQuery()), p.st.RunsETag); err != nil {
			return err
		}
	}
	if start.IsZero() {
		start = p.c.now()
	}
	// Options changed in place name new targets, or drop old ones.
	fresh := !p.first && p.retarget()
	if p.cfg.PRs && !p.first {
		if checksChanged, err = p.probeChecks(ctx, pendingChecks); err != nil {
			return err
		}
	}

	if p.first {
		return p.baseline(ctx, start, issues, runs)
	}

	issuesChanged := probedIssues && !issues.NotModified
	if issuesChanged {
		p.st.LastChange = start
	}
	// The probe sees a change at once, and the search only once its index
	// has caught up. Passes go on until one that began overlap after the
	// change has completed: however long the runs are apart — a long
	// interval, a rate limit, a box asleep — a pass the index was too soon
	// for is followed by one it was not.
	settling := probedIssues && !p.st.LastChange.IsZero() && p.st.Since.Before(p.st.LastChange.Add(overlap))
	if settling || p.st.Walk != nil {
		if err := p.walkActivity(ctx, start); err != nil {
			return err
		}
	}
	runsChanged := probedRuns && !runs.NotModified
	if p.needsWatched(issuesChanged || runsChanged || checksChanged || fresh) {
		if err := p.queryWatched(ctx, true); err != nil {
			return err
		}
		for n, probe := range pendingChecks {
			// Saved only for the head it was probed at: a new head's checks
			// are probed afresh.
			if pr := p.st.PRs[n]; pr != nil && pr.HeadSHA == probe.sha {
				pr.ChecksETag, pr.StatusETag = probe.checks, probe.status
			}
		}
	}
	// Only now is all an issues change called for done: a run cut short
	// before here sees it again.
	if issuesChanged {
		p.st.IssuesETag = issues.ETag
	}
	if err := p.save(); err != nil {
		return err
	}
	if p.cfg.Actions {
		if err := p.followRuns(ctx, start, runs, true); err != nil {
			return err
		}
	}
	return p.finishIfDone()
}

// baseline records where everything stands and reports nothing (073 §3),
// except that an instance whose every named item is already done says so.
//
// It is done only once it has all been read: Since stays zero until the end,
// so a baseline cut short is taken again whole rather than read later as a
// pass over everything since the epoch.
func (p *poller) baseline(ctx context.Context, start time.Time, issues, runs response) error {
	if p.cfg.PRs && p.cfg.filtered() {
		if err := p.selectOpenPRs(ctx); err != nil {
			return err
		}
	}
	for _, n := range p.cfg.PRIDs {
		p.watchPR(n, true)
	}
	if err := p.queryWatched(ctx, false); err != nil {
		return err
	}
	if p.cfg.Actions {
		if err := p.followRuns(ctx, start, runs, false); err != nil {
			return err
		}
	}
	p.st.Since, p.st.Baseline = start, start
	p.st.IssuesETag = issues.ETag
	return p.finishIfDone()
}

// needsWatched reports whether the watched issues and PRs are queried this
// run: on a change, or while a mergeable state is still being computed.
func (p *poller) needsWatched(changed bool) bool {
	if len(p.st.PRs) == 0 && (!p.cfg.Issues || len(p.cfg.IssueIDs) == 0) {
		return false
	}
	return changed || (p.st.Recheck > 0 && p.st.Recheck <= maxRecheck)
}

// finishIfDone ends an instance watching only ids once every one is closed,
// merged or complete.
func (p *poller) finishIfDone() error {
	if !p.cfg.onlyIDs() {
		return nil
	}
	named, done := 0, 0
	if p.cfg.Issues {
		for _, n := range p.cfg.IssueIDs {
			named++
			if p.st.Issues[key(n)] == "CLOSED" {
				done++
			}
		}
	}
	if p.cfg.PRs {
		for _, n := range p.cfg.PRIDs {
			named++
			if pr := p.st.PRs[key(n)]; pr != nil && (pr.State == "CLOSED" || pr.State == "MERGED") {
				done++
			}
		}
	}
	if p.cfg.Actions {
		for _, id := range p.cfg.RunIDs {
			named++
			if strings.HasPrefix(p.st.Runs[key(id)], "done:") {
				done++
			}
		}
	}
	if named == 0 || done < named {
		return nil
	}
	return p.emit(monitors.Event{
		Kind:  "github.finished",
		Title: "everything this watches in " + p.cfg.repo() + " is closed or complete",
		Final: true,
	})
}

func key(n int64) string { return fmt.Sprint(n) }

// firstLine is text's first non-blank line, cut to at most n runes.
func firstLine(text string, n int) string {
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > n {
				return string(r[:n])
			}
			return line
		}
	}
	return ""
}
