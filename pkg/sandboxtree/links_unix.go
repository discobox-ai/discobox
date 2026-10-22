//go:build unix

package sandboxtree

import (
	"os"
	"syscall"
)

// linkIndex remembers the first archive name given to each inode, so a file
// reachable by several names is stored once and referenced afterwards.
//
// It matters more than it looks: pnpm links every package in its store into
// every node_modules that uses it, and a workspace with a handful of projects
// can hold the same megabyte a dozen times. Storing each one would multiply an
// export by whatever the link factor happens to be.
type linkIndex struct {
	names map[inode]string
}

type inode struct {
	device uint64
	number uint64
}

func newLinkIndex() *linkIndex {
	return &linkIndex{names: make(map[inode]string)}
}

// seen records name as an archive entry for info's inode and reports the name
// already stored for it, if any. A file with one link is never recorded: it can
// have no second name, and remembering every regular file in a workspace would
// cost a map entry per file for nothing.
func (l *linkIndex) seen(info os.FileInfo, name string) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink < 2 {
		return "", false
	}
	key := inode{device: uint64(stat.Dev), number: uint64(stat.Ino)} //nolint:unconvert,nolintlint // Dev and Ino differ in width across unix platforms.
	if existing, ok := l.names[key]; ok {
		return existing, true
	}
	l.names[key] = name
	return "", false
}
