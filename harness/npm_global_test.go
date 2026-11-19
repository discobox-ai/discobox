package harness_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/internal/harnesstest"
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
	pnpm, err := os.ReadFile("../sandbox-agent/features/node/install-pnpm.sh")
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{"install-pnpm.sh": string(pnpm)}
	packages := map[string]string{"install-pnpm.sh": "pnpm"}
	for _, dir := range harness.BuiltinDirectories() {
		staged := harnesstest.Stage(t, dir)
		if !staged.Declaration().Coding() {
			continue
		}
		scripts[dir] = string(staged.FeatureFile(t, "install-cli.sh"))
		packages[dir] = staged.Declaration().Install.Package
	}
	for script, text := range scripts {
		pkg := packages[script]
		t.Run(script, func(t *testing.T) {
			if !strings.Contains(text, "exec /usr/local/libexec/discobox/npm-global install "+pkg+"\n") {
				t.Errorf("%s does not install %s with npm-global", script, pkg)
			}
			if npmByName.MatchString(text) {
				t.Errorf("%s runs npm by name", script)
			}
		})
	}
}
