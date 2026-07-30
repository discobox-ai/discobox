package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/obot-platform/discobox/internal/gitutil"
)

// applyStatus is what happened to one source in an apply run. Every source
// ends in exactly one of these, and the value is the same string in text and
// JSON output so a caller can key off it either way.
type applyStatus string

const (
	// applyStatusApplied: commits were cherry-picked and the host branch
	// fast-forwarded onto them.
	applyStatusApplied applyStatus = "applied"
	// applyStatusUpToDate: the sandbox has no commits the host does not have.
	applyStatusUpToDate applyStatus = "up-to-date"
	// applyStatusConflict: a commit did not apply cleanly; the host repository
	// is unchanged.
	applyStatusConflict applyStatus = "conflict"
	// applyStatusBlocked: the source was not attempted because the sandbox has
	// uncommitted changes.
	applyStatusBlocked applyStatus = "blocked"
	// applyStatusError: anything else went wrong before or during the attempt.
	applyStatusError applyStatus = "error"
)

// baseOrigin says where the base commit — the "everything after this" point a
// source's commits are taken from — came from, since the two answers mean
// different things to whoever reads the output.
type baseOrigin string

const (
	// baseOriginLastApplied: a previous disco apply of this source recorded
	// this commit, so only what the sandbox added since then is applied.
	baseOriginLastApplied baseOrigin = "last-applied"
	// baseOriginMergeBase: no prior apply, so the base is the common ancestor
	// of the sandbox tip and the host branch.
	baseOriginMergeBase baseOrigin = "merge-base"
)

// applyReport is the full result of one `disco apply` invocation: what was
// looked at, what was decided, and what changed. It is the JSON payload of
// `disco apply -o json` and the model the text output renders from, so the two
// can never describe different things.
type applyReport struct {
	SandboxID   string              `json:"sandboxId"`
	SandboxName string              `json:"sandboxName,omitempty"`
	Sources     []applySourceReport `json:"sources"`
}

type applySourceReport struct {
	Slug   string      `json:"slug"`
	Status applyStatus `json:"status"`

	// Host side: this machine, the repository disco was run from. Printed as
	// "local", which is what a user calls it.
	HostPath   string `json:"hostPath,omitempty"`
	HostBranch string `json:"hostBranch,omitempty"`
	// HostBase is the host commit the branch was on before this apply, and
	// still is unless Status is applied.
	HostBase string `json:"hostBase,omitempty"`
	// HostTip is the host commit the branch ends on; set when applied.
	HostTip string `json:"hostTip,omitempty"`

	// Sandbox side.
	SandboxDir string `json:"sandboxDir,omitempty"`
	SandboxRef string `json:"sandboxRef,omitempty"`
	SandboxTip string `json:"sandboxTip,omitempty"`

	// Range.
	Base       string     `json:"base,omitempty"`
	BaseOrigin baseOrigin `json:"baseOrigin,omitempty"`
	// Commits are the sandbox commits in base..tip, oldest first, each
	// carrying the host commit it became once applied.
	Commits []applyCommit `json:"commits,omitempty"`

	// Set on the failure statuses.
	ConflictCommit     string   `json:"conflictCommit,omitempty"`
	UncommittedChanges []string `json:"uncommittedChanges,omitempty"`
	// NextSteps are literal commands that resolve the failure, ready to run
	// by a human or an agent.
	NextSteps []string `json:"nextSteps,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type applyCommit struct {
	Commit     string    `json:"commit"`
	HostCommit string    `json:"hostCommit,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	Author     string    `json:"author,omitempty"`
	Date       time.Time `json:"date,omitempty"`
}

func applyCommits(commits []gitutil.Commit) []applyCommit {
	out := make([]applyCommit, 0, len(commits))
	for _, commit := range commits {
		out = append(out, applyCommit{
			Commit:  commit.SHA,
			Subject: commit.Subject,
			Author:  commit.Author,
			Date:    commit.Date,
		})
	}
	return out
}

// pairHostCommits fills in the host commit each sandbox commit became. A clean
// cherry-pick of a range replays every commit in order, so the two lists line
// up one-to-one; if git ever disagrees (a dropped empty commit, say) the
// sandbox commits are still reported, just without their counterparts, rather
// than paired up wrongly.
func pairHostCommits(commits []applyCommit, hostCommits []gitutil.Commit) []applyCommit {
	if len(commits) != len(hostCommits) {
		return commits
	}
	for i := range commits {
		commits[i].HostCommit = hostCommits[i].SHA
	}
	return commits
}

// applyPrinter writes the running account of an apply to the terminal. It is
// silent in JSON mode, where the report is printed once at the end instead.
type applyPrinter struct {
	out io.Writer
	on  bool
}

