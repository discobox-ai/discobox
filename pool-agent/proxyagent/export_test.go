package proxyagent

import "testing"

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
