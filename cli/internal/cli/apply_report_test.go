package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/internal/gitutil"
)

func appliedSourceFixture() applySourceReport {
	when := time.Now().Add(-2 * time.Hour)
	return applySourceReport{
		Slug:       "primary",
		Status:     applyStatusApplied,
		HostPath:   "/home/ada/src/disco2",
		HostBranch: "main",
		HostBase:   "4f3a1c2b8d90aaaabbbbccccddddeeeeffff0000",
		HostTip:    "77aa88bb99ccddddeeeeffff0000111122223333",
		SandboxDir: "/work/disco2",
		SandboxRef: "refs/discobox/apply/sbx_h1ssjzhp60emtc2n/primary",
		SandboxTip: "9c1d2e3f4a5b6666777788889999aaaabbbbcccc",
		Base:       "4f3a1c2b8d90aaaabbbbccccddddeeeeffff0000",
		BaseOrigin: baseOriginMergeBase,
		Commits: []applyCommit{{
			Commit:     "9c1d2e3f4a5b6666777788889999aaaabbbbcccc",
			HostCommit: "11aa22bb33cc4444555566667777888899990000",
			Subject:    "Fix parser panic on empty input",
			Author:     "Ada Lovelace",
			Date:       when,
		}},
	}
}

func renderApplied(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	printer := applyPrinter{out: &buf, on: true}
	report := appliedSourceFixture()
	printer.sourceHeader(report)
	printer.step("base %s (%s)", shortSHA(report.Base), formatBaseOrigin(report.BaseOrigin))
	printer.commitList(report.Commits)
	printer.appliedList(report.Commits)
	return buf.String()
}

// The whole point of the command's output is that nothing about the git
// operation is implicit: both repositories, the base and why it is the base,
// and each commit's identity on both sides have to be on screen.
func TestAppliedOutputNamesBothSidesAndEveryCommit(t *testing.T) {
	out := renderApplied(t)
	for _, want := range []string{
		"==> source primary",
		"local repo    /home/ada/src/disco2",
		"sandbox repo  /work/disco2",
		"branch main at 4f3a1c2b8d90",
		"refs/discobox/apply/sbx_h1ssjzhp60emtc2n/primary",
		"base 4f3a1c2b8d90 (merge base of the sandbox tip and local HEAD)",
		"9c1d2e3f4a5b  Fix parser panic on empty input  (Ada Lovelace, 2 hours ago)",
		"9c1d2e3f4a5b -> 11aa22bb33cc  Fix parser panic on empty input",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
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
	printer := applyPrinter{out: &buf, on: false}
	printer.sourceHeader(appliedSourceFixture())
	printer.step("something happened")
	printer.summary(applyReport{Sources: []applySourceReport{{Status: applyStatusApplied}, {Status: applyStatusConflict}}})
	if buf.Len() != 0 {
		t.Fatalf("JSON mode wrote progress text:\n%s", buf.String())
	}
}

func TestSummaryCountsEveryStatusForMultipleSources(t *testing.T) {
	var buf bytes.Buffer
	printer := applyPrinter{out: &buf, on: true}
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
	applyPrinter{out: &buf, on: true}.summary(applyReport{Sources: []applySourceReport{{Status: applyStatusApplied}}})
	if buf.Len() != 0 {
		t.Fatalf("single-source run printed a summary:\n%s", buf.String())
	}
}

func TestDetachedHeadIsNamedRatherThanBlank(t *testing.T) {
	report := applySourceReport{HostPath: "/srv/repo", HostBase: "4f3a1c2b8d90aaaa"}
	if got := formatBranchAt(report.HostBranch, report.HostBase); !strings.Contains(got, "detached HEAD at 4f3a1c2b8d90") {
		t.Fatalf("formatBranchAt = %q", got)
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
