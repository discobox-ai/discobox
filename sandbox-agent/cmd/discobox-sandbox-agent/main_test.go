package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	agentLink = regexp.MustCompile(`^\s*(?:&& )?ln -sf /usr/local/bin/discobox-sandbox-agent "?([^"\s]+)"?`)
	anyLink   = regexp.MustCompile(`\bln\b.*discobox-sandbox-agent`)
	linkLoop  = regexp.MustCompile(`for c in ([^;]+); do`)
)

// linkedPaths is every link to the agent that an image recipe lays,
// expanding the `for c in ...` loop that lays several. A line that links the
// agent some other way fails the test rather than going unseen.
func linkedPaths(t *testing.T, recipe string) []string {
	t.Helper()
	data, err := os.ReadFile(recipe)
	if err != nil {
		t.Fatal(err)
	}
	var paths, loop []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if m := linkLoop.FindStringSubmatch(line); m != nil {
			loop = strings.Fields(m[1])
		}
		if !anyLink.MatchString(line) {
			continue
		}
		m := agentLink.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("%s: links the agent in a form this test does not read: %q", recipe, strings.TrimSpace(line))
			continue
		}
		if strings.Contains(m[1], "$c") {
			for _, c := range loop {
				paths = append(paths, strings.ReplaceAll(m[1], "$c", c))
			}
			continue
		}
		paths = append(paths, m[1])
	}
	slices.Sort(paths)
	return paths
}

// A link the agent does not dispatch on would start the sandbox agent itself
// under that name, and a personality nothing links is a command the image
// lost. Where each link sits matters as much as its name: `runc` must be on
// the directory containerd's and dockerd's drop-ins put ahead of the real one,
// and the rest on the user's PATH. The container image and its boxd twin lay
// the same links.
func TestImageLinksEveryPersonalityAndNothingElse(t *testing.T) {
	var want []string
	for name := range personalities {
		dir := "/usr/local/bin/"
		if name == "runc" {
			dir = "/opt/discobox/bin/"
		}
		want = append(want, dir+name)
	}
	slices.Sort(want)
	for _, recipe := range []string{"../../Dockerfile", "../../boxd.yaml"} {
		if got := linkedPaths(t, recipe); !slices.Equal(got, want) {
			t.Errorf("%s links the agent as %v, want %v", recipe, got, want)
		}
	}
}
