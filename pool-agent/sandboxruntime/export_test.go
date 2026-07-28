package sandboxruntime

import "testing"

// withTestRoot relocates the state tree under a temporary directory for the
// duration of one test and returns that directory.
func withTestRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := testRoot
	testRoot = dir
	t.Cleanup(func() { testRoot = old })
	return dir
}
