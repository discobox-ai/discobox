package githubrepo

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/builtin"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func comment(id string, at time.Time, body string) timelineItem {
	return timelineItem{Type: "IssueComment", ID: id, CreatedAt: at, URL: "https://github.com/o/r/issues/1#" + id, Body: body, Author: &login{"alice"}}
}

func issueNode(n int64, created, updated time.Time, items ...timelineItem) activityNode {
	return activityNode{
		Type: "Issue", ID: fmt.Sprintf("I_%d", n), Number: n, Title: fmt.Sprintf("issue %d", n),
		URL: fmt.Sprintf("https://github.com/o/r/issues/%d", n), State: "OPEN",
		CreatedAt: created, UpdatedAt: updated, Author: &login{"bob"},
		Timeline: timeline{Nodes: items},
	}
}

func prNode(n int64, head string, created, updated time.Time, items ...timelineItem) activityNode {
	node := issueNode(n, created, updated, items...)
	node.Type, node.ID, node.HeadRefName = "PullRequest", fmt.Sprintf("PR_%d", n), head
	node.URL = fmt.Sprintf("https://github.com/o/r/pull/%d", n)
	node.Title = fmt.Sprintf("pr %d", n)
	return node
}

func pull(n int64, head, sha, rollup, mergeable string) watchedPR {
	pr := watchedPR{
		Number: n, Title: fmt.Sprintf("pr %d", n), URL: fmt.Sprintf("https://github.com/o/r/pull/%d", n),
		State: "OPEN", HeadRefName: head, HeadRefOid: sha, Mergeable: mergeable, Author: &login{"worker"},
	}
	if rollup != "" {
		pr.Commits.Nodes = []headCommit{{}}
		r := &checkRollup{State: rollup}
		pr.Commits.Nodes[0].Commit.Rollup = r
		conclusion := map[string]string{"SUCCESS": "SUCCESS", "FAILURE": "FAILURE"}[rollup]
		status := "COMPLETED"
		if conclusion == "" {
			status = "IN_PROGRESS"
		}
		r.Contexts.Nodes = []checkContext{
			{Type: "CheckRun", Name: "test", Status: status, Conclusion: conclusion},
			{Type: "CheckRun", Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS"},
		}
	}
	return pr
}

func TestAQuietRepositoryCostsNoCountedCalls(t *testing.T) {
	for name, opts := range map[string]map[string]string{
		"whole repository": {"repo": "o/r"},
		"one PR":           {"repo": "o/r", "pr": "7", "watch": "prs,actions"},
		"PRs by head":      {"repo": "o/r", "head": "discobox/*", "watch": "prs,actions"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, t0)
			f.touchIssues()
			f.setRuns(workflowRun{ID: 50, Name: "ci", Status: "in_progress", CreatedAt: t0.Add(-time.Minute), UpdatedAt: t0})
			f.prs[7] = pull(7, "discobox/issue-7", "abc1234567", "PENDING", "MERGEABLE")
			f.setChecks("abc1234567", `{"total_count":1,"check_runs":[{"name":"test","status":"in_progress"}]}`)
			in := newInstance(t, f, opts)
			if got := in.poll(); len(got) != 0 {
				t.Fatalf("baseline reported %+v", got)
			}
			// The second run learns the head's check-run ETag; from then on
			// nothing that has not changed is counted.
			f.advance(time.Minute)
			in.poll()
			f.reset()
			for range 3 {
				f.advance(time.Minute)
				if got := in.poll(); len(got) != 0 {
					t.Fatalf("a quiet repository reported %+v", got)
				}
			}
			if f.counted != 0 {
				t.Fatalf("counted %d calls while quiet: %v", f.counted, f.requests)
			}
			if len(f.requests) == 0 {
				t.Fatal("no probes were sent")
			}
		})
	}
}

