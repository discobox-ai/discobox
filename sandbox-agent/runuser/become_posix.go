//go:build !windows

package runuser

import "syscall"

// Become makes this whole process a: its group set, then its gid, then its
// uid, in that order, since a process that has given up root can change none
// of them. The calls apply to every thread (Go does that on Linux; on macOS
// they are per process).
func Become(a Account) error {
	if err := syscall.Setgroups(a.Groups); err != nil {
		return err
	}
	if err := syscall.Setgid(a.GID); err != nil {
		return err
	}
	return syscall.Setuid(a.UID)
}
