package termpane

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// claiming is a pane that answers discobox links itself, as a multiplexer
// whose panes name each other does.
func claiming(cols int) *Model {
	m := New(WithClaimedLinks("discobox", "discobox+http", "discobox+https"))
	m.cols = cols
	return m
}

// A claimed URI in plain text is found where it lies, with the punctuation of
// the sentence round it left off.
func TestClaimLinksFindsPlainText(t *testing.T) {
	m := claiming(80)
	row := "worker discobox:///sbx_0123, see also DISCOBOX+HTTP://10.0.0.5:8081/sbx_9."

	got := m.claimLinks(4, row)

	if got != row {
		t.Errorf("claimLinks = %q, want plain text left exactly as printed", got)
	}
	want := []Link{
		{Row: 4, Col: 7, Width: len("discobox:///sbx_0123"), URI: "discobox:///sbx_0123"},
		{Row: 4, Col: 38, Width: len("DISCOBOX+HTTP://10.0.0.5:8081/sbx_9"), URI: "DISCOBOX+HTTP://10.0.0.5:8081/sbx_9"},
	}
	if !reflect.DeepEqual(m.Links(), want) {
		t.Errorf("Links = %+v, want %+v", m.Links(), want)
	}
}

// A link the application emitted to a claimed scheme loses its OSC 8, open and
// close, and keeps its text: nothing outside the host can follow it.
func TestClaimLinksTakesTheOSC8OffAnEmittedLink(t *testing.T) {
	m := claiming(80)
	row := "jump to " + ansi.SetHyperlink("discobox:///sbx_0123") + "worker 3" + ansi.ResetHyperlink() + " now"

	got := m.claimLinks(0, row)

	if strings.Contains(got, "\x1b]8;") {
		t.Errorf("claimLinks = %q, want no OSC 8 left", got)
	}
	if ansi.Strip(got) != "jump to worker 3 now" {
		t.Errorf("text = %q, want the application's text", ansi.Strip(got))
	}
	want := []Link{{Row: 0, Col: 8, Width: 8, URI: "discobox:///sbx_0123"}}
	if !reflect.DeepEqual(m.Links(), want) {
		t.Errorf("Links = %+v, want %+v", m.Links(), want)
	}
}

// Everything that is not claimed is left as it was: a web link keeps its OSC 8,
// and a claimed URI that is the text of a web link is that link's.
func TestClaimLinksLeavesOtherLinksAlone(t *testing.T) {
	m := claiming(80)
	web := ansi.SetHyperlink("https://example.com/") + "discobox:///sbx_0123" + ansi.ResetHyperlink()
	for _, row := range []string{
		web,
		"see https://example.com/ for more",
		"xdiscobox://sbx_0123 is no scheme of ours",
	} {
		if got := m.claimLinks(0, row); got != row {
			t.Errorf("claimLinks(%q) = %q, want it untouched", row, got)
		}
	}
	if len(m.Links()) != 0 {
		t.Errorf("Links = %+v, want none", m.Links())
	}
}

// A pane that claims nothing does no work for it.
func TestClaimLinksWithoutTheOption(t *testing.T) {
	m := New()
	m.cols = 80
	row := ansi.SetHyperlink("discobox:///sbx_0123") + "x" + ansi.ResetHyperlink()
	if got := m.claimLinks(0, row); got != row || len(m.Links()) != 0 {
		t.Errorf("claimLinks = %q, %+v, want the row as it was and no links", got, m.Links())
	}
}

// A link past the pane's edge is cut where the grid is: only cells that are
// drawn can be pressed.
func TestClaimLinksStopsAtTheGrid(t *testing.T) {
	m := claiming(20)
	m.claimLinks(0, "0123456789 discobox:///sbx_0123")
	want := []Link{{Row: 0, Col: 11, Width: 9, URI: "discobox:///sbx_0123"}}
	if !reflect.DeepEqual(m.Links(), want) {
		t.Errorf("Links = %+v, want %+v", m.Links(), want)
	}
}

// End to end: what the far end prints, View draws without an OSC 8 to a claimed
// scheme and reports where it drew it, on the row it drew it on.
func TestViewReportsClaimedLinks(t *testing.T) {
	m, stream, cmd := attach(t, 40, 3, WithClaimedLinks("discobox"))

	stream.send("top\r\n  " + ansi.SetHyperlink("discobox:///sbx_7") + "box 7" + ansi.ResetHyperlink() + " and discobox:///sbx_8\r\nend")
	pump(t, m, cmd, "end")

	rows := m.View()
	if strings.Contains(rows[1], "discobox:///sbx_7") {
		t.Errorf("row = %q, want no OSC 8 to the claimed link", rows[1])
	}
	want := []Link{
		{Row: 1, Col: 2, Width: 5, URI: "discobox:///sbx_7"},
		{Row: 1, Col: 12, Width: len("discobox:///sbx_8"), URI: "discobox:///sbx_8"},
	}
	if !reflect.DeepEqual(m.Links(), want) {
		t.Errorf("Links = %+v, want %+v", m.Links(), want)
	}
}

// A scheme starts a word: the http:// at the tail of discobox+http:// is not a
// web URL for the host's rewrite to move, so the claimed link is the host's
// whole even when the rewrite would have moved that port.
func TestAClaimedSchemeIsNotTakenForTheWebSchemeAtItsTail(t *testing.T) {
	row := "discobox+http://localhost:8080/sbx_0123"
	if got := rewriteLinks(row, remapPort); got != row {
		t.Errorf("rewriteLinks = %q, want the tail of a longer scheme left alone", got)
	}

	m, stream, cmd := attach(t, 60, 2, WithLinkRewrite(remapPort), WithClaimedLinks("discobox+http"))
	stream.send(row + "\r\nend")
	pump(t, m, cmd, "end")
	rows := m.View()
	if strings.Contains(rows[0], "\x1b]8;") {
		t.Errorf("row = %q, want no OSC 8 on the host's own link", rows[0])
	}
	want := []Link{{Row: 0, Col: 0, Width: len(row), URI: row}}
	if !reflect.DeepEqual(m.Links(), want) {
		t.Errorf("Links = %+v, want %+v", m.Links(), want)
	}

	// And a claimed scheme that is the tail of a longer word is no link.
	m2 := claiming(80)
	m2.claimLinks(0, "see x+discobox:///sbx_0123")
	if len(m2.Links()) != 0 {
		t.Errorf("Links = %+v, want none for a scheme inside a longer one", m2.Links())
	}
}