func TestTheBaselineReportsNothingThatWasAlreadyThere(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	// A comment a minute before the baseline is inside the search's overlap,
	// and is still not new.
	f.nodes = []activityNode{issueNode(1, t0.Add(-time.Hour), t0.Add(-time.Minute), comment("c0", t0.Add(-time.Minute), "old"))}
	in := newInstance(t, f, map[string]string{"repo": "o/r", "watch": "issues"})
	in.poll()
	f.advance(time.Minute)
	f.nodes[0].UpdatedAt = f.now
	f.nodes[0].Timeline.Nodes = append(f.nodes[0].Timeline.Nodes, comment("c1", f.now, "new\nmore"))
	f.touchIssues()
	got := in.poll()
	if len(got) != 1 || got[0].Kind != "issue.comment" || got[0].Title != "alice commented on #1: new" || got[0].Body != "new\nmore" {
		t.Fatalf("events = %+v", got)
	}
	// The overlap reads it again on the next change, and it is not repeated.
	f.advance(time.Minute)
	f.touchIssues()
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("repeated %+v", got)
	}
}

// busyGap is 60 issues commented on while the instance was asleep: three
// pages of the search.
func busyGap(f *fakeGitHub, from time.Time) {
	for i := range 60 {
		at := from.Add(time.Duration(i+1) * time.Minute)
		f.nodes = append(f.nodes, issueNode(int64(100+i), t0.Add(-time.Hour), at, comment(fmt.Sprintf("c%d", i), at, fmt.Sprintf("comment %d", i))))
	}
	f.now = from.Add(2 * time.Hour)
	f.touchIssues()
}

func commentOrder(t *testing.T, events []monitors.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		if ev.Kind != "issue.comment" {
			t.Fatalf("unexpected %+v", ev)
		}
		out = append(out, strings.TrimPrefix(ev.Body, "comment "))
	}
	return out
}

func wantComments(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprint(i)
	}
	return out
}

func TestACatchUpReadsEveryPageOnceInOrder(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	in := newInstance(t, f, map[string]string{"repo": "o/r", "watch": "issues"})
	in.poll()
	busyGap(f, t0)
	f.reset()
	got := commentOrder(t, in.poll())
	if !slices.Equal(got, wantComments(60)) {
		t.Fatalf("comments = %v", got)
	}
	// The probe, then one query per page: no call per issue.
	if f.counted != 1+3 {
		t.Fatalf("counted %d: %v", f.counted, f.requests)
	}
	if st := in.state(); st.Walk != nil || !st.Since.Equal(f.now) {
		t.Fatalf("state = since %v, walk %+v", st.Since, st.Walk)
	}
}

func TestARateLimitPartWayKeepsWhatWasDoneAndResumes(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	in := newInstance(t, f, map[string]string{"repo": "o/r", "watch": "issues"})
	in.poll()
	busyGap(f, t0)
	f.limitAt["search"] = 2
	first := commentOrder(t, in.poll())
	if !slices.Equal(first, wantComments(searchPage)) {
		t.Fatalf("before the limit = %v", first)
	}
	st := in.state()
	if st.Walk == nil || st.NotBefore.IsZero() {
		t.Fatalf("the limit left no progress or wait: %+v", st)
	}
	// Until the reset it calls nothing at all.
	f.reset()
	in.clock = in.clock.Add(5 * time.Minute)
	if got := in.poll(); len(got) != 0 || len(f.requests) != 0 {
		t.Fatalf("while limited: events %v, requests %v", got, f.requests)
	}
	// After it, the pass resumes where it stopped, and repeats nothing.
	in.clock = in.clock.Add(10 * time.Minute)
	f.advance(15 * time.Minute)
	rest := commentOrder(t, in.poll())
	if all := append(first, rest...); !slices.Equal(all, wantComments(60)) {
		t.Fatalf("after the limit = %v", rest)
	}
	if st := in.state(); st.Walk != nil || !st.NotBefore.IsZero() {
		t.Fatalf("state after = %+v", st)
	}
}

