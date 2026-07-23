//go:build unix

// Package execidentity builds the os/exec identity switch a subprocess needs
// to run as a specific OS user, so its file ownership matches whichever
// directory it operates on instead of inheriting the caller's own identity.
package execidentity

import "syscall"

// SysProcAttr returns the SysProcAttr that makes a subprocess run as uid/gid,
// or nil when uid is negative, meaning "inherit the caller's own identity."
//
// NoSetGroups is always set: the codebase models ownership as a plain uid:gid
// pair (see chownRecursive), never supplementary groups, and without it Go
// unconditionally calls setgroups before exec, which fails for any caller
// that isn't fully privileged even when uid/gid otherwise need no change.
func SysProcAttr(uid, gid int) *syscall.SysProcAttr {
	if uid < 0 {
		return nil
	}
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:         uint32(uid),
			Gid:         uint32(gid),
			NoSetGroups: true,
		},
	}
}
