package githubrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
)

const (
	// newestRuns is the page of runs the probe reads, newest created first.
	// A larger page costs the same calls — one when it changes, none when
	// it does not — and sees a re-run of an older run: a re-run keeps its
	// created_at, so one older than this page goes unseen.
	newestRuns = 100
	// maxRunProbes bounds the runs fetched one by one each poll.
	maxRunProbes = 30
	// runPages bounds a catch-up's listing of runs created in a gap.
	runPages = 10
)

type workflowRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HeadBranch   string    `json:"head_branch"`
	Event        string    `json:"event"`
	DisplayTitle string    `json:"display_title"`
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	PullRequests []struct {
		Number int64 `json:"number"`
	} `json:"pull_requests"`
}

type runList struct {
	TotalCount int           `json:"total_count"`
	Runs       []workflowRun `json:"workflow_runs"`
}

func (p *poller) branchQuery() string {
	if p.cfg.Branch == "" {
		return ""
	}
	return "&branch=" + url.QueryEscape(p.cfg.Branch)
}

// followRuns reports Actions runs completing: a named run when its probe
// changes, else those on the newest page and those created in a gap since
// the last pass, and any run still followed that has left the newest page.
func (p *poller) followRuns(ctx context.Context, start time.Time, newest response, report bool) error {
	if p.st.RunsSince.IsZero() {
		// No pass over runs yet — Actions watched only since its options
		// changed: this one is its baseline.
		report = false
	}
	if len(p.cfg.RunIDs) > 0 {
		for _, id := range p.cfg.RunIDs {
			if err := p.probeRun(ctx, key(id), true, report); err != nil {
				return err
			}
		}
		p.st.RunsSince = start
		return p.save()
	}
	seen := map[string]bool{}
	for _, id := range p.st.NewestRuns {
		seen[id] = true
	}
	if !newest.NotModified {
		var page runList
		if err := json.Unmarshal(newest.Body, &page); err != nil {
			return fmt.Errorf("GitHub actions/runs: %w", err)
		}
		if report && len(page.Runs) == newestRuns && page.Runs[len(page.Runs)-1].CreatedAt.After(p.st.RunsSince) {
			// More runs were created since the last pass than the page
			// holds: read the gap, a page at a time.
			if err := p.listRunsSince(ctx, seen); err != nil {
				return err
			}
		}
		p.st.NewestRuns = p.st.NewestRuns[:0]
		for _, run := range page.Runs {
			seen[key(run.ID)] = true
			p.st.NewestRuns = append(p.st.NewestRuns, key(run.ID))
			if err := p.record(ctx, run, false, report); err != nil {
				return err
			}
		}
		p.st.RunsETag, p.st.RunsSince = newest.ETag, start
		if err := p.save(); err != nil {
			return err
		}
	}
	// A run still followed that has left the newest page: fetched by id,
	// conditionally, so one still running costs nothing after its first.
	var followed []string
	for id, mark := range p.st.Runs {
		if mark == "running" && !seen[id] {
			followed = append(followed, id)
		}
	}
	slices.Sort(followed)
	for i, id := range followed {
		if i == maxRunProbes {
			break
		}
		if err := p.probeRun(ctx, id, false, report); err != nil {
			return err
		}
	}
	p.st.pruneRuns()
	return p.save()
}

// listRunsSince reads every run created since the last pass, newest first,
// saving what it has recorded after each page.
func (p *poller) listRunsSince(ctx context.Context, seen map[string]bool) error {
	created := url.QueryEscape(">=" + p.st.RunsSince.UTC().Format(time.RFC3339))
	for page := 1; page <= runPages; page++ {
		var list runList
		path := p.repoPath(fmt.Sprintf("/actions/runs?per_page=100&page=%d&created=%s%s", page, created, p.branchQuery()))
		if err := p.c.getJSON(ctx, path, &list); err != nil {
			return err
		}
		for _, run := range list.Runs {
			seen[key(run.ID)] = true
			if err := p.record(ctx, run, false, true); err != nil {
				return err
			}
		}
		if err := p.save(); err != nil {
			return err
		}
		if len(list.Runs) < 100 {
			return nil
		}
	}
	return nil
}