func TestAPrimaryLimitNearlySpentWaitsForTheReset(t *testing.T) {
	c := newClient("http://unused", "token", nil)
	c.date = t0
	c.limits["core"] = limit{remaining: 10, reset: t0.Add(time.Minute)}
	_, err := c.get(context.Background(), "/x", "")
	if rl, ok := isRateLimited(err); !ok || !rl.until.Equal(t0.Add(time.Minute)) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnInstanceWatchingOnlyIdsEndsWhenEveryOneIsDone(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	f.prs[7] = pull(7, "fix", "abc1234567", "SUCCESS", "MERGEABLE")
	f.setChecks("abc1234567", `{"total_count":0,"check_runs":[]}`)
	f.set("/repos/o/r/actions/runs/99", `{"id":99,"name":"ci","status":"in_progress","head_branch":"main"}`)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "pr": "7", "run": "99"})
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("baseline = %+v", got)
	}

	// The run completes; the PR is still open.
	f.advance(time.Minute)
	f.set("/repos/o/r/actions/runs/99", fmt.Sprintf(`{"id":99,"name":"ci","status":"completed","conclusion":"success","head_branch":"main","event":"push","display_title":"fix","html_url":"https://github.com/o/r/actions/runs/99","updated_at":%q}`, f.now.Format(time.RFC3339)))
	if got := in.poll(); !slices.Equal(kinds(got), []string{"run.completed"}) || got[0].Title != "ci success on main" {
		t.Fatalf("events = %+v", got)
	}

	// The PR merges.
	f.advance(time.Minute)
	merged := pull(7, "fix", "abc1234567", "SUCCESS", "MERGEABLE")
	merged.State = "MERGED"
	f.prs[7] = merged
	f.nodes = []activityNode{prNode(7, "fix", t0.Add(-time.Hour), f.now,
		timelineItem{Type: "ClosedEvent", ID: "close", CreatedAt: f.now, Actor: &login{"carol"}},
		timelineItem{Type: "MergedEvent", ID: "merge", CreatedAt: f.now, Actor: &login{"carol"}})}
	f.touchIssues()
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"pr.merged", "github.finished"}) || !got[1].Final || got[0].Key != "pr-7-state" {
		t.Fatalf("events = %+v", got)
	}
}

func TestPRsSelectedByHeadReportChecksAndConflicts(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	// At the baseline its mergeability is not computed yet, and what it
	// turns out to be was already so: recorded, not reported.
	f.prs[5] = pull(5, "discobox/issue-5", "aaa1111111", "PENDING", "UNKNOWN")
	f.prs[6] = pull(6, "feature", "bbb2222222", "PENDING", "MERGEABLE")
	f.setChecks("aaa1111111", `{"check_runs":[{"name":"test","status":"in_progress"}]}`)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "head": "discobox/*"})
	in.poll()
	if st := in.state(); st.PRs["5"] == nil || st.PRs["6"] != nil || st.Recheck != 1 {
		t.Fatalf("watched = %+v, recheck %d", st.PRs, st.Recheck)
	}

	// Asked again while unknown: it is CONFLICTING, which it already was.
	f.advance(time.Minute)
	f.prs[5] = pull(5, "discobox/issue-5", "aaa1111111", "PENDING", "CONFLICTING")
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("a state known since before the baseline was reported: %+v", got)
	}

	// Its checks fail: the check-run probe changes, though the PR did not.
	f.advance(time.Minute)
	f.prs[5] = pull(5, "discobox/issue-5", "aaa1111111", "FAILURE", "CONFLICTING")
	f.setChecks("aaa1111111", `{"check_runs":[{"name":"test","status":"completed","conclusion":"failure"}]}`)
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"pr.checks"}) || got[0].Title != "#5 checks failed on aaa1111: test" || got[0].Key != "pr-5-checks" {
		t.Fatalf("events = %+v", got)
	}

	// A rebase clears the conflict; the new head's checks pass.
	f.advance(time.Minute)
	f.prs[5] = pull(5, "discobox/issue-5", "ccc3333333", "SUCCESS", "MERGEABLE")
	f.setChecks("ccc3333333", `{"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`)
	f.nodes = []activityNode{prNode(5, "discobox/issue-5", t0.Add(-time.Hour), f.now)}
	f.touchIssues()
	got = in.poll()
	if !slices.Equal(kinds(got), []string{"pr.checks", "pr.conflict"}) ||
		got[0].Title != "#5 checks passed on ccc3333" || got[1].Title != "#5 no longer conflicts with its base: pr 5" || got[1].Key != "pr-5-mergeable" {
		t.Fatalf("events = %+v", got)
	}

	// A worker opens a PR on a matching branch, conflicting from the start:
	// it was not there at the baseline, so that is news.
	f.advance(time.Minute)
	f.prs[8] = pull(8, "discobox/issue-8", "ddd4444444", "PENDING", "CONFLICTING")
	f.setChecks("ddd4444444", `{"check_runs":[]}`)
	f.nodes = append(f.nodes, prNode(8, "discobox/issue-8", f.now, f.now))
	f.touchIssues()
	got = in.poll()
	if !slices.Equal(kinds(got), []string{"pr.opened", "pr.conflict"}) || got[1].Title != "#8 conflicts with its base: pr 8" {
		t.Fatalf("events = %+v", got)
	}
}

