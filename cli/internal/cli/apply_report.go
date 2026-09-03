package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/discobox-ai/x/gitutil"
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
	// applyStatusBlocked: the source was not attempted — the sandbox has
	// uncommitted changes, or the local working tree is no longer what the
	// sandbox was created from.
	applyStatusBlocked applyStatus = "blocked"
	// applyStatusError: anything else went wrong before or during the attempt.
	applyStatusError applyStatus = "error"
)

// baseOrigin says where the base commit — the "everything after this" point a
// source's commits are taken from — came from, since the two answers mean
// different things to whoever reads the output.
type baseOrigin string

const (
	// baseOriginLastApplied: a previous discobox apply of this source recorded
	// this commit, so only what the sandbox added since then is applied.
	baseOriginLastApplied baseOrigin = "last-applied"
	// baseOriginMergeBase: no prior apply, so the base is the common ancestor
	// of the sandbox tip and the host branch.
	baseOriginMergeBase baseOrigin = "merge-base"
	// baseOriginDiscoboxBase: the discobox was created from a repository with
	// no commits, so it starts from an empty base commit of its own and there
	// is no shared history to find a merge base in. Everything after that base
	// is the discobox's work (ADR 0084).
	baseOriginDiscoboxBase baseOrigin = "discobox-base"
)

// hostDirOrigin says how the local directory a source applies into was chosen.
// Applying onto the wrong repository is the one mistake this command cannot
// undo for the user, so the choice is reported rather than assumed.
type hostDirOrigin string

const (
	// hostDirFromOverride: named explicitly with --dir, and the sandbox's own
	// origin was not consulted.
	hostDirFromOverride hostDirOrigin = "dir-override"
	// hostDirFromSandboxOrigin: the sandbox was created on this machine, from
	// this directory, and that directory is still there.
	hostDirFromSandboxOrigin hostDirOrigin = "sandbox-origin"
)

// applyReport is the full result of one `discobox apply` invocation: what was
// looked at, what was decided, and what changed. It is the JSON payload of
// `discobox apply -o json` and the model the text output renders from, so the two
// can never describe different things.
type applyReport struct {
	SandboxID   string              `json:"sandboxId"`
	SandboxName string              `json:"sandboxName,omitempty"`
	Sources     []applySourceReport `json:"sources"`
}

