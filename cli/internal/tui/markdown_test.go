package tui

import (
	"strings"
	"testing"

	"charm.land/glamour/v2"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// A block of code is highlighted in the window's syntax colors with no field
// behind it, and code in a sentence is the header's blue.
func TestMarkdownCode(t *testing.T) {
	r, err := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle(newStyles(true))), glamour.WithWordWrap(80))
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Render("Run `task check` first.\n\n```go\n// add sums\nfunc add(a int) string { return \"x\" + 1 }\n```\n\n```\nplain block\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	fg := func(c string) string { return "38;5;" + c + "m" }
	for what, want := range map[string]string{
		"inline code":   fg(colInfo) + "task check",
		"keyword":       fg("141") + "func",
		"function name": fg(colInfo) + "add",
		"type":          fg("117") + "int",
		"string":        fg("180") + `"x"`,
		"comment":       fg(colGrey) + "// add sums",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%s: no %q in\n%q", what, want, out)
		}
	}
	if strings.Contains(out, "[48;") {
		t.Errorf("a field is painted behind the code:\n%q", out)
	}
	if strings.Contains(out, fg(colGreen)) {
		t.Errorf("the command green is still used:\n%q", out)
	}
}

// The renderer decodes character references, so one that names a character a
// terminal would act on is left as it was typed: a comment cannot spell an
// escape sequence, a bidirectional override or an invisible character in
// printable text. The ones a page is written with still decode.
func TestMarkdownDoesNotDecodeACharacterATerminalActsOn(t *testing.T) {
	r, err := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle(newStyles(false))), glamour.WithWordWrap(200))
	if err != nil {
		t.Fatal(err)
	}
	evil := "&#x1b;[31mred&#27;[0m &#x1b;]52;c;aGVsbG8=&#7; abc&#x202e;def a&#x200b;b &rlm;x &#x9b;31m &#x85; &NewLine; &#x1b[32mgreen &#27[0m &shy"
	out, err := r.Render(console.MarkdownSource(evil + "\n\nfine: &amp; &lt;b&gt; a&nbsp;b &#128512; &copy; &nosuchname;"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07\u202e\u200b\u200f\u009b\u0085\u00ad") {
		t.Errorf("a character reference reached the screen as the character:\n%q", out)
	}
	// A link is the renderer's own escape sequence, and what a reference
	// spelled in its address is not in it.
	linked, err := r.Render(console.MarkdownSource("[l](http://a.example/&#x1b;&#7;x)"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(linked, "example/\x1b") || strings.Contains(linked, "\x07x") {
		t.Errorf("a character reference reached a link's address as the character:\n%q", linked)
	}
	for _, want := range []string{"&#x1b;[31mred&#27;[0m", "abc&#x202e;def", "&#x1b[32mgreen", "fine: & <b> a\u00a0b \U0001F600 \u00a9 &nosuchname;"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%q", want, out)
		}
	}
}