func TestActionsRunsOfABranchAreReportedOncePerCompletion(t *testing.T) {
	f := newFake(t, t0)
	runs := func(rs ...workflowRun) {
		f.setRuns(rs...)
		f.rest["/repos/o/r/actions/runs?per_page=100&branch=main"] = f.rest["/repos/o/r/actions/runs?per_page=100"]
	}
	running := workflowRun{ID: 1, Name: "ci", Status: "in_progress", HeadBranch: "main", CreatedAt: t0, UpdatedAt: t0}
	runs(running)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "branch": "main"})
	in.poll()

	f.advance(time.Minute)
	failed := running
	failed.Status, failed.Conclusion, failed.UpdatedAt = "completed", "failure", f.now
	failed.PullRequests = []struct {
		Number int64 `json:"number"`
	}{{Number: 9}}
	runs(failed)
	f.set("/repos/o/r/actions/runs/1/jobs?per_page=100", `{"jobs":[{"name":"test","conclusion":"failure","html_url":"https://github.com/o/r/actions/runs/1/job/2","steps":[{"name":"go test","conclusion":"failure"}]},{"name":"lint","conclusion":"success"}]}`)
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"run.completed"}) || got[0].Title != "ci failure on main (#9)" ||
		got[0].Body != "test: failure\n  step: go test\n  https://github.com/o/r/actions/runs/1/job/2" {
		t.Fatalf("events = %+v", got)
	}
	// Seen again, unchanged, it is not news; re-run, it is.
	f.advance(time.Minute)
	runs(failed, workflowRun{ID: 2, Name: "other", Status: "queued", CreatedAt: f.now, UpdatedAt: f.now})
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("repeated %+v", got)
	}
	f.advance(time.Minute)
	rerun := failed
	rerun.Conclusion, rerun.UpdatedAt = "success", f.now
	runs(rerun)
	if got := in.poll(); len(got) != 1 || got[0].Title != "ci success on main (#9)" {
		t.Fatalf("events = %+v", got)
	}
}

func TestWithoutACredentialTheFailureNamesDiscoboxAccess(t *testing.T) {
	t.Setenv("PATH", "")
	env := builtin.Env{State: t.TempDir(), Options: map[string]string{"repo": "o/r"}, Getenv: func(string) string { return "" }, Stderr: io.Discard}
	err := Run(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "discobox-access") {
		t.Fatalf("err = %v", err)
	}
}

