//go:build !unix

package sandboxruntime

import "os"

// fileOwner is -1 for both ids: the platform has no Unix ownership, and the
// pool agent only runs on Unix. It exists so the package cross-compiles.
func fileOwner(os.FileInfo) (uid, gid int) {
	return -1, -1
}
