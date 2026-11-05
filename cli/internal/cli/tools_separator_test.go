package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Exercise the command tree and the tool process: pflag consumes a leading
// separator, while the shared discobox parser consumes one after a target.
// Both paths must preserve argument boundaries and the tool's own separators.
func TestHostToolDropsOptionalSeparatorAfterDiscobox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the tool is a shell script")
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"positional_separator", []string{"sbx_devbox00000001", "--", "one", "two words", "--", "--all"}, "one\ntwo words\n--\n--all\n"},
		{"positional_no_separator", []string{"sbx_devbox00000001", "one", "two words", "--", "--all"}, "one\ntwo words\n--\n--all\n"},
		{"selected_separator", []string{"--", "one", "two words", "--", "--all"}, "one\ntwo words\n--\n--all\n"},
		{"named_separator", []string{"devbox", "--", "one", "two words", "--", "--all"}, "one\ntwo words\n--\n--all\n"},
		{"positional_literal_separator", []string{"sbx_devbox00000001", "--", "--", "--all"}, "--\n--all\n"},
		{"selected_literal_separator", []string{"--", "--", "--all"}, "--\n--all\n"},
		{"empty_argument", []string{"sbx_devbox00000001", "--", "", "two words"}, "\ntwo words\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			record := filepath.Join(home, "args")
			declareUserTool(t, home, "probe.sh", "#!/bin/sh\n#---\n# runs: host\n#---\nprintf '%s\\n' \"$@\" > '"+record+"'\n")
			if err := os.Chmod(filepath.Join(home, ".config", "discobox", "tools", "probe.sh"), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, _, stderr, err := runToolsCmd(t, vscodeFakeServer(), home, "probe", tc.args...); err != nil {
				t.Fatalf("tools probe: %v\n%s", err, stderr)
			}
			if got := readFile(t, record); got != tc.want {
				t.Errorf("tool argv = %q, want %q", got, tc.want)
			}
		})
	}
}
