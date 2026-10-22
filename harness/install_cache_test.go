package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Installers run both in image builds and on boxes whose npm cache is mounted.
// Fake npm and rm so a regression cannot delete the machine running this test.
func TestNPMInstallersOwnTheirScratchCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux provisioning scripts")
	}
	for _, script := range []string{
		"../sandbox-agent/features/node/install-pnpm.sh",
		"claude-code/install-cli.sh", "codex-cli/install-cli.sh",
		"copilot/install-cli.sh", "omp/install-cli.sh",
		"opencode/install-cli.sh", "pi/install-cli.sh",
	} {
		for _, status := range []int{0, 23} {
			t.Run(fmt.Sprintf("%s/exit-%d", script, status), func(t *testing.T) {
				dir := t.TempDir()
				for name, body := range map[string]string{
					"npm": `test -d "$npm_config_cache" || exit 91
case "$npm_config_cache" in "$TMPDIR"/*) ;; *) exit 92 ;; esac
printf '%s' "$npm_config_cache" > "$TMPDIR/used-cache"
exit "$INSTALL_STATUS"
`,
					"rm": `printf '%s\n' "$@" >> "$TMPDIR/cleanup"
`,
				} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nset -eu\n"+body), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				cmd := exec.CommandContext(t.Context(), "sh", script)
				cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+dir, fmt.Sprintf("INSTALL_STATUS=%d", status), "npm_config_cache=/must-not-use-shared-cache")
				out, err := cmd.CombinedOutput()
				if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != status {
					t.Fatalf("installer: %v, %s", err, out)
				}
				cache, err := os.ReadFile(filepath.Join(dir, "used-cache"))
				if err != nil {
					t.Fatal(err)
				}
				cleanup, err := os.ReadFile(filepath.Join(dir, "cleanup"))
				if err != nil || strings.TrimSpace(string(cleanup)) != "-rf\n"+string(cache) {
					t.Fatalf("cleanup = %q, %v; want only private cache %q", cleanup, err, cache)
				}
			})
		}
	}
}
