package sandboxcreate

import (
	"strings"
	"testing"
)

func TestAGitMeterBecomesTheStepItBelongsTo(t *testing.T) {
	for meter, want := range map[string]Step{
		"Receiving objects:  42% (1234/2938), 12.30 MiB | 1.20 MiB/s": "client fetching source: receiving objects 42%, 12.30 MiB at 1.20 MiB/s",
		"Receiving objects:   3% (89/2938)":                           "client fetching source: receiving objects 3%",
		"remote: Compressing objects: 100% (87/87), done.":            "client fetching source: compressing objects 100%",
		"Resolving deltas:  67% (804/1200)":                           "client fetching source: resolving deltas 67%",
		"Writing objects: 100% (30/30), 4.52 KiB | 4.52 MiB/s, done.": "client fetching source: writing objects 100%, 4.52 KiB at 4.52 MiB/s",
	} {
		got, ok := progressStep(StepFetchingSource, meter)
		if !ok || got != want {
			t.Errorf("progressStep(%q) = %q, %v; want %q", meter, got, ok, want)
		}
	}
	for _, line := range []string{"From https://github.com/acme/widget", " * [new branch]      main -> origin/main", "fatal: unable to access"} {
		if _, ok := progressStep(StepFetchingSource, line); ok {
			t.Errorf("%q was taken for a meter", line)
		}
	}
}

// A fetch says how far it has got, and a failed one says what git said, not
// its meters.
func TestATransferReportsItsProgressAndKeepsGitsWordsForTheError(t *testing.T) {
	work := t.TempDir()
	deliverGit(t, work, "init", "--quiet", "--initial-branch=main")
	deliverCommit(t, work, "a")
	bare := t.TempDir()
	deliverGit(t, bare, "init", "--quiet", "--bare")

	var steps []Step
	report := Report(func(step Step) { steps = append(steps, step) })
	if err := gitTransfer(t.Context(), bare, nil, StepFetchingSource, report, "fetch", "--progress", "file://"+work, "main"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(steps) == 0 || !strings.HasPrefix(string(steps[0]), "client fetching source: ") {
		t.Fatalf("a fetch reported %v, want its progress", steps)
	}
	err := gitTransfer(t.Context(), bare, nil, StepFetchingSource, nil, "fetch", "--progress", "file://"+work+"-absent", "main")
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("a failed fetch said %v, want what git said", err)
	}
}
