package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/x/gitutil"
)

func appliedSourceFixture() applySourceReport {
	when := time.Now().Add(-2 * time.Hour)
	return applySourceReport{
		Slug:           "primary",
		Status:         applyStatusApplied,
		HostPath:       "/home/ada/src/disco2",
		HostPathOrigin: hostDirFromSandboxOrigin,
		HostBranch:     "main",
		HostBase:       "4f3a1c2b8d90aaaabbbbccccddddeeeeffff0000",
		HostTip:        "77aa88bb99ccddddeeeeffff0000111122223333",
		SandboxDir:     "/work/disco2",
		SandboxRef:     "refs/discobox/apply/sbx_h1ssjzhp60emtc2n/primary",
		SandboxTip:     "9c1d2e3f4a5b6666777788889999aaaabbbbcccc",
		Base:           "4f3a1c2b8d90aaaabbbbccccddddeeeeffff0000",
		BaseOrigin:     baseOriginMergeBase,
		Commits: []applyCommit{{
			Commit:     "9c1d2e3f4a5b6666777788889999aaaabbbbcccc",
			HostCommit: "11aa22bb33cc4444555566667777888899990000",
			Subject:    "Fix parser panic on empty input",
			Author:     "Ada Lovelace",
			Date:       when,
		}},
	}
}

// renderApplyRun plays a whole successful apply through a printer, in the order
// applyOneSource prints it, so the tests read the report the way a reader does
// rather than a line at a time.
func renderApplyRun(printer applyPrinter) {
	report := appliedSourceFixture()
	printer.sandboxHeader(applyReport{SandboxID: "sbx_h1ssjzhp60emtc2n", SandboxName: "fix flaky pool reaper tests"}, 1)
	printer.sourceHeader(report)
	printer.note("checking the discobox working tree (git status --porcelain)")
	printer.noteDetail("clean, nothing uncommitted")
	printer.note("fetching the discobox's commits")
	printer.noteDetail("discobox tip %s", shortSHA(report.SandboxTip))
	printer.note("base %s — %s", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))
	printer.commitsToApply(report.Commits)
	printer.commitList(report.Commits)
	printer.note("cherry-picking them in a scratch worktree, then fast-forwarding local %s", applyTarget(report))
	printer.outcome(applyStatusApplied, "APPLIED %d %s to local %s",
		len(report.Commits), pluralize("commit", len(report.Commits)), applyTarget(report))
	printer.appliedList(report.Commits)
	printer.landed(report)
	printer.note("recorded on discobox %s as applied to %s", "sbx_h1ssjzhp60emtc2n", report.HostPath)
}

// plainPrinter is the printer a pipe, a file or a NO_COLOR terminal gets: the
// writer is not a terminal, so every escape the styles write is stripped on the
// way out. It is what almost every test here reads, because the report has to
// say everything it says without color.
func plainPrinter(out io.Writer) applyPrinter { return newApplyPrinter(out, true) }

// colorPrinter forces the painted path, which no test environment's stdout
// would otherwise take.
func colorPrinter(out io.Writer) applyPrinter {
	printer := newApplyPrinter(out, true)
	printer.out = &colorprofile.Writer{Forward: out, Profile: colorprofile.ANSI256}
	printer.width = 96
	return printer
}

func renderApplied(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	renderApplyRun(plainPrinter(&buf))
	return buf.String()
}

