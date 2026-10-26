package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

func agentBoxes() []Sandbox {
	box := func(id string, state programstatus.State, msg string) Sandbox {
		return Sandbox{
			ID: id, Name: id, State: StateRunning,
			OriginKey: "here:/src/app", OriginHostID: "host_here", Source: "/src/app",
			Agent: programstatus.Status{State: state, Msg: msg},
		}
	}
	return []Sandbox{
		box("blocked-box", programstatus.Blocked, "Allow rm -rf build?"),
		box("working-box", programstatus.Working, ""),
		box("idle-box", programstatus.Idle, ""),
		box("done-box", programstatus.Done, ""),
		box("failed-box", programstatus.Error, ""),
		box("quiet-box", "", ""),
	}
}

func agentList() *sandboxList { return listForTest(Session{}, agentBoxes()) }

// What a discobox's agent reports is a glyph after the box's own state dot,
// one shape per state, and a blank cell for no report (ADR 26-10-16-424 §5).
func TestTheListDrawsWhatTheAgentReports(t *testing.T) {
	l := agentList()
	out := ansi.Strip(l.view(newStyles(true), &zones{}, false))
	for _, want := range []string{"● ◆ blocked-box", "● ▶ working-box", "● ◇ idle-box", "● ✓ done-box", "● × failed-box", "●   quiet-box"} {
		if !strings.Contains(out, want) {
			t.Errorf("view has no %q:\n%s", want, out)
		}
	}
}

// Without color the glyphs go and the word comes back, in the state column:
// every report implies a running box, so it takes running's place.
func TestWithoutColorTheAgentsWordTakesRunningsPlace(t *testing.T) {
	l := agentList()
	out := ansi.Strip(l.view(newStyles(false), &zones{}, false))
	for name, word := range map[string]string{
		"blocked-box": "blocked", "working-box": "working", "idle-box": "idle",
		"done-box": "done", "failed-box": "failed", "quiet-box": "running",
	} {
		line := lineWith(out, name)
		if !strings.Contains(line, " "+word+" ") {
			t.Errorf("%s's row has no %q: %q", name, word, line)
		}
	}
}

// An agent blocked on a person makes its box blocked the way a waiting
// credential request does, and under the cursor its row says what it is
// blocked on.
func TestABlockedAgentIsTheBoxsStandingAndSaysWhy(t *testing.T) {
	boxes := agentBoxes()
	if boxes[0].standing() != standingBlocked {
		t.Fatal("a blocked agent does not make its box's standing blocked")
	}
	if boxes[1].standing() != standingNone {
		t.Fatal("a working agent made its box's standing something")
	}
	l := agentList()
	l.moveTo(0)
	out := ansi.Strip(l.view(newStyles(true), &zones{}, true))
	if !strings.Contains(out, "❯ ● ◆ Allow rm") {
		t.Fatalf("the cursor row does not say what its agent is blocked on:\n%s", out)
	}
}

func lineWith(out, s string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, s) {
			return line
		}
	}
	return ""
}

// Under the cursor a box still provisioning a feature says which, and an
// agent blocked on a person still comes first.
func TestTheCursorRowSaysWhatIsStillProvisioning(t *testing.T) {
	boxes := agentBoxes()
	boxes[0].Provisioning = "provisioning docker"
	boxes[1].Provisioning = "provisioning docker"
	l := listForTest(Session{}, boxes)
	l.moveTo(1)
	out := ansi.Strip(l.view(newStyles(true), &zones{}, true))
	if !strings.Contains(out, "❯ ● ▶ provisioning d") {
		t.Fatalf("the cursor row does not say what is provisioning:\n%s", out)
	}
	if !strings.Contains(out, "blocked-box") {
		t.Fatalf("a row off the cursor lost its name:\n%s", out)
	}
	l.moveTo(0)
	out = ansi.Strip(l.view(newStyles(true), &zones{}, true))
	if !strings.Contains(out, "❯ ● ◆ Allow rm") {
		t.Fatalf("provisioning displaced what the agent is blocked on:\n%s", out)
	}
}

func TestCursorNotifiesPendingHarnessCapture(t *testing.T) {
	boxes := agentBoxes()
	boxes[1].Name = "dynamic program title"
	boxes[1].HarnessConfigPending = true
	l := listForTest(Session{}, boxes)
	l.width = 150
	l.moveTo(1)
	out := ansi.Strip(l.view(newStyles(true), &zones{}, true))
	if !strings.Contains(out, "harness changes: discobox admin harness changes working-box") {
		t.Fatalf("missing capture notice: %s", out)
	}
	boxes[0].HarnessConfigPending = true
	l = listForTest(Session{}, boxes)
	l.moveTo(0)
	out = ansi.Strip(l.view(newStyles(true), &zones{}, true))
	if !strings.Contains(out, "Allow rm") {
		t.Fatalf("capture displaced blocking question: %s", out)
	}
}