func TestOptions(t *testing.T) {
	for _, tc := range []struct {
		opts    map[string]string
		want    config
		onlyIDs bool
		problem string
	}{
		{opts: map[string]string{"repo": "o/r"}, want: config{Owner: "o", Name: "r", Issues: true, PRs: true, Actions: true}},
		{opts: map[string]string{"repo": "o/r", "pr": "412"}, want: config{Owner: "o", Name: "r", PRs: true, PRIDs: []int64{412}}, onlyIDs: true},
		{opts: map[string]string{"repo": "o/r", "head": "discobox/*"}, want: config{Owner: "o", Name: "r", PRs: true, Head: "discobox/*"}},
		{opts: map[string]string{"repo": "o/r", "branch": "main"}, want: config{Owner: "o", Name: "r", Actions: true, Branch: "main"}},
		{opts: map[string]string{"repo": "o/r", "pr": "4", "watch": "prs,actions"}, want: config{Owner: "o", Name: "r", PRs: true, Actions: true, PRIDs: []int64{4}}},
		{opts: map[string]string{"repo": "nope"}, problem: "owner/name"},
		{opts: map[string]string{"repo": "o/r", "pr": "x"}, problem: "not a number"},
		{opts: map[string]string{"repo": "o/r", "watch": "pulls"}, problem: "watch takes"},
		{opts: map[string]string{"repo": "o/r", "head": "["}, problem: "not a glob"},
	} {
		got, err := parseConfig(tc.opts)
		if tc.problem != "" {
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Errorf("%v: err = %v", tc.opts, err)
			}
			continue
		}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tc.want) || got.onlyIDs() != tc.onlyIDs {
			t.Errorf("%v = %+v, %v (only ids %v)", tc.opts, got, err, got.onlyIDs())
		}
	}
	c, _ := parseConfig(map[string]string{"repo": "o/r", "head": "discobox/*", "author": "@worker"})
	if !c.wantsPR(1, "discobox/issue-1", "Worker") || c.wantsPR(2, "discobox/issue-2", "someone") || c.wantsPR(3, "main", "worker") {
		t.Fatal("head and author do not select together")
	}
}

// The image's declaration is the one this package reads: every option it
// declares is one parseConfig takes, and its interval is the issue's.
func TestTheImageDeclaresThisMonitor(t *testing.T) {
	path, err := filepath.Abs("../../features/cli/monitors/10-github-repo.sh")
	if err != nil {
		t.Fatal(err)
	}
	def, err := monitors.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if def.ID != Name || def.Problem != "" || def.Interval != time.Minute || def.MinInterval != 10*time.Second {
		t.Fatalf("declaration = %+v", def)
	}
	known := []string{"repo", "watch", "issue", "pr", "head", "author", "run", "branch", "interval"}
	for _, opt := range def.Options {
		if !slices.Contains(known, opt.Name) {
			t.Errorf("declares %q, which the monitor does not read", opt.Name)
		}
	}
	if len(def.Options) != len(known) {
		t.Errorf("declares %d options, the monitor reads %d", len(def.Options), len(known))
	}
}

func TestATargetNamedSinceTheBaselineStartsAsItsOwnBaseline(t *testing.T) {
	// Options changed in place (ADR 26-11-01-375 §2, proposed): a PR named
	// later reports what happens after it was named, not what it already was.
	f := newFake(t, t0)
	f.touchIssues()
	f.prs[7] = pull(7, "a", "aaa1111111", "SUCCESS", "MERGEABLE")
	f.prs[8] = pull(8, "b", "bbb2222222", "FAILURE", "CONFLICTING")
	f.setChecks("aaa1111111", `{"check_runs":[]}`)
	f.setChecks("bbb2222222", `{"check_runs":[{"conclusion":"failure"}]}`)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "pr": "7"})
	in.poll()

	f.advance(time.Minute)
	in.env.Options = map[string]string{"repo": "o/r", "pr": "8"}
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("a newly named PR reported its standing state: %+v", got)
	}
	if st := in.state(); st.PRs["7"] != nil || st.PRs["8"] == nil || st.PRs["8"].Checks != "failure" {
		t.Fatalf("watched = %+v", st.PRs)
	}
	f.advance(time.Minute)
	f.prs[8] = pull(8, "b", "bbb2222222", "SUCCESS", "MERGEABLE")
	f.setChecks("bbb2222222", `{"check_runs":[{"conclusion":"success"}]}`)
	if got := in.poll(); !slices.Equal(kinds(got), []string{"pr.checks", "pr.conflict"}) {
		t.Fatalf("events = %+v", got)
	}
}

