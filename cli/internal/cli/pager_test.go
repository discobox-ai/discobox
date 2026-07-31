package cli

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestPagerCommandPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		discobox, gitPager, pager string
		want                      string
	}{
		{name: "falls back to less", want: defaultPager},
		{name: "PAGER", pager: "more", want: "more"},
		{name: "GIT_PAGER beats PAGER", gitPager: "delta", pager: "more", want: "delta"},
		{name: "DISCOBOX_PAGER wins", discobox: "bat", gitPager: "delta", pager: "more", want: "bat"},
		{name: "arguments survive", pager: "less -R -X", want: "less -R -X"},
		{name: "blank is skipped", discobox: "  ", pager: "more", want: "more"},
		// "cat" is the conventional way to disable paging through the
		// environment, so it means no pager rather than a pager named cat.
		{name: "cat disables", pager: "cat", want: ""},
		{name: "cat wins where it is set", discobox: "cat", pager: "less", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pagerCommand(tc.discobox, tc.gitPager, tc.pager); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPagerEnvDefaults pins the less settings that make a paged diff usable:
// without R the ANSI colors arrive as literal escape codes.
func TestPagerEnvDefaults(t *testing.T) {
	t.Setenv("LESS", "")
	os.Unsetenv("LESS")
	os.Unsetenv("LV")

	env := pagerEnv(nil)
	if !slices.Contains(env, "LESS=FRX") {
		t.Fatalf("LESS default missing: %v", env)
	}
	if !slices.Contains(env, "LV=-c") {
		t.Fatalf("LV default missing: %v", env)
	}
}

func TestPagerEnvKeepsTheUsersChoice(t *testing.T) {
	t.Setenv("LESS", "-S")
	for _, entry := range pagerEnv(nil) {
		if strings.HasPrefix(entry, "LESS=") {
			t.Fatalf("overrode the user's LESS: %q", entry)
		}
	}
}

// TestStartPagerLeavesNonTerminalsAlone is what keeps `disco diff > file` and
// every test in this package writing straight to their own writer.
func TestStartPagerLeavesNonTerminalsAlone(t *testing.T) {
	var buf strings.Builder
	out, done := startPager(t.Context(), &buf, true)
	if out != any(&buf) {
		t.Fatal("a non-terminal must not be paged")
	}
	if err := done(); err != nil {
		t.Fatal(err)
	}
}

// TestBrokenPipeIsRecognized covers the error a quit pager actually produces.
// It arrives as an *fs.PathError wrapping EPIPE, printed as "write |1: broken
// pipe", and mistaking it for a failure turns "I have seen enough" into an
// error and a nonzero exit.
func TestBrokenPipeIsRecognized(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	// The first write may land in the pipe buffer; the reader being gone is
	// only reported once the kernel notices.
	var writeErr error
	for range 100 {
		if _, writeErr = writer.Write([]byte(strings.Repeat("x", 4096))); writeErr != nil {
			break
		}
	}
	if writeErr == nil {
		t.Skip("the platform did not report a broken pipe")
	}
	if !isBrokenPipe(writeErr) {
		t.Fatalf("a quit pager must not read as a failure: %v", writeErr)
	}
}

func TestBrokenPipeDoesNotSwallowRealErrors(t *testing.T) {
	if isBrokenPipe(os.ErrPermission) {
		t.Fatal("a real error must still be reported")
	}
}

// TestRenderedOutputReportsBrokenPipeUnwrapped walks the path a quit pager
// actually takes — the rendered view, through the color-profile writer — and
// checks the error still reads as a broken pipe at the far end. A wrapper that
// loses the cause here would turn quitting the pager back into an error.
func TestRenderedOutputReportsBrokenPipeUnwrapped(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	view := &diffView{out: writer, terminal: writer, render: true, color: true, width: 80}
	patch := "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+two\n"
	// Enough repetitions to overflow the pipe buffer, since a small write can
	// succeed into a buffer nobody will ever read.
	var renderErr error
	for range 100 {
		if renderErr = view.writePatch(patch, "base"); renderErr != nil {
			break
		}
	}
	if renderErr == nil {
		t.Skip("the platform did not report a broken pipe")
	}
	if !isBrokenPipe(renderErr) {
		t.Fatalf("rendered output lost the broken pipe: %v", renderErr)
	}
}
