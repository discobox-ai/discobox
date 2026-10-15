package cli

import (
	"strings"
	"testing"
	"text/tabwriter"
)

// A use in a table is broken up as the console breaks it, its parts set in
// under its first line and anything after it on its last.
func TestAUseIsWrittenAsRowsInItsParts(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	tw := tabwriter.NewWriter(&out, 0, 4, 2, ' ', 0)
	useRows(tw, "USES", "Push branch fix-43 to acme/web. Read anything in the repository; push that branch. Never push main, or read secrets.", " (use_1)")
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "USES  Push branch fix-43 to acme/web.\n" +
		"        Read anything in the repository;\n" +
		"          push that branch.\n" +
		"        Never push main,\n" +
		"          or read secrets. (use_1)\n"
	if out.String() != want {
		t.Fatalf("rows =\n%s\nwant\n%s", out.String(), want)
	}
}

// A use is an agent's words: a control sequence in one is shown, not obeyed.
func TestAUseIsEscapedOnItsWayToTheTerminal(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	useRows(&out, "USE", "Read issues in org/repo.\x1b]52;c;aGk=\x07 Never close one.", "")
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("rows = %q, want the control sequence escaped", out.String())
	}
}