func TestAChangeTheSearchIndexHasNotCaughtUpWithIsFoundByTheNextPass(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	in := newInstance(t, f, map[string]string{"repo": "o/r", "watch": "issues"})
	in.poll()
	// The probe sees the comment; the search does not have it yet.
	f.advance(time.Minute)
	at := f.now
	f.touchIssues()
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
	// The probe is quiet now, and the index has caught up.
	f.advance(time.Minute)
	f.nodes = []activityNode{issueNode(1, t0.Add(-time.Hour), at, comment("c1", at, "late"))}
	if got := in.poll(); len(got) != 1 || got[0].Body != "late" {
		t.Fatalf("a comment the index lagged on was lost: %+v", got)
	}
	// Past the overlap, a quiet probe costs nothing again.
	f.advance(2 * overlap)
	in.poll()
	f.reset()
	f.advance(time.Minute)
	in.poll()
	if f.counted != 0 {
		t.Fatalf("counted %v", f.requests)
	}
}

func TestAWatchedQueryCutShortIsTakenAgainNextRun(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	f.prs[7] = pull(7, "fix", "abc1234567", "SUCCESS", "MERGEABLE")
	f.setChecks("abc1234567", `{"check_runs":[]}`)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "pr": "7"})
	in.poll()

	f.advance(time.Minute)
	merged := f.prs[7]
	merged.State = "MERGED"
	f.prs[7] = merged
	f.nodes = []activityNode{prNode(7, "fix", t0.Add(-time.Hour), f.now,
		timelineItem{Type: "MergedEvent", ID: "merge", CreatedAt: f.now, Actor: &login{"carol"}})}
	f.touchIssues()
	f.limitAt["watched"] = f.calls["watched"] + 1
	if got := in.poll(); !slices.Equal(kinds(got), []string{"pr.merged"}) {
		t.Fatalf("events = %+v", got)
	}
	in.clock = in.clock.Add(15 * time.Minute)
	f.advance(15 * time.Minute)
	got := in.poll()
	if !slices.Equal(kinds(got), []string{"github.finished"}) {
		t.Fatalf("after the limit, events = %+v", got)
	}
}

func TestABaselineCutShortIsTakenAgainWhole(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	f.setRuns(workflowRun{ID: 1, Name: "ci", Status: "completed", Conclusion: "success", CreatedAt: t0.Add(-time.Hour), UpdatedAt: t0.Add(-time.Hour)})
	f.prs[7] = pull(7, "fix", "abc1234567", "SUCCESS", "MERGEABLE")
	f.limitAt["watched"] = 1
	in := newInstance(t, f, map[string]string{"repo": "o/r", "pr": "7", "watch": "prs,actions"})
	in.poll()
	if st := in.state(); !st.Since.IsZero() {
		t.Fatalf("a baseline cut short was taken as done: since %v", st.Since)
	}
	in.clock = in.clock.Add(15 * time.Minute)
	f.advance(15 * time.Minute)
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("the retaken baseline reported %+v", got)
	}
	if st := in.state(); st.Since.IsZero() || st.RunsSince.IsZero() {
		t.Fatalf("state = %+v", st)
	}
}

