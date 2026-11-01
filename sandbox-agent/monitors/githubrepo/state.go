package githubrepo

import (
	"sort"
	"time"
)

// stateFile is the instance's whole memory, in its state directory.
const stateFile = "github-repo.json"

// state is what one run leaves the next. It is written after each step that
// changes it — each page of a catch-up included — so a run ended part way,
// by a rate limit or anything else, keeps what it did.
type state struct {
	// Since is the GitHub time the last complete pass over issue and PR
	// activity began; zero before the baseline.
	Since time.Time `json:"since"`
	// Baseline is the GitHub time of the first run. Nothing before it is
	// reported, however far a search reaches back.
	Baseline time.Time `json:"baseline"`
	// NotBefore is when the next run may call GitHub: a rate limit's reset,
	// or an X-Poll-Interval.
	NotBefore time.Time `json:"notBefore,omitzero"`

	// Probe ETags. Each is saved only once what its change calls for is done,
	// so a pass cut short is seen as a change again.
	IssuesETag string `json:"issuesETag,omitempty"`
	RunsETag   string `json:"runsETag,omitempty"`

	// LastChange is the GitHub time the issues probe last saw a change.
	LastChange time.Time `json:"lastChange,omitzero"`
	// Walk is a pass over activity in progress: where its search has got to.
	Walk *walk `json:"walk,omitempty"`
	// Reported is each activity item reported (a comment, a review, a state
	// change, an opening) by id, with its time, kept for as long as a search
	// can still return it: the window overlaps the last so a lagging search
	// index misses nothing, and these keep the overlap from repeating it.
	Reported map[string]time.Time `json:"reported,omitempty"`

	// PRs are the watched pull requests: named, or selected by head and
	// author.
	PRs map[string]*prState `json:"prs,omitempty"`
	// Issues are the named issues' states.
	Issues map[string]string `json:"issues,omitempty"`
	// Recheck forces the watched PRs' query on the next run, while GitHub has
	// yet to compute a mergeable state it was asked for.
	Recheck int `json:"recheck,omitempty"`

	// Runs are Actions runs this instance has seen: "running", or
	// "done:<updated_at>" once reported complete.
	Runs map[string]string `json:"runs,omitempty"`
	// NewestRuns are the runs on the newest page the probe last read, whose
	// changes the probe sees without fetching them one by one.
	NewestRuns []string `json:"newestRuns,omitempty"`
	// RunETags are the conditional probes of runs fetched one by one: named
	// ones, and tracked ones that have left the newest page.
	RunETags map[string]string `json:"runETags,omitempty"`
	// RunsSince is the GitHub time the last complete pass over Actions runs
	// began.
	RunsSince time.Time `json:"runsSince,omitzero"`
}

// walk is a search over activity, ascending by update, cut into pages.
type walk struct {
	// From is the update time the next page starts at.
	From time.Time `json:"from"`
	// After is the search's cursor, for a page that could not move From on:
	// every item of it updated in the same second.
	After string `json:"after,omitempty"`
	// Until is the GitHub time the pass began, which Since becomes when it
	// completes.
	Until time.Time `json:"until"`
}

type prState struct {
	State      string `json:"state,omitempty"`
	Head       string `json:"head,omitempty"`
	HeadSHA    string `json:"headSha,omitempty"`
	ChecksETag string `json:"checksEtag,omitempty"`
	StatusETag string `json:"statusEtag,omitempty"`
	// Checks is the conclusion reported for HeadSHA: success, failure,
	// pending or none.
	Checks    string `json:"checks,omitempty"`
	ChecksSHA string `json:"checksSha,omitempty"`
	// Mergeable is the last known MERGEABLE or CONFLICTING; empty until
	// GitHub has computed one.
	Mergeable string `json:"mergeable,omitempty"`
	// Author is the PR's author, with Head what selects it.
	Author string `json:"author,omitempty"`
	// Quiet marks a PR that was already there when it began to be watched —
	// at the baseline, or named since — whose checks and mergeability are
	// recorded the first time they are known, not reported.
	Quiet bool `json:"quiet,omitempty"`
}

func (s *state) init() {
	if s.Reported == nil {
		s.Reported = map[string]time.Time{}
	}
	if s.PRs == nil {
		s.PRs = map[string]*prState{}
	}
	if s.Issues == nil {
		s.Issues = map[string]string{}
	}
	if s.Runs == nil {
		s.Runs = map[string]string{}
	}
	if s.RunETags == nil {
		s.RunETags = map[string]string{}
	}
}

// pruneReported forgets reported items older than the search can return.
// cutoff is the time activity must be after to be reported: the overlap
// before the last pass, and never before the baseline.
func (s *state) cutoff() time.Time {
	return later(s.Since.Add(-overlap), s.Baseline)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *state) pruneReported() {
	cutoff := s.Since.Add(-overlap - time.Minute)
	for id, at := range s.Reported {
		if at.Before(cutoff) {
			delete(s.Reported, id)
		}
	}
}

// keepRuns is how many completed runs are remembered, newest first: enough
// to recognize one seen again on the newest page.
const keepRuns = 200

func (s *state) pruneRuns() {
	type done struct{ id, mark string }
	var finished []done
	for id, mark := range s.Runs {
		if mark != "running" {
			finished = append(finished, done{id, mark})
		}
	}
	if len(finished) <= keepRuns {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].mark > finished[j].mark })
	for _, d := range finished[keepRuns:] {
		delete(s.Runs, d.id)
		delete(s.RunETags, d.id)
	}
}
