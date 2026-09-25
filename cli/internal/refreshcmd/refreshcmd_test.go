package refreshcmd

import (
	"context"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestSplitAndJoinRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"gh auth token", []string{"gh", "auth", "token"}},
		{"  gcloud   auth print-access-token ", []string{"gcloud", "auth", "print-access-token"}},
		{`op read "op://Private/GitHub token/credential"`, []string{"op", "read", "op://Private/GitHub token/credential"}},
		{`printf '%s' 'it'\''s'`, []string{"printf", "%s", "it's"}},
		{`a\ b ''`, []string{"a b", ""}},
	} {
		got, err := Split(tc.line)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("Split(%q) = %q, %v; want %q", tc.line, got, err, tc.want)
		}
		again, err := Split(Join(got))
		if err != nil || !slices.Equal(again, got) {
			t.Fatalf("Split(Join(%q)) = %q, %v", got, again, err)
		}
	}
	if _, err := Split(`gh "auth`); err == nil {
		t.Fatal("an unterminated quote split without error")
	}
}

func TestRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX utilities")
	}
	ctx := context.Background()
	value, err := Run(ctx, []string{"printf", "  gho_abc\n"})
	if err != nil || value != "gho_abc" {
		t.Fatalf("Run = %q, %v; want the trimmed output", value, err)
	}
	if _, err := Run(ctx, []string{"true"}); err == nil || !strings.Contains(err.Error(), "printed nothing") {
		t.Fatalf("empty output err = %v", err)
	}
	_, err = Run(ctx, []string{"sh", "-c", "echo not logged in >&2; echo second >&2; exit 1"})
	if err == nil || !strings.Contains(err.Error(), "not logged in") || strings.Contains(err.Error(), "second") {
		t.Fatalf("failure err = %v, want stderr's first line", err)
	}
	if _, err := Run(ctx, []string{"head", "-c", "70000", "/dev/zero"}); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("overflow err = %v", err)
	}
}