type applySourceReport struct {
	Slug   string      `json:"slug"`
	Status applyStatus `json:"status"`

	// Host side: this machine, the repository discobox was run from. Printed as
	// "local", which is what a user calls it.
	HostPath string `json:"hostPath,omitempty"`
	// HostPathOrigin is how that directory was chosen.
	HostPathOrigin hostDirOrigin `json:"hostPathOrigin,omitempty"`
	HostBranch     string        `json:"hostBranch,omitempty"`
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

	// ConflictCommit is set when Status is conflict.
	ConflictCommit string `json:"conflictCommit,omitempty"`
	// UncommittedChanges is the sandbox working tree's `git status --porcelain`
	// entries, set whenever it was dirty — whether that blocked the source or
	// was applied over with --allow-dirty.
	UncommittedChanges []string `json:"uncommittedChanges,omitempty"`
	// DirtyIgnored records that --allow-dirty applied this source over those
	// uncommitted changes, which is why a dirty sandbox did not block it.
	DirtyIgnored bool `json:"dirtyIgnored,omitempty"`
	// LocalChanges is set when a first apply into a repository with no commits
	// was refused: the local working tree is no longer what the discobox was
	// created from, and these are the `git diff --name-status` entries saying
	// how.
	LocalChanges []string `json:"localChanges,omitempty"`
	// NextSteps are the ways out of a failure, each a described set of literal
	// commands ready to run by a human or an agent. More than one step means
	// alternatives, not a sequence.
	NextSteps []applyNextStep `json:"nextSteps,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type applyNextStep struct {
	Description string   `json:"description"`
	Commands    []string `json:"commands"`
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

// The report is drawn in three weights, because everything in it having the
// same weight is what made it a wall of text. The plumbing — which repositories,
// which ref, what is being fetched, where the base came from — is dim: it is
// context, and it is read only when something goes wrong. The commits are
// bright: they are the work, and they are what the reader came for. The one line
// that says how the source ended is marked and colored by its status, so the
// outcome is findable by eye in a scrolled-back terminal.
//
// Color is written unconditionally and taken away by the writer (see
// newApplyPrinter): every escape is stripped for a pipe or a file, and NO_COLOR
// on a real terminal drops the color and keeps the weight. Marks are not color
// and stay — `✓` in a pipe is still worth reading, and a status word is spelled
// out in capitals beside it so nothing depends on either.
//
// The palette is the launcher's (internal/tui/theme.go), because apply is read
// inside a pane of that window as often as in a shell, and two palettes for one
// command would be two answers to what a color means.
const (
	applyColSHA     = "220" // gold: a commit, on either side
	applyColDim     = "245" // grey: the plumbing, the labels, the attribution
	applyColOK      = "83"  // green: it landed
	applyColWarn    = "214" // amber: nothing is broken, but something is yours to do
	applyColErr     = "196" // red: it did not land
	applyColCommand = "120" // the light green the launcher prints commands in
)

var (
	applyStyleSHA     = lipgloss.NewStyle().Foreground(lipgloss.Color(applyColSHA))
	applyStyleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color(applyColDim))
	applyStyleBold    = lipgloss.NewStyle().Bold(true)
	applyStyleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color(applyColOK)).Bold(true)
	applyStyleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color(applyColWarn)).Bold(true)
	applyStyleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color(applyColErr)).Bold(true)
	applyStyleCommand = lipgloss.NewStyle().Foreground(lipgloss.Color(applyColCommand))
)

// applyPrinter writes the running account of an apply to the terminal. It is
// silent in JSON mode, where the report is printed once at the end instead.
type applyPrinter struct {
	out io.Writer
	on  bool
	// width is the terminal's, for the rule that opens a source. Zero when the
	// stream is not a terminal, and then the rule takes a fixed width: a report
	// in a log file should not change shape with whoever's window ran it.
	width int
}

// newApplyPrinter wraps the command's own output stream in a color-profile
// writer, which is where NO_COLOR and a non-terminal are honored — once, for
// every line the report prints, rather than at each call site. The writer
// strips every escape for a pipe or a file and drops color while keeping bold
// under NO_COLOR, and downsamples to what a 16-color terminal can actually
// show. CLICOLOR_FORCE still gets color into a pipe, for `less -R`.
func newApplyPrinter(out io.Writer, on bool) applyPrinter {
	// The width is measured on the real stream: the wrapper is not a file, and
	// asking it would answer 0 for every terminal.
	return applyPrinter{out: colorprofile.NewWriter(out, os.Environ()), on: on, width: terminalWidth(out)}
}

func (p applyPrinter) paint(style lipgloss.Style, text string) string {
	if text == "" {
		return text
	}
	return style.Render(text)
}

// sha is a commit, short and gold, wherever one appears. Every SHA in the
// report goes through it: they are the identifiers a reader copies out, and
// they are the same color on both sides of the apply.
func (p applyPrinter) sha(commit string) string {
	return p.paint(applyStyleSHA, shortSHA(commit))
}

func (p applyPrinter) dim(text string) string  { return p.paint(applyStyleDim, text) }
func (p applyPrinter) bold(text string) string { return p.paint(applyStyleBold, text) }

func (p applyPrinter) linef(format string, args ...any) {
	if !p.on {
		return
	}
	fmt.Fprintf(p.out, format+"\n", args...)
}

// blank separates the three parts of a source — where it stands, the commits,
// how it ended — because a blank line is what stops the account reading as one
// paragraph.
func (p applyPrinter) blank() { p.linef("") }

// step is an action or a fact, indented under its source heading.
func (p applyPrinter) step(format string, args ...any) {
	p.linef("  "+format, args...)
}

// detail is subordinate to the step above it: a commit, a changed file, a
// command to run.
func (p applyPrinter) detail(format string, args ...any) {
	p.linef(strings.Repeat(" ", applyDetailIndent)+format, args...)
}

// note is the plumbing: what is being done, and to which repository. Dim as a
// whole, because it is the half of the output that is only read when something
// has gone wrong.
func (p applyPrinter) note(format string, args ...any) {
	p.step("%s", p.dim(fmt.Sprintf(format, args...)))
}

// noteDetail is a plumbing line subordinate to the one above it.
func (p applyPrinter) noteDetail(format string, args ...any) {
	p.detail("%s", p.dim(fmt.Sprintf(format, args...)))
}

// mark prints a line led by a glyph in a color: the shape every outcome takes.
func (p applyPrinter) mark(glyph string, style lipgloss.Style, format string, args ...any) {
	p.step("%s", p.paint(style, glyph+" "+fmt.Sprintf(format, args...)))
}

// caution is a line about something the reader chose that the report will not
// let pass silently — `--allow-dirty` leaving work behind. Marked like an
// outcome because it is a caveat on one, and it is not one: the source has not
// ended yet.
func (p applyPrinter) caution(format string, args ...any) {
	p.mark("⚠", applyStyleWarn, format, args...)
}

// outcome is the one line that says how a source ended, marked and colored by
// its status. The status word itself is in the message, in capitals: a reader
// scrolling back finds it by eye where there is color and by shape where there
// is not.
func (p applyPrinter) outcome(status applyStatus, format string, args ...any) {
	p.blank()
	glyph, style := applyStatusMark(status)
	p.mark(glyph, style, format, args...)
}

// applyStatusMark is how each ending is drawn. Applied is green and up-to-date
// is grey: both are fine, and only one of them changed anything. Blocked is
// amber rather than red because nothing is broken — there is something for the
// reader to do — while a conflict and an error are the two ways the commits did
// not land.
func applyStatusMark(status applyStatus) (string, lipgloss.Style) {
	switch status {
	case applyStatusApplied:
		return "✓", applyStyleOK
	case applyStatusUpToDate:
		return "✓", applyStyleDim
	case applyStatusBlocked:
		return "⚠", applyStyleWarn
	case applyStatusConflict, applyStatusError:
		return "✗", applyStyleErr
	}
	return "·", applyStyleDim
}

func (p applyPrinter) sandboxHeader(report applyReport, sourceCount int) {
	line := "discobox " + p.bold(report.SandboxID)
	if report.SandboxName != "" {
		line += "  " + report.SandboxName
	}
	p.linef("%s  %s", line, p.dim(fmt.Sprintf("· applying %d %s", sourceCount, pluralize("source", sourceCount))))
}

// bareSourceHeader opens a source that failed before its repositories could
// even be identified, so its error still appears under a heading like every
// other source.
func (p applyPrinter) bareSourceHeader(slug string) {
	p.blank()
	p.rule(slug)
}

// rule is the titled line that opens a source: the slug is the only thing on
// the row, so several sources in one run read as sections rather than as a
// scroll.
func (p applyPrinter) rule(label string) {
	head := "── " + label + " "
	tail := max(p.ruleWidth()-runeLen(head), 3)
	p.linef("%s%s%s", p.dim("── "), p.bold(label), p.dim(" "+strings.Repeat("─", tail)))
}

// ruleWidth is how wide the source rule is drawn: the window, up to a length
// that stays readable on a wide one, and a fixed width where there is no window
// to measure.
func (p applyPrinter) ruleWidth() int {
	if p.width <= 0 {
		return 72
	}
	return min(max(p.width, 24), 100)
}

// sourceHeader states the two repositories involved and where each one stands.
// Every label is dim and every value is not: the labels are read once and the
// values are what a reader is looking for.
func (p applyPrinter) sourceHeader(report applySourceReport) {
	p.bareSourceHeader(report.Slug)
	p.field("local repo", report.HostPath+p.branchAt(report.HostBranch, report.HostBase))
	if report.SandboxDir != "" {
		p.field("discobox repo", report.SandboxDir)
	}
	p.field("fetch ref", report.SandboxRef)
	p.field("chosen by", formatHostDirOrigin(report))
}

// field is a label/value row of the source heading, on one aligned column. The
// label is painted and the column padding is not: an escape wrapped round
// trailing spaces is a run of colored blanks in a terminal that draws
// backgrounds.
func (p applyPrinter) field(label, value string) {
	if value == "" {
		// A label with nothing after it is a row that says only that the
		// report has a field for this.
		return
	}
	p.step("%s%s  %s", p.dim(label), padding(label, applyFieldWidth), value)
}

// applyFieldWidth is the label column, sized to the longest label there is.
const applyFieldWidth = len("discobox repo")

// branchAt is where a repository is sitting, for the end of its heading row.
func (p applyPrinter) branchAt(branch, commit string) string {
	switch {
	case branch != "" && commit != "":
		return p.dim("  branch ") + branch + p.dim(" at ") + p.sha(commit)
	case branch != "":
		return p.dim("  branch ") + branch
	case commit != "":
		return p.dim("  detached HEAD at ") + p.sha(commit)
	}
	return ""
}

// commitsToApply heads the commit list, which is the part of the report the
// reader came for: what is about to land, or what would have.
func (p applyPrinter) commitsToApply(commits []applyCommit) {
	p.blank()
	p.step("%s", p.bold(fmt.Sprintf("%d %s to apply", len(commits), pluralize("commit", len(commits)))))
}

func (p applyPrinter) commitList(commits []applyCommit) {
	width := p.subjectColumn(commits)
	for _, commit := range commits {
		subject := truncateTableValue(commit.Subject, applyMaxSubject)
		attribution := commitAttribution(commit)
		if attribution == "" {
			p.detail("%s  %s", p.sha(commit.Commit), subject)
			continue
		}
		p.detail("%s  %s  %s", p.sha(commit.Commit), padRight(subject, width), p.dim(attribution))
	}
}

// subjectColumn is the width the subjects are padded to, so the authors line up
// under each other instead of stepping across the screen: the longest subject
// in the list, or no padding at all when the aligned row would not fit the
// window anyway. Padding a row that is about to wrap only moves the wrap.
func (p applyPrinter) subjectColumn(commits []applyCommit) int {
	width := applySubjectWidth(commits)
	if p.width <= 0 {
		return width
	}
	longest := 0
	for _, commit := range commits {
		longest = max(longest, runeLen(commitAttribution(commit)))
	}
	// The detail indent, the short SHA, and the two gaps around the subject.
	if applyDetailIndent+len("0123456789ab")+2+width+2+longest > p.width {
		return 0
	}
	return width
}

// applyDetailIndent is how far a detail line — a commit, a command, a changed
// path — sits in from the left.
const applyDetailIndent = 4

// appliedList shows the sandbox commit → local commit mapping, which is the
// answer to "what are these new commits on my branch".
func (p applyPrinter) appliedList(commits []applyCommit) {
	for _, commit := range commits {
		if commit.HostCommit == "" {
			p.detail("%s  %s", p.sha(commit.Commit), truncateTableValue(commit.Subject, applyMaxSubject))
			continue
		}
		p.detail("%s %s %s  %s", p.sha(commit.Commit), p.dim("→"), p.sha(commit.HostCommit),
			truncateTableValue(commit.Subject, applyMaxSubject-12))
	}
}

// landed is where the local branch ended up: the range a reader hands to
// `git log` or `git show`. It sits under the applied commits rather than on the
// status line above them, which is about how many commits went where.
func (p applyPrinter) landed(report applySourceReport) {
	from := p.sha(report.HostBase)
	if report.HostBase == "" {
		// A branch that had no commits before this apply; applyFrom says so in
		// words, since there is no SHA to name.
		from = p.dim(applyFrom(report))
	}
	p.detail("%s  %s %s %s", p.dim("local "+applyTarget(report)), from, p.dim("→"), p.sha(report.HostTip))
}

// applyMaxSubject is how much of a commit subject is printed. A subject is one
// line by convention and a long one is usually a body that was never wrapped,
// so it is cut rather than allowed to wrap the report.
const applyMaxSubject = 72

// applySubjectWidth is the column the attribution starts at: the longest
// subject in the list, so the authors line up under each other instead of
// stepping across the screen.
func applySubjectWidth(commits []applyCommit) int {
	width := 0
	for _, commit := range commits {
		width = max(width, runeLen(truncateTableValue(commit.Subject, applyMaxSubject)))
	}
	return width
}

// nextSteps prints the ways out of a failure: the description, then the literal
// commands, in the color the launcher prints commands in — they are the part of
// a failed report meant to be copied.
func (p applyPrinter) nextSteps(steps []applyNextStep) {
	for _, step := range steps {
		p.step("%s", p.dim(step.Description+":"))
		for _, command := range step.Commands {
			p.detail("%s", p.paint(applyStyleCommand, command))
		}
	}
}

// detailLines prints already-formatted lines — porcelain status entries —
// without treating their contents as a format string. They keep their exact
// prefixes and take no color: ` M` and `??` are git's own two columns, and a
// reader matching them against `git status` output must see them unchanged.
func (p applyPrinter) detailLines(lines []string) {
	for _, line := range lines {
		p.detail("%s", line)
	}
}

// summary closes a multi-source run with the per-status counts, so the outcome
// does not have to be reconstructed by reading back through the whole log. Each
// count is drawn in its own status color, which makes "2 applied, 1 conflict"
// answerable at a glance.
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
		if counts[status] == 0 {
			continue
		}
		_, style := applyStatusMark(status)
		parts = append(parts, p.paint(style, fmt.Sprintf("%d %s", counts[status], status)))
	}
	p.blank()
	p.linef("%s %s", p.dim(fmt.Sprintf("%d %s:", len(report.Sources), pluralize("source", len(report.Sources)))),
		strings.Join(parts, p.dim(", ")))
}

// formatHostDirOrigin states which check put the apply in this directory, so
// "why is it touching this repository" never has to be inferred.
func formatHostDirOrigin(report applySourceReport) string {
	switch report.HostPathOrigin {
	case hostDirFromOverride:
		return fmt.Sprintf("--dir %s=%s (the discobox's own origin was not consulted)", report.Slug, report.HostPath)
	case hostDirFromSandboxOrigin:
		return "the discobox's origin: it was created on this machine, from this directory"
	}
	return string(report.HostPathOrigin)
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
		return "merge base of the discobox tip and local HEAD"
	case baseOriginDiscoboxBase:
		return "the empty base this discobox started from; local had no commits"
	}
	return string(origin)
}

// commitAttribution is who wrote a commit and when, for the dim column after
// its subject. Empty when the report carries neither, so nothing prints an
// empty pair of parentheses.
func commitAttribution(commit applyCommit) string {
	var parts []string
	if commit.Author != "" {
		parts = append(parts, commit.Author)
	}
	if !commit.Date.IsZero() {
		parts = append(parts, formatTime(commit.Date))
	}
	return strings.Join(parts, ", ")
}

// padRight pads text out to a column, measured in runes: the report's own
// columns are its alignment, and a padded value must not be cut.
func padRight(text string, width int) string {
	return text + padding(text, width)
}

// padding is the blanks that carry text to a column, for the callers that paint
// the text and must not paint them.
func padding(text string, width int) string {
	if gap := width - runeLen(text); gap > 0 {
		return strings.Repeat(" ", gap)
	}
	return ""
}

func quoteSubject(subject string) string {
	subject = truncateTableValue(subject, applyMaxSubject)
	if subject == "" {
		return ""
	}
	return fmt.Sprintf(" %q", subject)
}
