package cli

import (
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func stagedPool(name string, staged bool, stage *apimodel.PoolImageStage) apimodel.Pool {
	pool := apimodel.Pool{ID: "pool_1", Name: name, ImagesStaged: apiclientgen.NewOptBool(staged)}
	if stage != nil {
		pool.ImageStage = apiclientgen.NewOptPoolImageStage(*stage)
	}
	return pool
}

// The wait ends when there is nothing left to stage.
func TestStagedEverywhere(t *testing.T) {
	if !stagedEverywhere(nil) {
		t.Fatal("a project with no pools should not be waited on: there is no host to stage onto")
	}
	if !stagedEverywhere([]apimodel.Pool{stagedPool("a", true, nil), stagedPool("b", true, nil)}) {
		t.Fatal("all staged should end the wait")
	}
	if stagedEverywhere([]apimodel.Pool{stagedPool("a", true, nil), stagedPool("b", false, nil)}) {
		t.Fatal("one unstaged pool should keep the wait")
	}
}

// What the user reads while the gigabytes arrive.
func TestStagingLineCarriesTheBytes(t *testing.T) {
	line := stagingLine([]apimodel.Pool{stagedPool("Default", false, &apimodel.PoolImageStage{
		State:          apiclientgen.PoolImageStateStaging,
		Image:          apiclientgen.NewOptString("ghcr.io/discobox-ai/discobox-harness-claude-code:v1"),
		Done:           1,
		Total:          4,
		Current:        apiclientgen.NewOptInt64(818 << 20),
		Size:           apiclientgen.NewOptInt64(1400 << 20),
		Layers:         apiclientgen.NewOptInt(40),
		LayersComplete: apiclientgen.NewOptInt(12),
	})})
	for _, want := range []string{"Downloading images (2 of 4):", "discobox-harness-claude-code:v1", "818.0 MiB", "1.4 GiB", "12/40 layers"} {
		if !strings.Contains(line, want) {
			t.Fatalf("stagingLine() = %q, missing %q", line, want)
		}
	}
}

// Before staging reports, the pool itself is usually still being built — and it
// says what it is doing. Naming the pool and stopping ("preparing Default")
// wasted the one line available on the part of the wait that is longest.
func TestStagingLineFallsBackToThePoolsOwnPhase(t *testing.T) {
	pool := stagedPool("Default", false, nil)
	pool.ProvisionProgress = apiclientgen.NewOptPoolProvisionProgress(apimodel.PoolProvisionProgress{
		Phase: apiclientgen.PoolProvisionPhaseWaitingForDocker,
	})
	pool.ProvisionProgressAt = apiclientgen.NewOptDateTime(time.Now())

	line := stagingLine([]apimodel.Pool{pool})
	// A phase with nothing downloading is setup, and which kind of setup
	// explains nothing to somebody who has never heard of any of it.
	if line != setupMessage {
		t.Fatalf("stagingLine() = %q, want %q", line, setupMessage)
	}
}

// A download during setup is worth reporting, because it is long and because
// bytes mean the same thing to everybody.
func TestStagingLineReportsASetupDownload(t *testing.T) {
	pool := stagedPool("Default", false, nil)
	pool.ProvisionProgress = apiclientgen.NewOptPoolProvisionProgress(apimodel.PoolProvisionProgress{
		Phase: apiclientgen.PoolProvisionPhasePullingPoolImage,
		Pull: apiclientgen.NewOptSandboxPullProgress(apimodel.SandboxPullProgress{
			Image:   "ghcr.io/discobox-ai/discobox-pool-agent:v1",
			Current: apiclientgen.NewOptInt64(152 << 20),
			Total:   apiclientgen.NewOptInt64(264 << 20),
		}),
	})
	pool.ProvisionProgressAt = apiclientgen.NewOptDateTime(time.Now())

	line := stagingLine([]apimodel.Pool{pool})
	for _, want := range []string{"Downloading runtime image", "152.0 MiB", "264.0 MiB"} {
		if !strings.Contains(line, want) {
			t.Fatalf("stagingLine() = %q, missing %q", line, want)
		}
	}
	// And the reference itself stays off the screen: it is called
	// "discobox-pool-agent", which is the vocabulary this is removing.
	if strings.Contains(line, "pool-agent") {
		t.Fatalf("stagingLine() = %q, leaks the image reference", line)
	}
}

// The pool's own name is an identifier the reader has never been introduced to,
// and the image references are internal spellings of the same thing. Neither
// belongs on this line. "resource pool" does: it describes what is being set up
// rather than naming which one.
func TestStagingLineNeverNamesThePool(t *testing.T) {
	cases := []apimodel.Pool{
		stagedPool("Default", false, nil),
		stagedPool("Default", false, &apimodel.PoolImageStage{
			State: apiclientgen.PoolImageStateStaging,
			Image: apiclientgen.NewOptString("ghcr.io/x/harness-shell:v1"),
			Done:  0, Total: 4,
		}),
		stagedPool("Default", false, &apimodel.PoolImageStage{
			State: apiclientgen.PoolImageStateFailed, Total: 4,
			Error: apiclientgen.NewOptString("unauthorized"),
		}),
	}
	for _, pool := range cases {
		line := stagingLine([]apimodel.Pool{pool})
		for _, forbidden := range []string{"Default", "pool-agent", "discobox-vm"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("stagingLine() = %q, leaks %q", line, forbidden)
			}
		}
	}
}

// And with nothing recorded at all, say that this is first-time setup rather
// than naming the pool and leaving the user to guess why.
func TestStagingLineBeforeAnyReportAtAll(t *testing.T) {
	line := stagingLine([]apimodel.Pool{stagedPool("Default", false, nil)})
	if line != setupMessage {
		t.Fatalf("stagingLine() = %q, want %q", line, setupMessage)
	}
}

// A failure says why. Staging retries quietly, so this line is the only place
// a user would learn it is failing.
func TestStagingLineReportsAFailure(t *testing.T) {
	line := stagingLine([]apimodel.Pool{stagedPool("Default", false, &apimodel.PoolImageStage{
		State: apiclientgen.PoolImageStateFailed,
		Total: 4,
		Error: apiclientgen.NewOptString("unauthorized"),
	})})
	if !strings.Contains(line, "unauthorized") {
		t.Fatalf("stagingLine() = %q, want the reason", line)
	}
}

// Staged pools are skipped, so the line describes what is actually outstanding.
func TestStagingLineSkipsStagedPools(t *testing.T) {
	pools := []apimodel.Pool{
		stagedPool("Done", true, nil),
		stagedPool("Busy", false, &apimodel.PoolImageStage{State: apiclientgen.PoolImageStateStaging, Done: 0, Total: 2}),
	}
	line := stagingLine(pools)
	// The unstaged one is what is described; the staged one is skipped. With no
	// name in the line, the count is what distinguishes them.
	if !strings.Contains(line, "(1 of 2)") {
		t.Fatalf("stagingLine() = %q, want the unstaged host's progress", line)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{512, "512 B"}, {1 << 10, "1.0 KiB"}, {1536 << 20, "1.5 GiB"}} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Fatalf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
