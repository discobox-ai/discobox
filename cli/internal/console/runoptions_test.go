package console

import "testing"

// The command preview only quotes what a shell would otherwise split or
// interpret: a preview full of needless quotes is one you stop trusting.
func TestShellQuote(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                  "''",
		"codex":             "codex",
		"/src/disco2@main":  "/src/disco2@main",
		"fix the reaper":    "'fix the reaper'",
		"it's":              `'it'\''s'`,
		"--include-dirty=1": "--include-dirty=1",
	} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
