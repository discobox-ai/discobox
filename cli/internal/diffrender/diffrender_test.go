package diffrender

import (
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2"
)

const samplePatch = `diff --git a/cli/internal/cli/diff.go b/cli/internal/cli/diff.go
index 1111111..2222222 100644
--- a/cli/internal/cli/diff.go
+++ b/cli/internal/cli/diff.go
@@ -10,7 +10,7 @@ func run() error {
 	ctx := context.Background()
 	client := newClient()
-	return client.Diff(ctx, "old")
+	return client.Diff(ctx, "new")
 }
@@ -40,2 +40,3 @@ func other() {
 	x := 1
+	y := 2
diff --git a/notes.txt b/notes.txt
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/notes.txt
@@ -0,0 +1 @@
+hello
`

func TestParseFilesAndCounts(t *testing.T) {
	files := Parse(samplePatch)
	if len(files) != 2 {
		t.Fatalf("files: got %d, want 2", len(files))
	}
	if got, want := files[0].Path, "cli/internal/cli/diff.go"; got != want {
		t.Fatalf("path: got %q, want %q", got, want)
	}
	if files[0].Added != 2 || files[0].Removed != 1 {
		t.Fatalf("counts: got +%d -%d, want +2 -1", files[0].Added, files[0].Removed)
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("hunks: got %d, want 2", len(files[0].Hunks))
	}
	if files[1].Status != AddedFile {
		t.Fatalf("second file status: got %v, want AddedFile", files[1].Status)
	}
	if got, want := files[1].Path, "notes.txt"; got != want {
		t.Fatalf("new file path: got %q, want %q", got, want)
	}
}

func TestParseLineNumbers(t *testing.T) {
	hunk := Parse(samplePatch)[0].Hunks[0]
	want := []Line{
		{Kind: Context, Old: 10, New: 10},
		{Kind: Context, Old: 11, New: 11},
		{Kind: Removed, Old: 12},
		{Kind: Added, New: 12},
		{Kind: Context, Old: 13, New: 13},
	}
	if len(hunk.Lines) != len(want) {
		t.Fatalf("lines: got %d, want %d", len(hunk.Lines), len(want))
	}
	for i, line := range hunk.Lines {
		if line.Kind != want[i].Kind || line.Old != want[i].Old || line.New != want[i].New {
			t.Fatalf("line %d: got %+v, want kind %v old %d new %d", i, line, want[i].Kind, want[i].Old, want[i].New)
		}
	}
}

// TestParseQuotedPath covers git's C-style quoting, which is how every
// non-ASCII path arrives.
func TestParseQuotedPath(t *testing.T) {
	patch := `diff --git "a/d/\303\251 n.txt" "b/d/\303\251 n.txt"
new file mode 100644
--- /dev/null
+++ "b/d/\303\251 n.txt"
@@ -0,0 +1 @@
+h
`
	files := Parse(patch)
	if len(files) != 1 {
		t.Fatalf("files: got %d, want 1", len(files))
	}
	if got, want := files[0].Path, "d/é n.txt"; got != want {
		t.Fatalf("path: got %q, want %q", got, want)
	}
}

