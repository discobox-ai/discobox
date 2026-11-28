package server

import (
	"os"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/gitcache"
)

// TestMain runs this test binary as a Git cache's pre-receive hook when Git
// runs it as one: every push into a cache runs the hook, which names this
// program as it is installed (os.Executable), and under test that is the
// test binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == gitcache.PreReceiveCommand {
		os.Exit(gitcache.RunPreReceive(os.Stdin, os.Stderr))
	}
	os.Exit(m.Run())
}
