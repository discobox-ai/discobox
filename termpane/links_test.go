package termpane

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// remapPort is a host's answer for a pane whose 8080 is reachable here as 8081,
// and whose bind address is not an address at all.
func remapPort(url string) string {
	for _, from := range []string{"http://localhost:8080", "http://0.0.0.0:8080", "http://127.0.0.1:8080"} {
		if strings.HasPrefix(url, from) {
			return "http://localhost:8081" + url[len(from):]
		}
	}
	return url
}

// A link the application made itself keeps its text and loses its target: the
// application knows what it is offering, and the host knows where that is.
func TestRewriteLinksRetargetsTheApplicationsOwnLinks(t *testing.T) {
	row := "open " + ansi.SetHyperlink("http://localhost:8080/admin") + "the console" + ansi.ResetHyperlink()

	got := rewriteLinks(row, remapPort)

	if want := ansi.SetHyperlink("http://localhost:8081/admin"); !strings.Contains(got, want) {
		t.Errorf("rewriteLinks = %q, want it to open %q", got, want)
	}
	if strings.Contains(got, "localhost:8080") {
		t.Errorf("rewriteLinks = %q, want the old target gone", got)
	}
	if got, want := ansi.Strip(got), "open the console"; got != want {
		t.Errorf("text = %q, want %q — the application's text is not the host's", got, want)
	}
}

// A URL in plain output becomes a link when the host moves it. The text still
// says what the program printed, because that is what the program printed.
func TestRewriteLinksLinksTextTheHostMoves(t *testing.T) {
	row := "listening on http://0.0.0.0:8080/ ..."

	got := rewriteLinks(row, remapPort)

	want := "listening on " + ansi.SetHyperlink("http://localhost:8081/") + "http://0.0.0.0:8080/" + ansi.ResetHyperlink() + " ..."
	if got != want {
		t.Errorf("rewriteLinks = %q, want %q", got, want)
	}
}

// A URL the host does not move is left as text. The terminal drawing the pane
// does its own detection on it, and a link that says the same thing would
// replace a working one with a copy.
func TestRewriteLinksLeavesTextItDoesNotMoveAlone(t *testing.T) {
	for _, row := range []string{
		"see https://github.com/discobox-ai/discobox for the rest",
		"listening on http://localhost:9999/",
		"nothing here at all",
	} {
		if got := rewriteLinks(row, remapPort); got != row {
			t.Errorf("rewriteLinks(%q) = %q, want it untouched", row, got)
		}
	}
}

// Text the application has already linked is not linked again: its target was
// rewritten where the link is, and a second link over the same cells would be
// two answers to one question.
func TestRewriteLinksDoesNotLinkInsideALink(t *testing.T) {
	row := ansi.SetHyperlink("http://localhost:8080/docs") + "http://localhost:8080/other" + ansi.ResetHyperlink()

	got := rewriteLinks(row, remapPort)

	if n := strings.Count(got, oscHyperlink); n != 2 {
		t.Errorf("rewriteLinks = %q, want one link opened and one closed, got %d sequences", got, n)
	}
	if want := ansi.SetHyperlink("http://localhost:8081/docs"); !strings.Contains(got, want) {
		t.Errorf("rewriteLinks = %q, want it to open %q", got, want)
	}
}

// A URL the application colored is still one URL. The style changes inside it
// are cells of the row, not breaks in the text.
func TestRewriteLinksSurvivesAStyleInsideTheURL(t *testing.T) {
	row := "up at http://localhost:\x1b[1m8080\x1b[0m/health"

	got := rewriteLinks(row, remapPort)

	if want := ansi.SetHyperlink("http://localhost:8081/health"); !strings.Contains(got, want) {
		t.Errorf("rewriteLinks = %q, want it to link %q", got, want)
	}
	if !strings.Contains(got, "\x1b[1m8080\x1b[0m") {
		t.Errorf("rewriteLinks = %q, want the application's style kept", got)
	}
}

// The punctuation a URL picked up from the sentence around it is not part of
// the URL; the brackets of an IPv6 authority are.
func TestRewriteLinksTrimsTheSentenceAndNotTheAddress(t *testing.T) {
	moved := ""
	rewrite := func(url string) string {
		moved = url
		return "http://localhost:8081/"
	}

	rewriteLinks("try http://localhost:8080/, then stop.", rewrite)
	if want := "http://localhost:8080/"; moved != want {
		t.Errorf("rewrote %q, want %q", moved, want)
	}

	rewriteLinks("try http://[::1]:8080 (loopback)", rewrite)
	if want := "http://[::1]:8080"; moved != want {
		t.Errorf("rewrote %q, want %q", moved, want)
	}
}

// The sequences take no cells, so a row that gained a link still measures as
// its text — the grid is exactly as wide as it was.
func TestRewriteLinksAddsNoCells(t *testing.T) {
	row := "listening on http://localhost:8080/"

	got := rewriteLinks(row, remapPort)

	if w, want := ansi.StringWidth(got), ansi.StringWidth(row); w != want {
		t.Fatalf("width = %d, want %d", w, want)
	}
	if text, want := ansi.Strip(got), row; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
}

// Without a rewriter the pane is what it was: nothing is detected and nothing
// is retargeted.
func TestRewriteLinksIsInertWithoutARewriter(t *testing.T) {
	row := "listening on http://localhost:8080/"
	if got := rewriteLinks(row, nil); got != row {
		t.Errorf("rewriteLinks = %q, want it untouched", got)
	}
}

// End to end: what the far end prints, the pane draws with the host's link on
// it — through the scrollback too, which is where output goes to be read.
func TestPaneLinksWhatTheFarEndPrints(t *testing.T) {
	m, stream, cmd := attach(t, 40, 3, WithLinkRewrite(remapPort))

	stream.send("serving http://localhost:8080/\r\n")
	for range 4 {
		stream.send("filler\r\n")
	}
	cmd = pump(t, m, cmd, "filler")
	_ = cmd

	m.Scroll(3)
	rows := strings.Join(m.View(), "\n")
	if want := ansi.SetHyperlink("http://localhost:8081/"); !strings.Contains(rows, want) {
		t.Errorf("view = %q, want a link to %q", rows, want)
	}
	if !strings.Contains(ansi.Strip(rows), "serving http://localhost:8080/") {
		t.Errorf("view = %q, want the text the far end printed", ansi.Strip(rows))
	}
}
