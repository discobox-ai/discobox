package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	agentLink = regexp.MustCompile(`^\s*ln -sf /usr/local/bin/discobox-sandbox-agent "?([^"\s]+)"?$`)
	anyLink   = regexp.MustCompile(`\bln\b.*discobox-sandbox-agent`)
	// agentItself is the link that installs the agent from its tree, which
	// is no personality.
	agentItself = regexp.MustCompile(`\bln\b.*\s/usr/local/bin/discobox-sandbox-agent$`)
	linkLoop    = regexp.MustCompile(`for c in ([^;]+); do`)
)

// linkedPaths is every link to the agent that the features' scripts lay,
// expanding the `for c in ...` loop that lays several. A line that links the
// agent some other way fails the test rather than going unseen.
func linkedPaths(t *testing.T, scripts ...string) []string {
	t.Helper()
	var paths []string
	for _, script := range scripts {
		paths = append(paths, scriptLinks(t, script)...)
	}
	slices.Sort(paths)
	return paths
}

func scriptLinks(t *testing.T, script string) []string {
	t.Helper()
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	var paths, loop []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if m := linkLoop.FindStringSubmatch(line); m != nil {
			loop = strings.Fields(m[1])
		}
		if !anyLink.MatchString(line) || agentItself.MatchString(line) {
			continue
		}
		m := agentLink.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("%s: links the agent in a form this test does not read: %q", script, strings.TrimSpace(line))
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
	return paths
}

// A link the agent does not dispatch on would start the sandbox agent itself
// under that name, and a personality nothing links is a command the image
// lost. Where each link sits matters as much as its name: `runc` must be on
// the directory containerd's and dockerd's drop-ins put ahead of the real one,
// and the rest on the user's PATH.
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
	// Core lays its own personalities, and the docker feature the two that
	// are Docker's (ADR 26-10-16-423 §7).
	scripts := []string{"../../features/core/link-agent.sh", "../../features/docker/link-personalities.sh"}
	if got := linkedPaths(t, scripts...); !slices.Equal(got, want) {
		t.Errorf("the features link the agent as %v, want %v", got, want)
	}
}
