package runcca

import (
	"fmt"
	"os"
	"path/filepath"
)

// systemPath is where runc is looked for when the wrapper's environment
// names no PATH: the directories a Debian-family root's PATH holds.
const systemPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// RealRunc is the runc this wrapper delegates to: the first `runc` on path
// that is not self, the wrapper's own executable. Where it lives is the box's
// to say, never this package's: Docker's containerd.io puts it at
// /usr/bin/runc, Ubuntu's runc package at /usr/sbin/runc (ADR 26-10-16-423
// §9). The wrapper is `runc` on a directory prepended to containerd's and
// dockerd's PATH, so the search steps over every entry that is the wrapper
// itself, however it is linked, rather than re-exec it. An empty path is
// systemPath.
func RealRunc(path string, self os.FileInfo) (string, error) {
	if path == "" {
		path = systemPath
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			// A relative entry resolves against wherever the caller
			// happens to be, which is not a place to find a runtime.
			continue
		}
		candidate := filepath.Join(dir, "runc")
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if self != nil && os.SameFile(info, self) {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("no runc on %s but this wrapper", path)
}

// SelfInfo is the running executable, for RealRunc to step over.
func SelfInfo() (os.FileInfo, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return os.Stat(exe)
}