func TestChecksAreNotFailedWhileAnyIsStillRunning(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	f.prs[7] = pull(7, "fix", "abc1234567", "PENDING", "MERGEABLE")
	f.setChecks("abc1234567", `{"check_runs":[1]}`)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "pr": "7"})
	in.poll()

	// One failed, one still runs: GitHub's rollup already says FAILURE.
	f.advance(time.Minute)
	pr := pull(7, "fix", "abc1234567", "FAILURE", "MERGEABLE")
	pr.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes = []checkContext{
		{Type: "CheckRun", Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
		{Type: "CheckRun", Name: "e2e", Status: "IN_PROGRESS"},
	}
	f.prs[7] = pr
	f.setChecks("abc1234567", `{"check_runs":[2]}`)
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("reported before every check finished: %+v", got)
	}
	f.advance(time.Minute)
	pr.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes[1] = checkContext{Type: "CheckRun", Name: "e2e", Status: "COMPLETED", Conclusion: "FAILURE"}
	f.prs[7] = pr
	f.setChecks("abc1234567", `{"check_runs":[3]}`)
	if got := in.poll(); len(got) != 1 || got[0].Title != "#7 checks failed on abc1234: test, e2e" {
		t.Fatalf("events = %+v", got)
	}
}

func TestACommitStatusFinishingIsSeenWithoutACheckRunChanging(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	pending := pull(7, "fix", "abc1234567", "PENDING", "MERGEABLE")
	pending.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes = []checkContext{{Type: "StatusContext", Context: "ci/jenkins", State: "PENDING"}}
	f.prs[7] = pending
	f.setChecks("abc1234567", `{"check_runs":[]}`)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "pr": "7"})
	in.poll()
	f.advance(time.Minute)
	in.poll()

	f.advance(time.Minute)
	done := pull(7, "fix", "abc1234567", "SUCCESS", "MERGEABLE")
	done.Commits.Nodes[0].Commit.Rollup.Contexts.Nodes = []checkContext{{Type: "StatusContext", Context: "ci/jenkins", State: "SUCCESS"}}
	f.prs[7] = done
	f.set("/repos/o/r/commits/abc1234567/status?per_page=100", `{"state":"success"}`)
	if got := in.poll(); len(got) != 1 || got[0].Title != "#7 checks passed on abc1234" || got[0].Body != "ci/jenkins: success" {
		t.Fatalf("events = %+v", got)
	}
}

func TestARunDeletedWhileFollowedIsLetGo(t *testing.T) {
	f := newFake(t, t0)
	f.setRuns(workflowRun{ID: 1, Name: "ci", Status: "in_progress", CreatedAt: t0, UpdatedAt: t0})
	in := newInstance(t, f, map[string]string{"repo": "o/r", "watch": "actions"})
	in.poll()
	// It leaves the newest page, and is gone when fetched.
	f.advance(time.Minute)
	f.setRuns(workflowRun{ID: 2, Name: "ci", Status: "queued", CreatedAt: f.now, UpdatedAt: f.now})
	in.poll()
	f.advance(time.Minute)
	f.missingOK = true // the deleted run's 404
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
	if st := in.state(); st.Runs["1"] != "" {
		t.Fatalf("still follows the deleted run: %+v", st.Runs)
	}
}

func TestANamedRunThatDoesNotExistSaysSo(t *testing.T) {
	f := newFake(t, t0)
	f.missingOK = true
	in := newInstance(t, f, map[string]string{"repo": "o/r", "run": "404"})
	err := run(context.Background(), in.env, f.srv.URL, f.srv.Client(), time.Now)
	if err == nil || !strings.Contains(err.Error(), "run 404 is not in o/r") {
		t.Fatalf("err = %v", err)
	}
}

