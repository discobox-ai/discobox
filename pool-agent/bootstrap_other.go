//go:build !unix

package poolagent

import "os"

// checkBootstrapOwner has no owner to compare off unix, where no pool machine
// runs.
func checkBootstrapOwner(string, os.FileInfo) error { return nil }
