//go:build !unix

package execidentity

import "syscall"

// SysProcAttr returns nil on platforms without Unix credential switching: the
// pool agent runs on Unix, so identity switching is a Unix-only concern. The
// stub exists only so the package cross-compiles (e.g. for editor/LSP checks).
func SysProcAttr(_, _ int) *syscall.SysProcAttr {
	return nil
}
