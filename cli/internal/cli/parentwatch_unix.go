//go:build !windows

package cli

import "os"

// parentProcessAlive reports whether the process that spawned this one is still
// running, by asking who this process's parent is now.
//
// A parent that exits leaves its children to be adopted — by init, or by the
// nearest subreaper where the system has one — so the answer changes and never
// changes back. This is cheaper and more exact than probing the pid itself,
// which cannot tell a live parent from a recycled pid.
func parentProcessAlive(pid int) bool {
	return os.Getppid() == pid
}