// probeRun fetches one run by id, conditionally on its last ETag.
func (p *poller) probeRun(ctx context.Context, id string, named, report bool) error {
	resp, err := p.c.get(ctx, p.repoPath("/actions/runs/"+id), p.st.RunETags[id])
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		if named {
			return fmt.Errorf("the Actions run %s is not in %s: check the run option", id, p.cfg.repo())
		}
		// Deleted while followed: nothing more will come of it.
		delete(p.st.Runs, id)
		delete(p.st.RunETags, id)
		return nil
	}
	if err != nil {
		return err
	}
	if resp.NotModified {
		return nil
	}
	var run workflowRun
	if err := json.Unmarshal(resp.Body, &run); err != nil {
		return fmt.Errorf("GitHub actions/runs/%s: %w", id, err)
	}
	if err := p.record(ctx, run, named, report); err != nil {
		return err
	}
	if run.Status == "completed" && !named {
		// Done with. A re-run of it shows on the newest page while it is
		// one of the newest runs.
		delete(p.st.RunETags, id)
		return nil
	}
	p.st.RunETags[id] = resp.ETag
	return nil
}

// record notes a run's state and reports it completing. A run is reported
// once per completion: a re-run completes again, with a new updated_at.
func (p *poller) record(ctx context.Context, run workflowRun, named, report bool) error {
	id := key(run.ID)
	was := p.st.Runs[id]
	if run.Status != "completed" {
		p.st.Runs[id] = "running"
		return nil
	}
	mark := "done:" + run.UpdatedAt.UTC().Format(time.RFC3339)
	if was == mark {
		return nil
	}
	p.st.Runs[id] = mark
	switch {
	case !report:
		// Already complete at the baseline: not seen to complete.
		return nil
	case was == "" && (named || !run.UpdatedAt.After(p.st.RunsSince)):
		// Complete before this instance, or this pass, could see it run.
		return nil
	}
	var failed string
	if run.Conclusion != "success" && run.Conclusion != "skipped" {
		var err error
		if failed, err = p.failedJobs(ctx, id); err != nil {
			return err
		}
	}
	title := fmt.Sprintf("%s %s on %s", run.Name, run.Conclusion, run.HeadBranch)
	if len(run.PullRequests) > 0 {
		prs := make([]string, 0, len(run.PullRequests))
		for _, pr := range run.PullRequests {
			prs = append(prs, fmt.Sprintf("#%d", pr.Number))
		}
		title += " (" + strings.Join(prs, ", ") + ")"
	}
	return p.emit(monitors.Event{
		Kind:    "run.completed",
		Key:     "run-" + id,
		Title:   title,
		Summary: run.Event + ", " + run.DisplayTitle,
		URL:     run.HTMLURL,
		Body:    failed,
	})
}

// failedJobs is a failed run's jobs that did not succeed, with their failed
// steps.
func (p *poller) failedJobs(ctx context.Context, id string) (string, error) {
	var jobs struct {
		Jobs []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
			Steps      []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := p.c.getJSON(ctx, p.repoPath("/actions/runs/"+id+"/jobs?per_page=100"), &jobs); err != nil {
		return "", err
	}
	var out []string
	for _, job := range jobs.Jobs {
		if job.Conclusion == "success" || job.Conclusion == "skipped" {
			continue
		}
		lines := []string{job.Name + ": " + job.Conclusion}
		for _, step := range job.Steps {
			if step.Conclusion == "failure" {
				lines = append(lines, "  step: "+step.Name)
			}
		}
		lines = append(lines, "  "+job.HTMLURL)
		out = append(out, strings.Join(lines, "\n"))
	}
	return strings.Join(out, "\n"), nil
}
