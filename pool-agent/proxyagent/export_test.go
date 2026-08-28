package proxyagent

import (
	"log/slog"
	"testing"
)

// withTestRoot relocates this package's paths under a temporary directory for
// the duration of one test.
func withTestRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := testRoot
	testRoot = dir
	t.Cleanup(func() { testRoot = previous })
	return dir
}

// testLogger discards output: these tests assert on behavior, and a serving
// endpoint's info lines would drown the failures that matter.
func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