func TestRenderPlainCarriesMeaningWithoutColor(t *testing.T) {
	var out strings.Builder
	if err := Render(&out, Parse(samplePatch), Options{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"cli/internal/cli/diff.go",
		"+2 -1",
		` 12 -	return client.Diff(ctx, "old")`,
		` 12 +	return client.Diff(ctx, "new")`,
		"⋯",
		"notes.txt  (new file)",
	} {
		// Tabs are expanded, so compare on the un-tabbed form.
		want = strings.ReplaceAll(want, "\t", strings.Repeat(" ", defaultTabWidth))
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("color leaked into an uncolored render:\n%q", got)
	}
}

func TestRenderPadsAndWrapsToWidth(t *testing.T) {
	patch := "diff --git a/long.md b/long.md\n--- a/long.md\n+++ b/long.md\n@@ -1 +1 @@\n-short\n+" + strings.Repeat("x", 40) + "\n"
	var out strings.Builder
	if err := Render(&out, Parse(patch), Options{Width: 20}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if width := len([]rune(line)); width > 20 {
			t.Fatalf("line wider than the terminal (%d): %q", width, line)
		}
	}
	// The long added line has to survive the wrap in full.
	if got := strings.Count(out.String(), "x"); got != 40 {
		t.Fatalf("wrapped line lost content: got %d x's, want 40", got)
	}
}

func TestChangedSpanFindsTheEdit(t *testing.T) {
	oldSpan, newSpan, ok := changedSpan(`	return client.Diff(ctx, "old")`, `	return client.Diff(ctx, "new")`)
	if !ok {
		t.Fatal("a one-word edit should be emphasized")
	}
	if oldSpan.start != newSpan.start {
		t.Fatalf("spans should share a prefix: %+v vs %+v", oldSpan, newSpan)
	}
	if got := `	return client.Diff(ctx, "old")`[oldSpan.start:oldSpan.end]; got != "old" {
		t.Fatalf("emphasized span: got %q, want %q", got, "old")
	}

	// A line that changed completely says nothing extra by highlighting all of
	// it; the background color already carries that.
	if _, _, ok := changedSpan("alpha beta gamma", "nothing alike here"); ok {
		t.Fatal("a wholly rewritten line should not be emphasized")
	}
}

func TestExpandTabsAlignsToStops(t *testing.T) {
	if got, want := expandTabs("a\tb", 4), "a   b"; got != want {
		t.Fatalf("expandTabs: got %q, want %q", got, want)
	}
	if got, want := expandTabs("\t\tx", 4), "        x"; got != want {
		t.Fatalf("expandTabs: got %q, want %q", got, want)
	}
}

// TestEmphasisSurvivesTabExpansion guards the offset bug the layout invites:
// emphasis is measured in columns, so it has to be measured after tabs become
// spaces, not before.
func TestEmphasisSurvivesTabExpansion(t *testing.T) {
	patch := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-\t\tcall(\"old\")\n+\t\tcall(\"new\")\n"
	texts := []string{expandTabs("\t\tcall(\"old\")", defaultTabWidth), expandTabs("\t\tcall(\"new\")", defaultTabWidth)}
	spans := pairEmphasis(Parse(patch)[0].Hunks[0].Lines, texts)
	if got := texts[0][spans[0].start:spans[0].end]; got != "old" {
		t.Fatalf("emphasized span: got %q, want %q", got, "old")
	}
	if got := texts[1][spans[1].start:spans[1].end]; got != "new" {
		t.Fatalf("emphasized span: got %q, want %q", got, "new")
	}
}

// TestHighlightUsesHunkContext is the reason the lexer is fed a whole hunk
// rather than each line: a line inside a block comment is only recognizable as
// a comment if the lexer saw the comment open.
func TestHighlightUsesHunkContext(t *testing.T) {
	patch := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,4 +1,4 @@\n /*\n-old\n+new\n */\n"
	hunk := Parse(patch)[0].Hunks[0]
	texts := make([]string, len(hunk.Lines))
	for i, line := range hunk.Lines {
		texts[i] = line.Text
	}
	spans := highlightHunk(lexerFor("x.go"), hunk.Lines, texts)
	for i, line := range hunk.Lines {
		if line.Text != "old" && line.Text != "new" {
			continue
		}
		if len(spans[i]) == 0 {
			t.Fatalf("line %q inside a block comment produced no spans", line.Text)
		}
		if got := spans[i][0].token.Category(); got != chroma.Comment {
			t.Fatalf("line %q: got token category %v, want Comment", line.Text, got)
		}
	}
}

func TestLexerForUnknownFileIsNil(t *testing.T) {
	if lexerFor("notes.unknownextension") != nil {
		t.Fatal("an unrecognized file should not be highlighted at all")
	}
	if lexerFor("main.go") == nil {
		t.Fatal("a Go file should have a lexer")
	}
}

// TestStyleRunsCombineSyntaxAndDiff checks the two channels compose: the
// background keeps saying what the diff did while the foreground says what the
// code is, and runs break only where the rendered style actually changes.
func TestStyleRunsCombineSyntaxAndDiff(t *testing.T) {
	theme := newTheme(Options{Color: true, Dark: true})
	text := `if x == "old" {`
	spans := highlightHunk(lexerFor("x.go"), []Line{{Kind: Added, Text: text}}, []string{text})[0]

	// No emphasis: runs break at the colored tokens only, not at every token.
	plain := styleRuns(text, spans, emphasis{}, theme.add, theme.addEmph, theme.syntax)
	var rebuilt strings.Builder
	for _, run := range plain {
		rebuilt.WriteString(string(run.runes))
	}
	if rebuilt.String() != text {
		t.Fatalf("runs lost content: got %q, want %q", rebuilt.String(), text)
	}
	if len(plain) < 2 {
		t.Fatalf("expected the keyword and the string to be their own runs, got %d runs", len(plain))
	}
	if len(plain) > 6 {
		t.Fatalf("runs did not coalesce: got %d for %q", len(plain), text)
	}

	// With emphasis over the string, the emphasized background applies without
	// costing the string its own color.
	start := strings.Index(text, `"old"`)
	emphasized := styleRuns(text, spans, emphasis{start: start, end: start + 5}, theme.add, theme.addEmph, theme.syntax)
	var found bool
	for _, run := range emphasized {
		if string(run.runes) != `"old"` {
			continue
		}
		found = true
		if run.style.GetBackground() != theme.addEmph.GetBackground() {
			t.Fatal("emphasized run lost the emphasis background")
		}
		if run.style.GetForeground() == nil {
			t.Fatal("emphasized run lost its syntax color")
		}
	}
	if !found {
		t.Fatalf("emphasized span did not become its own run: %v", runTexts(emphasized))
	}
}

func runTexts(runs []styledRun) []string {
	out := make([]string, 0, len(runs))
	for _, run := range runs {
		out = append(out, string(run.runes))
	}
	return out
}
