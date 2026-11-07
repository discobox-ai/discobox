package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The reason names the obstacle, not the row's state: the two ways to have no
// container are undone by different things.
func TestAttachWhyNamesTheObstacle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    console.Sandbox
		want string
	}{
		{"archived points at unarchive", console.Sandbox{State: console.StateArchived}, "unarchive"},
		{"no container points at repair", console.Sandbox{State: console.StateError}, "repair"},
		{"attachable has nothing to say", console.Sandbox{State: console.StateError, HasRuntime: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := attachWhy(true, []console.Sandbox{tc.s})
			if tc.want == "" {
				if got != "" {
					t.Fatalf("why = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("why = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// A discobox's tags follow its name on its row, as many as leave the name room
// to be read, and none on a window too narrow for even one (ADR 0136).
func TestARowShowsTheTagsItHasRoomFor(t *testing.T) {
	t.Parallel()
	box := console.Sandbox{ID: "sbx_one", Name: "fix the reaper", State: console.StateRunning, Tags: []string{"ticket=ENG-12", "wip"}}
	row := func(width int) string {
		l := newSandboxList(console.Session{})
		l.width = width
		line, _ := l.row(newStyles(false), &zones{}, box, 0, 0, false)
		return ansi.Strip(line)
	}
	if wide := row(160); !strings.Contains(wide, "fix the reaper #ticket=ENG-12 #wip") {
		t.Fatalf("wide row = %q, want the name followed by both tags", wide)
	}
	narrow := row(40)
	if strings.Contains(narrow, "#") {
		t.Fatalf("narrow row = %q, want the tags dropped before the name is squeezed", narrow)
	}
	if !strings.Contains(narrow, "fix the") {
		t.Fatalf("narrow row = %q, want the name kept", narrow)
	}
}

// A row tagged with a status draws the tag in the status's color, blocked
// over ready, and leaves the row itself unpainted — as it does
// a row waiting on a credential request, which says so with its red `!`.
func TestAStatusTagColorsItsTagNotItsRow(t *testing.T) {
	t.Parallel()
	st := newStyles(true)
	for _, tc := range []struct {
		tags    []string
		pending int
		want    string
	}{
		{[]string{"ready", "issue=4"}, 0, st.readyText.Render("#ready")},
		{[]string{"blocked"}, 0, st.blockedText.Render("#blocked")},
		{[]string{"blocked", "ready"}, 0, st.blockedText.Render("#blocked")},
		{nil, 1, st.statusER.Render("!")},
	} {
		l := newSandboxList(console.Session{})
		l.width = 160
		box := console.Sandbox{ID: "sbx_one", Name: "fix the reaper", State: console.StateRunning, Tags: tc.tags, PendingRequests: tc.pending}
		line, _ := l.row(st, &zones{}, box, 0, 0, false)
		if strings.HasPrefix(line, "\x1b[48;5;") {
			t.Errorf("tags %v: row = %q, want it unpainted", tc.tags, line)
		}
		if !strings.Contains(line, tc.want) {
			t.Errorf("tags %v: row = %q, want it to draw %q", tc.tags, line, tc.want)
		}
	}
}

// The status is the last tag a narrowing row gives up: the tag is the only
// place the row says it.
func TestARowKeepsItsPullRequestStatusLongest(t *testing.T) {
	t.Parallel()
	box := console.Sandbox{ID: "sbx_one", Name: "fix the reaper", State: console.StateRunning, Tags: []string{"issue=4", "ticket=ENG-12", "ready"}}
	shown := 0
	for width := 40; width <= 160; width++ {
		l := newSandboxList(console.Session{})
		l.width = width
		line, _ := l.row(newStyles(false), &zones{}, box, 0, 0, false)
		line = ansi.Strip(line)
		if strings.Contains(line, "#") {
			shown++
			if !strings.Contains(line, "fix the reaper #ready") {
				t.Fatalf("width %d: row = %q, want #ready first among the tags it shows", width, line)
			}
		}
	}
	if shown == 0 {
		t.Fatal("no width showed a tag")
	}
}
