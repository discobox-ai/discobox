package sourceconverge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// git redraws a meter in place with carriage returns and ends it with a
// newline; every redraw is a report, the stage resets the counts, and the
// bytes received outlast the stage that received them.
func TestProgressWriterReadsGitsMeters(t *testing.T) {
	var got []CloneProgress
	w := &progressWriter{report: func(p CloneProgress) { got = append(got, p) }}
	stderr := "Cloning into '/w/clone'...\n" +
		"remote: Enumerating objects: 35477, done.\n" +
		"remote: Counting objects:  50% (10/20)\rremote: Counting objects: 100% (20/20), done.\n" +
		"Receiving objects:   1% (355/35477)\rReceiving objects:  50% (17739/35477), 14.90 MiB | 5.00 MiB/s\r" +
		"Receiving objects: 100% (35477/35477), 27.60 MiB | 5.00 MiB/s, done.\n" +
		"Resolving deltas:  10% (25"
	// Split mid-line, as a pipe may.
	for _, chunk := range []string{stderr[:40], stderr[40:]} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write([]byte("76/25762)\n")); err != nil {
		t.Fatal(err)
	}
	want := []CloneProgress{
		{Stage: CloneCounting, Objects: 35477},
		{Stage: CloneCounting, Objects: 10, ObjectsTotal: 20},
		{Stage: CloneCounting, Objects: 20, ObjectsTotal: 20},
		{Stage: CloneReceiving, Objects: 355, ObjectsTotal: 35477},
		{Stage: CloneReceiving, Objects: 17739, ObjectsTotal: 35477, Bytes: progressBytes("14.90", "MiB")},
		{Stage: CloneReceiving, Objects: 35477, ObjectsTotal: 35477, Bytes: progressBytes("27.60", "MiB")},
		{Stage: CloneResolving, Objects: 2576, ObjectsTotal: 25762, Bytes: progressBytes("27.60", "MiB")},
	}
	if len(got) != len(want) {
		t.Fatalf("reports = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("report %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if messages := w.String(); messages != "Cloning into '/w/clone'...\n" {
		t.Fatalf("messages = %q, want only git's own words, the meters left out", messages)
	}
}

// A failed clone is explained by what git said, not by its meters.
func TestProgressWriterKeepsMessagesForTheError(t *testing.T) {
	w := &progressWriter{}
	_, _ = w.Write([]byte("Receiving objects:  50% (1/2)\rfatal: early EOF"))
	if got := w.String(); got != "fatal: early EOF\n" {
		t.Fatalf("messages = %q", got)
	}
}

// git's fatal line is the last it writes, so a clone that said a great deal
// first still fails with it; a meter the parser does not know, redrawn in
// place, is kept once rather than crowding it out.
func TestProgressWriterKeepsTheEndOfALongStderr(t *testing.T) {
	w := &progressWriter{}
	for i := range 2000 {
		_, _ = w.Write([]byte("Checking connectivity: " + strconv.Itoa(i) + "\r"))
		_, _ = w.Write([]byte("Checking connectivity: " + strconv.Itoa(i) + "\r"))
	}
	_, _ = w.Write([]byte("fatal: the remote end hung up unexpectedly\n"))
	got := w.String()
	if !strings.HasSuffix(got, "Checking connectivity: 1999\nfatal: the remote end hung up unexpectedly\n") || len(got) > maxCloneMessages {
		t.Fatalf("messages (%d bytes) end %q, want the fatal line kept under the bound", len(got), got[max(0, len(got)-120):])
	}
	if strings.Count(got, "Checking connectivity: 1999\n") != 1 || !strings.HasPrefix(got, "Checking connectivity: ") {
		t.Fatalf("messages = %q, want each redraw once and whole lines only", got[:min(len(got), 120)])
	}
}

// The clone itself runs with --progress and in git's own words, so a real
// one over smart HTTP reports the meters a status line is drawn from.
func TestACloneReportsItsProgress(t *testing.T) {
	o := newOrigin(t)
	o.commit("README.md", "hello\n")
	first := o.commit("other.txt", strings.Repeat("content\n", 1000))
	target := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	// The document names the origin, so the agent's helper has its token.
	h.c.Converge(document(1, sandboxconfig.RuntimeSource{Slug: "primary", Target: target, OriginURL: o.url, OriginToken: testToken, Commit: first}))
	repo := &repository{dir: target, env: map[string]string{"HOME": h.home, "GIT_CONFIG_NOSYSTEM": "1", "PATH": os.Getenv("PATH"), "LANG": "de_DE.UTF-8"}}
	var reports []CloneProgress
	cloned, err := repo.clone(t.Context(), o.url, sandboxconfig.Source{}, h.helper, func(p CloneProgress) { reports = append(reports, p) })
	if err != nil || !cloned {
		t.Fatalf("clone = %v, %v", cloned, err)
	}
	var received *CloneProgress
	for i := range reports {
		if reports[i].Stage == CloneReceiving {
			received = &reports[i]
		}
	}
	// No bytes: git shows them only with a throughput, which a clone this
	// small and this local finishes before it starts.
	if received == nil || received.ObjectsTotal == 0 || received.Objects != received.ObjectsTotal {
		t.Fatalf("reports = %+v, want receiving to finish with every object", reports)
	}
	if head, _ := h.git(t, target, "rev-parse", "HEAD"); strings.TrimSpace(head) != first {
		t.Fatalf("cloned HEAD %q, want %s", head, first)
	}
}

// Progress belongs to the attempt that is cloning: it rides that state, a
// meter from an attempt that has since ended is dropped, and the state an
// attempt ends in carries none.
func TestCloneProgressRidesOnlyTheCloningState(t *testing.T) {
	c := New(Config{})
	cloning := c.setState(SourceState{Slug: "primary", State: StateCloning})
	c.cloneProgress(cloning, CloneProgress{Stage: CloneReceiving, Objects: 1, ObjectsTotal: 2})
	if got := c.states["primary"].Progress; got == nil || got.Objects != 1 {
		t.Fatalf("progress = %+v, want the meter on the cloning state", got)
	}
	c.setState(SourceState{Slug: "primary", State: StateFailed, Error: "boom"})
	c.cloneProgress(cloning, CloneProgress{Stage: CloneReceiving, Objects: 2, ObjectsTotal: 2})
	if got := c.states["primary"]; got.State != StateFailed || got.Progress != nil {
		t.Fatalf("state = %+v, want the failure with no progress", got)
	}
}