func (p applyPrinter) linef(format string, args ...any) {
	if !p.on {
		return
	}
	fmt.Fprintf(p.out, format+"\n", args...)
}

// step is an action being taken, indented under its source heading.
func (p applyPrinter) step(format string, args ...any) {
	p.linef("    "+format, args...)
}

// detail is subordinate to the step above it: a commit, a changed file, a
// command to run.
func (p applyPrinter) detail(format string, args ...any) {
	p.linef("      "+format, args...)
}

func (p applyPrinter) sandboxHeader(report applyReport, sourceCount int) {
	name := report.SandboxName
	if name != "" {
		name = " (" + name + ")"
	}
	p.linef("sandbox %s%s: applying %d %s", report.SandboxID, name, sourceCount, pluralize("source", sourceCount))
}

// bareSourceHeader opens a source that failed before its repositories could
// even be identified, so its error still appears under a heading like every
// other source.
func (p applyPrinter) bareSourceHeader(slug string) {
	p.linef("")
	p.linef("==> source %s", slug)
}

// sourceHeader states the two repositories involved and where each one stands.
func (p applyPrinter) sourceHeader(report applySourceReport) {
	p.bareSourceHeader(report.Slug)
	p.step("local repo    %s%s", report.HostPath, formatBranchAt(report.HostBranch, report.HostBase))
	if report.SandboxDir != "" {
		p.step("sandbox repo  %s", report.SandboxDir)
	}
	p.step("fetch ref     %s", report.SandboxRef)
}

func (p applyPrinter) commitList(commits []applyCommit) {
	for _, commit := range commits {
		p.detail("%s  %s", shortSHA(commit.Commit), formatCommitSubject(commit))
	}
}

// appliedList shows the sandbox commit → host commit mapping, which is the
// answer to "what are these new commits on my branch".
func (p applyPrinter) appliedList(commits []applyCommit) {
	for _, commit := range commits {
		if commit.HostCommit == "" {
			p.detail("%s  %s", shortSHA(commit.Commit), truncateTableValue(commit.Subject, 72))
			continue
		}
		p.detail("%s -> %s  %s", shortSHA(commit.Commit), shortSHA(commit.HostCommit), truncateTableValue(commit.Subject, 60))
	}
}

func (p applyPrinter) nextSteps(steps []string) {
	if len(steps) == 0 {
		return
	}
	for _, step := range steps {
		p.detail("%s", step)
	}
}

// summary closes a multi-source run with the per-status counts, so the outcome
// does not have to be reconstructed by reading back through the whole log.
func (p applyPrinter) summary(report applyReport) {
	if len(report.Sources) < 2 {
		return
	}
	counts := map[applyStatus]int{}
	for _, source := range report.Sources {
		counts[source.Status]++
	}
	var parts []string
	for _, status := range []applyStatus{applyStatusApplied, applyStatusUpToDate, applyStatusConflict, applyStatusBlocked, applyStatusError} {
		if counts[status] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[status], status))
		}
	}
	p.linef("")
	p.linef("%d %s: %s", len(report.Sources), pluralize("source", len(report.Sources)), strings.Join(parts, ", "))
}

func formatBranchAt(branch, commit string) string {
	switch {
	case branch != "" && commit != "":
		return fmt.Sprintf(" (branch %s at %s)", branch, shortSHA(commit))
	case branch != "":
		return fmt.Sprintf(" (branch %s)", branch)
	case commit != "":
		return fmt.Sprintf(" (detached HEAD at %s)", shortSHA(commit))
	}
	return ""
}

// formatBaseOrigin explains, in the output itself, why the apply starts where
// it does — the difference between "everything since we last applied" and
// "everything since the two histories diverged" changes what a reader should
// expect to see applied.
func formatBaseOrigin(origin baseOrigin) string {
	switch origin {
	case baseOriginLastApplied:
		return "last commit applied from this source"
	case baseOriginMergeBase:
		return "merge base of the sandbox tip and local HEAD"
	}
	return string(origin)
}

func formatCommitSubject(commit applyCommit) string {
	subject := truncateTableValue(commit.Subject, 72)
	var attribution []string
	if commit.Author != "" {
		attribution = append(attribution, commit.Author)
	}
	if !commit.Date.IsZero() {
		attribution = append(attribution, formatTime(commit.Date))
	}
	if len(attribution) == 0 {
		return subject
	}
	return fmt.Sprintf("%s  (%s)", subject, strings.Join(attribution, ", "))
}

func quoteSubject(subject string) string {
	subject = truncateTableValue(subject, 72)
	if subject == "" {
		return ""
	}
	return fmt.Sprintf(" %q", subject)
}
