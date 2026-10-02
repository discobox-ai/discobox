package proxyagent

import (
	"log/slog"
	"testing"

	"github.com/discobox-ai/discobox/layout"
)

// withTestRoot is a pool container's filesystem relocated under a temporary
// directory, for one test.
func withTestRoot(t *testing.T) layout.Root {
	t.Helper()
	return layout.ContainerAt(t.TempDir())
}

// testLogger discards output: these tests assert on behavior, and a serving
// endpoint's info lines would drown the failures that matter.
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
