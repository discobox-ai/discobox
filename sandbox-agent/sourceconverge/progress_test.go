package sourceconverge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/gitprogress"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

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
	var reports []gitprogress.Progress
	cloned, err := repo.clone(t.Context(), o.url, sandboxconfig.Source{}, h.helper, func(p gitprogress.Progress) { reports = append(reports, p) })
	if err != nil || !cloned {
		t.Fatalf("clone = %v, %v", cloned, err)
	}
	var received *gitprogress.Progress
	for i := range reports {
		if reports[i].Stage == gitprogress.Receiving {
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
	c.cloneProgress(cloning, gitprogress.Progress{Stage: gitprogress.Receiving, Objects: 1, ObjectsTotal: 2})
	if got := c.states["primary"].Progress; got == nil || got.Objects != 1 {
		t.Fatalf("progress = %+v, want the meter on the cloning state", got)
	}
	c.setState(SourceState{Slug: "primary", State: StateFailed, Error: "boom"})
	c.cloneProgress(cloning, gitprogress.Progress{Stage: gitprogress.Receiving, Objects: 2, ObjectsTotal: 2})
	if got := c.states["primary"]; got.State != StateFailed || got.Progress != nil {
		t.Fatalf("state = %+v, want the failure with no progress", got)
	}
}
