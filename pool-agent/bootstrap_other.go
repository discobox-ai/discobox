//go:build !unix

package poolagent

import "os"

// checkBootstrapAccess has no owner or mode bits to compare off unix, where
// no pool machine runs: a Windows file's mode says nothing of who may read it.
func checkBootstrapAccess(string, os.FileInfo) error { return nil }