// The whole point of the command's output is that nothing about the git
// operation is implicit: both repositories, the base and why it is the base,
// and each commit's identity on both sides have to be on screen.
func TestAppliedOutputNamesBothSidesAndEveryCommit(t *testing.T) {
	out := renderApplied(t)
	for _, want := range []string{
		"── primary ",
		"local repo     /home/ada/src/disco2",
		"discobox repo  /work/disco2",
		"branch main at 4f3a1c2b8d90",
		"refs/discobox/apply/sbx_h1ssjzhp60emtc2n/primary",
		"base 4f3a1c2b8d90 — merge base of the discobox tip and local HEAD",
		"1 commit to apply",
		"9c1d2e3f4a5b  Fix parser panic on empty input  Ada Lovelace, 2 hours ago",
		"✓ APPLIED 1 commit to local main",
		"9c1d2e3f4a5b → 11aa22bb33cc  Fix parser panic on empty input",
		"local main  4f3a1c2b8d90 → 77aa88bb99cc",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// A pipe, a file and a NO_COLOR terminal all get the report with nothing in it
// but text. The whole report is written in color and the writer takes it away
// (newApplyPrinter), so this is the check that the taking-away actually covers
// every line rather than the ones somebody remembered to gate.
func TestTheReportWritesNoEscapesToAPipe(t *testing.T) {
	var buf bytes.Buffer
	printer := plainPrinter(&buf)
	renderApplyRun(printer)
	printer.caution("--allow-dirty: applying anyway")
	printer.nextSteps(dirtyNextSteps("sbx_1", "primary", "/work", nil))
	printer.detailLines([]string{" M server/main.go"})
	printer.summary(applyReport{Sources: []applySourceReport{
		{Status: applyStatusApplied}, {Status: applyStatusConflict},
	}})
	if strings.ContainsRune(buf.String(), '\x1b') {
		t.Fatalf("the report wrote an escape sequence to a plain writer:\n%q", buf.String())
	}
}

// The marks are not color, so they stay in a pipe — and the status word is
// spelled out beside them either way, so nothing about the outcome depends on
// reading a glyph.
func TestEveryOutcomeIsMarkedAndSpelledOut(t *testing.T) {
	for _, tc := range []struct {
		status applyStatus
		mark   string
	}{
		{applyStatusApplied, "✓"},
		{applyStatusUpToDate, "✓"},
		{applyStatusBlocked, "⚠"},
		{applyStatusConflict, "✗"},
		{applyStatusError, "✗"},
	} {
		var buf bytes.Buffer
		plainPrinter(&buf).outcome(tc.status, "%s: something", strings.ToUpper(string(tc.status)))
		out := buf.String()
		if !strings.Contains(out, tc.mark+" "+strings.ToUpper(string(tc.status))) {
			t.Fatalf("%s outcome = %q, want %q leading the status word", tc.status, out, tc.mark)
		}
	}
}

// Color is added to the report, never substituted for it: painting the same run
// and stripping the escapes has to give back exactly the plain report, or the
// two streams are showing different output.
func TestColorAddsNothingButColor(t *testing.T) {
	var plain, painted bytes.Buffer
	printer := plainPrinter(&plain)
	printer.width = 96 // the painted printer's, so the rules are the same length
	renderApplyRun(printer)
	renderApplyRun(colorPrinter(&painted))

	if !strings.ContainsRune(painted.String(), '\x1b') {
		t.Fatal("the painted report has no color in it")
	}
	if got := ansi.Strip(painted.String()); got != plain.String() {
		t.Fatalf("stripped color output differs from the plain report:\n%q\n%q", got, plain.String())
	}
}

// The commits are the part of the report a reader came for, so they are the
// part that is painted: gold for a SHA on either side of the apply, the
// plumbing dim around them, and the outcome in its status color.
func TestTheCommitsAndTheOutcomeAreThePaintedParts(t *testing.T) {
	var buf bytes.Buffer
	renderApplyRun(colorPrinter(&buf))
	out := buf.String()
	for what, want := range map[string]string{
		"the discobox commit":  "\x1b[38;5;" + applyColSHA + "m9c1d2e3f4a5b",
		"the local commit":     "\x1b[38;5;" + applyColSHA + "m11aa22bb33cc",
		"the plumbing":         "\x1b[38;5;" + applyColDim + "mfetching the discobox's commits",
		"the applied outcome":  applyColOK + "m✓ APPLIED",
		"the commit list head": "\x1b[1m1 commit to apply",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("%s is not painted as expected (%q):\n%q", what, want, out)
		}
	}
}

// The authors line up under each other, which is what makes a list of commits
// scannable rather than a paragraph with SHAs in it — but only while the
// aligned row fits the window. Padding a row that is about to wrap only moves
// the wrap.
func TestTheCommitListAlignsOnlyWhenTheRowFits(t *testing.T) {
	commits := []applyCommit{
		{Commit: "9c1d2e3f4a5b6666", Subject: "short one", Author: "Ada Lovelace", Date: time.Now().Add(-time.Hour)},
		{Commit: "aa1d2e3f4a5b6666", Subject: "a considerably longer commit subject", Author: "Ada Lovelace", Date: time.Now().Add(-time.Hour)},
	}

	var wide bytes.Buffer
	printer := plainPrinter(&wide)
	printer.width = 200
	printer.commitList(commits)
	if !strings.Contains(wide.String(), "short one                             Ada Lovelace") {
		t.Fatalf("a wide window did not align the attribution:\n%q", wide.String())
	}

	var narrow bytes.Buffer
	printer = plainPrinter(&narrow)
	printer.width = 60
	printer.commitList(commits)
	if !strings.Contains(narrow.String(), "short one  Ada Lovelace") {
		t.Fatalf("a narrow window padded a row that cannot fit anyway:\n%q", narrow.String())
	}
	for _, want := range []string{"9c1d2e3f4a5b", "a considerably longer commit subject", "Ada Lovelace"} {
		if !strings.Contains(narrow.String(), want) {
			t.Fatalf("a narrow window dropped %q:\n%q", want, narrow.String())
		}
	}
}

func TestBaseOriginDistinguishesRepeatApplies(t *testing.T) {
	if got := formatBaseOrigin(baseOriginLastApplied); !strings.Contains(got, "last commit applied") {
		t.Fatalf("last-applied base explained as %q", got)
	}
	if got := formatBaseOrigin(baseOriginMergeBase); !strings.Contains(got, "merge base") {
		t.Fatalf("merge-base base explained as %q", got)
	}
}

func TestPrinterIsSilentInJSONMode(t *testing.T) {
	var buf bytes.Buffer
	printer := newApplyPrinter(&buf, false)
	renderApplyRun(printer)
	printer.summary(applyReport{Sources: []applySourceReport{{Status: applyStatusApplied}, {Status: applyStatusConflict}}})
	if buf.Len() != 0 {
		t.Fatalf("JSON mode wrote progress text:\n%s", buf.String())
	}
}

func TestSummaryCountsEveryStatusForMultipleSources(t *testing.T) {
	var buf bytes.Buffer
	printer := plainPrinter(&buf)
	printer.summary(applyReport{Sources: []applySourceReport{
		{Slug: "primary", Status: applyStatusApplied},
		{Slug: "docs", Status: applyStatusUpToDate},
		{Slug: "web", Status: applyStatusConflict},
	}})
	out := buf.String()
	if !strings.Contains(out, "3 sources: 1 applied, 1 up-to-date, 1 conflict") {
		t.Fatalf("summary = %q", out)
	}
}

// A single source needs no summary: its own result lines already said it.
func TestSummarySkippedForOneSource(t *testing.T) {
	var buf bytes.Buffer
	plainPrinter(&buf).summary(applyReport{Sources: []applySourceReport{{Status: applyStatusApplied}}})
	if buf.Len() != 0 {
		t.Fatalf("single-source run printed a summary:\n%s", buf.String())
	}
}

func TestDetachedHeadIsNamedRatherThanBlank(t *testing.T) {
	report := applySourceReport{HostPath: "/srv/repo", HostBase: "4f3a1c2b8d90aaaa"}
	plain := plainPrinter(&bytes.Buffer{})
	if got := ansi.Strip(plain.branchAt(report.HostBranch, report.HostBase)); !strings.Contains(got, "detached HEAD at 4f3a1c2b8d90") {
		t.Fatalf("branchAt = %q", got)
	}
	if got := applyTarget(report); got != "/srv/repo (detached HEAD)" {
		t.Fatalf("applyTarget = %q", got)
	}
}

func TestPairHostCommitsOnlyPairsMatchingRanges(t *testing.T) {
	commits := []applyCommit{{Commit: "aaa"}, {Commit: "bbb"}}
	paired := pairHostCommits(commits, []gitutil.Commit{{SHA: "111"}, {SHA: "222"}})
	if paired[0].HostCommit != "111" || paired[1].HostCommit != "222" {
		t.Fatalf("commits not paired in order: %+v", paired)
	}

	unpaired := pairHostCommits([]applyCommit{{Commit: "aaa"}, {Commit: "bbb"}}, []gitutil.Commit{{SHA: "111"}})
	for _, commit := range unpaired {
		if commit.HostCommit != "" {
			t.Fatalf("mismatched ranges were paired anyway: %+v", unpaired)
		}
	}
}

func TestStatusLinesKeepPorcelainPrefixes(t *testing.T) {
	got := statusLines(" M cli/internal/cli/apply.go\r\n?? notes.txt\n\n")
	want := []string{" M cli/internal/cli/apply.go", "?? notes.txt"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// A blocked source has to name both ways out, and spell the re-run for this
// exact source so neither has to be reassembled by hand.
func TestDirtyNextStepsOfferBothCommitAndAllowDirty(t *testing.T) {
	steps := dirtyNextSteps("sbx_23x11jnw03w11nf2", "primary", "/work/disco2", nil)
	if len(steps) != 2 {
		t.Fatalf("got %d next steps, want 2: %+v", len(steps), steps)
	}
	if want := "discobox shell sbx_23x11jnw03w11nf2 -- git -C /work/disco2 commit -a -m MESSAGE"; steps[0].Commands[0] != want {
		t.Fatalf("commit command = %q, want %q", steps[0].Commands[0], want)
	}
	if want := "discobox apply sbx_23x11jnw03w11nf2 --source primary"; steps[0].Commands[1] != want {
		t.Fatalf("re-run command = %q, want %q", steps[0].Commands[1], want)
	}
	if want := "discobox apply sbx_23x11jnw03w11nf2 --source primary --allow-dirty"; steps[1].Commands[0] != want {
		t.Fatalf("allow-dirty command = %q, want %q", steps[1].Commands[0], want)
	}
}

// A source applied through --dir has no default local directory, so a re-run
// that dropped the override would fail the same way every time.
func TestDirtyNextStepsCarryDirOverride(t *testing.T) {
	steps := dirtyNextSteps("sbx_1", "web", "/work/web", map[string]string{"web": "/home/ada/src/web"})
	for _, step := range steps {
		for _, command := range step.Commands {
			if strings.HasPrefix(command, "discobox apply") && !strings.Contains(command, "--dir web=/home/ada/src/web") {
				t.Fatalf("re-run dropped the --dir override: %q", command)
			}
		}
	}
}

func TestNextStepsPrintDescriptionThenCommands(t *testing.T) {
	var buf bytes.Buffer
	plainPrinter(&buf).nextSteps(dirtyNextSteps("sbx_1", "primary", "/work", nil))
	out := buf.String()
	for _, want := range []string{
		"  commit them in the discobox, then apply again:",
		"    discobox apply sbx_1 --source primary\n",
		"  or apply only what is already committed, leaving them in the discobox:",
		"    discobox apply sbx_1 --source primary --allow-dirty\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// Porcelain entries and commands are data, never format strings: a path with a
// percent verb in it must survive verbatim.
func TestDetailLinesDoNotInterpretFormatVerbs(t *testing.T) {
	var buf bytes.Buffer
	plainPrinter(&buf).detailLines([]string{"?? weird%s%dname.txt"})
	if !strings.Contains(buf.String(), "?? weird%s%dname.txt") {
		t.Fatalf("line was reformatted: %q", buf.String())
	}
}

func TestBaseOriginExplainsADiscoboxThatStartedFromNothing(t *testing.T) {
	got := formatBaseOrigin(baseOriginDiscoboxBase)
	if !strings.Contains(got, "empty base") || !strings.Contains(got, "no commits") {
		t.Fatalf("discobox-base explained as %q, want it to name both the empty base and why", got)
	}
}

// A first apply refused because the local working tree moved on has exactly one
// way to finish the job — commit the local work, which the discobox's commits
// then cherry-pick on top of, since they never needed a shared history.
func TestLocalChangesNextStepsLeadWithCommittingTheLocalWork(t *testing.T) {
	steps := localChangesNextSteps("sbx_23x11jnw03w11nf2", "primary", "/home/ada/src/new", nil, true)
	if len(steps) != 2 {
		t.Fatalf("got %d next steps, want 2: %+v", len(steps), steps)
	}
	want := []string{
		"git -C /home/ada/src/new add -A",
		"git -C /home/ada/src/new commit -m MESSAGE",
		"discobox apply sbx_23x11jnw03w11nf2 --source primary",
	}
	if len(steps[0].Commands) != len(want) {
		t.Fatalf("commands = %v, want %v", steps[0].Commands, want)
	}
	for i, command := range want {
		if steps[0].Commands[i] != command {
			t.Fatalf("command %d = %q, want %q", i, steps[0].Commands[i], command)
		}
	}
}

func TestLocalChangesNextStepsCarryDirOverride(t *testing.T) {
	steps := localChangesNextSteps("sbx_1", "web", "/work/web", map[string]string{"web": "/home/ada/src/web"}, true)
	for _, step := range steps {
		for _, command := range step.Commands {
			if strings.HasPrefix(command, "discobox apply") && !strings.Contains(command, "--dir web=/home/ada/src/web") {
				t.Fatalf("re-run dropped the --dir override: %q", command)
			}
		}
	}
}

// A repository getting its first commits has no commit to name as the "from"
// half of the range, and a blank there reads as a bug rather than as a fact.
func TestAppliedFromNamesAnAbsentStartingPoint(t *testing.T) {
	if got := applyFrom(applySourceReport{}); got != "no commits" {
		t.Fatalf("applyFrom with no host base = %q, want it said in words", got)
	}
	if got := applyFrom(applySourceReport{HostBase: "11aa22bb33cc44dd55ee"}); got != "11aa22bb33cc" {
		t.Fatalf("applyFrom = %q, want the short SHA", got)
	}
}

// The guard fires for two different reasons, and only one of them is the user
// having changed something: a discobox told not to carry the working tree was
// never given those files, and saying it "has changed" accuses them of work
// they did not do.
func TestBlockedMessageDoesNotAccuseAUserWhoWithheldTheirFiles(t *testing.T) {
	changed := blockedLocalChanges("/home/ada/src/new", true)
	if !strings.Contains(changed, "has changed since this discobox was created") {
		t.Fatalf("carried workspace explained as %q", changed)
	}
	withheld := blockedLocalChanges("/home/ada/src/new", false)
	if strings.Contains(withheld, "has changed") {
		t.Fatalf("a discobox that was given nothing still says the repository changed: %q", withheld)
	}
	if !strings.Contains(withheld, "never given") {
		t.Fatalf("withheld files explained as %q, want it to say they were never carried", withheld)
	}
}

// Undoing an edit is a way out for a working tree the discobox was given.
// Doing that to files it was never given means deleting them, so that
// alternative must not be offered there.
func TestLocalChangesAlternativeNeverSuggestsDeletingWithheldFiles(t *testing.T) {
	carried := localChangesNextSteps("sbx_1", "primary", "/work/new", nil, true)
	if !strings.Contains(carried[1].Description, "put it back the way the discobox found it") {
		t.Fatalf("carried alternative = %q", carried[1].Description)
	}
	withheld := localChangesNextSteps("sbx_1", "primary", "/work/new", nil, false)
	if strings.Contains(withheld[1].Description, "put it back the way the discobox found it") {
		t.Fatalf("withheld alternative tells the user to delete their own files: %q", withheld[1].Description)
	}
	// Committing is the answer either way, and it has to stay first.
	for _, steps := range [][]applyNextStep{carried, withheld} {
		if !strings.HasPrefix(steps[0].Description, "commit the local files first") {
			t.Fatalf("first next step = %q, want committing to lead", steps[0].Description)
		}
	}
}
