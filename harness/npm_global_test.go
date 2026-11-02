package harness

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// npmByName is npm run as a command, as opposed to named in a comment or as
// part of npm-global's path.
var npmByName = regexp.MustCompile(`(?m)^[^#\n]*(^|[\s;&|(])npm\s`)

// Every installer of an npm package installs it through the node feature's
// npm-global, which runs the npm the feature chose into Discobox's prefix with
// a private scratch cache (its own test is sandbox-agent/featurescripts):
// never an npm found on PATH, which on a box whose image links one user's nvm
// into /usr/local/bin installs into that user's home, and never npm's default
// global prefix.
func TestNPMInstallersGoThroughNPMGlobal(t *testing.T) {
	for script, pkg := range map[string]string{
		"../sandbox-agent/features/node/install-pnpm.sh": "pnpm",
		"claude-code/install-cli.sh":                     "@anthropic-ai/claude-code",
		"codex-cli/install-cli.sh":                       "@openai/codex",
		"copilot/install-cli.sh":                         "@github/copilot",
		"omp/install-cli.sh":                             "@oh-my-pi/pi-coding-agent",
		"opencode/install-cli.sh":                        "@opencode/cli",
		"pi/install-cli.sh":                              "@earendil-works/pi-coding-agent",
	} {
		t.Run(script, func(t *testing.T) {
			data, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if !strings.Contains(text, "exec /usr/local/libexec/discobox/npm-global install "+pkg+"\n") {
				t.Errorf("%s does not install %s with npm-global", script, pkg)
			}
			if npmByName.MatchString(text) {
				t.Errorf("%s runs npm by name", script)
			}
		})
	}
}