func TestEveryReviewOfABusyPRIsReportedWithItsInlineComments(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	in := newInstance(t, f, map[string]string{"repo": "o/r", "watch": "prs"})
	in.poll()

	f.advance(time.Hour)
	at := func(i int) *time.Time { t := t0.Add(time.Duration(i+1) * time.Minute); return &t }
	mk := func(i int) review {
		r := review{ID: fmt.Sprintf("R%d", i), State: "COMMENTED", Body: fmt.Sprintf("review %d", i), URL: "u", SubmittedAt: at(i), Author: &login{"alice"}}
		return r
	}
	// 25 reviews: the search's page holds the latest 20, the rest are a page
	// back.
	node := prNode(9, "x", t0.Add(-time.Hour), f.now)
	for i := 5; i < 25; i++ {
		node.Reviews.Nodes = append(node.Reviews.Nodes, mk(i))
	}
	node.Reviews.PageInfo.HasPreviousPage, node.Reviews.PageInfo.StartCursor = true, "c5"
	var older reviews
	for i := range 5 {
		older.Nodes = append(older.Nodes, mk(i))
	}
	older.Nodes[0].Comments.TotalCount = 1
	f.reviewPages[node.ID] = older
	f.reviewComments["R0"] = []reviewComment{{Path: "main.go", Line: new(12), Body: "leaks the fd"}}
	f.nodes = []activityNode{node}
	f.touchIssues()
	got := in.poll()
	if len(got) != 25 || got[0].Body != "review 0\n\nmain.go:12: leaks the fd" || got[24].Body != "review 24" {
		t.Fatalf("%d events, first %+v", len(got), got[0])
	}
}

func TestALaggingSearchIsReadAgainHoweverFarApartTheRunsAre(t *testing.T) {
	// The next run is five minutes on, past the overlap: the pass that the
	// index was too soon for is still followed by another.
	f := newFake(t, t0)
	f.touchIssues()
	in := newInstance(t, f, map[string]string{"repo": "o/r", "watch": "issues", "interval": "5m"})
	in.poll()
	f.advance(5 * time.Minute)
	at := f.now
	f.touchIssues()
	in.poll()
	f.advance(5 * time.Minute)
	f.nodes = []activityNode{issueNode(1, t0.Add(-time.Hour), at, comment("c1", at, "late"))}
	if got := in.poll(); len(got) != 1 || got[0].Body != "late" {
		t.Fatalf("events = %+v", got)
	}
	// That pass began past the overlap: the next quiet run calls nothing.
	f.reset()
	f.advance(5 * time.Minute)
	in.poll()
	if f.counted != 0 {
		t.Fatalf("counted %v", f.requests)
	}
}

func TestAHeadWithMoreChecksThanAPageIsReadWhole(t *testing.T) {
	f := newFake(t, t0)
	f.touchIssues()
	pr := pull(7, "fix", "abc1234567", "PENDING", "MERGEABLE")
	f.prs[7] = pr
	f.setChecks("abc1234567", `{"check_runs":[1]}`)
	in := newInstance(t, f, map[string]string{"repo": "o/r", "pr": "7"})
	in.poll()

	// The first page has finished, one failed; a check on the next page
	// still runs, though the rollup says FAILURE.
	f.advance(time.Minute)
	pr = pull(7, "fix", "abc1234567", "FAILURE", "MERGEABLE")
	rollup := pr.Commits.Nodes[0].Commit.Rollup
	pr.Commits.Nodes[0].Commit.ID = "C1"
	rollup.Contexts.Nodes = []checkContext{{Type: "CheckRun", Name: "unit", Status: "COMPLETED", Conclusion: "FAILURE"}}
	rollup.Contexts.TotalCount = 2
	rollup.Contexts.PageInfo = pageInfo{HasNextPage: true, EndCursor: "p1"}
	f.prs[7] = pr
	f.contextPages["C1"] = contexts{TotalCount: 2, Nodes: []checkContext{{Type: "CheckRun", Name: "e2e", Status: "IN_PROGRESS"}}}
	f.setChecks("abc1234567", `{"check_runs":[2]}`)
	if got := in.poll(); len(got) != 0 {
		t.Fatalf("reported with a check on the next page still running: %+v", got)
	}
	f.advance(time.Minute)
	f.contextPages["C1"] = contexts{TotalCount: 2, Nodes: []checkContext{{Type: "CheckRun", Name: "e2e", Status: "COMPLETED", Conclusion: "SUCCESS"}}}
	f.setChecks("abc1234567", `{"check_runs":[3]}`)
	if got := in.poll(); len(got) != 1 || got[0].Title != "#7 checks failed on abc1234: unit" {
		t.Fatalf("events = %+v", got)
	}
}
