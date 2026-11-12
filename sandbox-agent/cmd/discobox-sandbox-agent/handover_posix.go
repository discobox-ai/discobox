//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// handOver gives the directory at path to uid and gid, and sets its mode when
// mode is not zero. It opens path from / one name at a time, each a directory
// (O_DIRECTORY), and changes what it opened through the descriptor, so a name
// replaced while it runs cannot turn it onto anything else.
//
// A link is followed only where nobody but root can have put it: a link root
// owns, in a directory root owns that nobody else may write (rootsLink).
// macOS reaches /etc, /var and the template's /run through exactly such links
// (to /private/...), and a link the account made, anywhere, is refused. A
// directory that is not there is nothing to hand over.
func handOver(path string, uid, gid int, mode fs.FileMode) error {
	fd, found, err := openDir(path)
	if err != nil || !found {
		return err
	}
	defer unix.Close(fd)
	return changeOwner(path, fd, uid, gid, mode)
}

// handOverWithFiles hands over what the pool placed in the directory at path
// for the agent, as root's alone: each regular file directly in it with no
// other name, and then the directory. The pool's TLS key is one, which the
// agent must still read once it is the account.
//
// It does so only while the directory is root's and nobody else may write
// it, which is how the pool places it at each machine start, and once any
// ACL the account gave itself there while it held it is gone (clearACL). Then nothing in
// it is the account's doing, and the account can neither add a name for a
// root file nor remove one between the check and the change; a second name
// left from before is skipped. A directory the account already holds is left
// alone: what is in it is its own, and root reading or changing it is what an
// account restarting its agent could aim at a root file.
func handOverWithFiles(path string, uid, gid int) error {
	fd, found, err := openDir(path)
	if err != nil || !found {
		return err
	}
	defer unix.Close(fd)
	if !rootsAlone(fd) {
		return nil
	}
	// The mode says nobody else may write it only once no ACL says otherwise
	// (clearACL). Cleared by path, so the path must still be what fd is open
	// on: it is, since the directory is root's in /etc/discobox, which is
	// root's and sticky, where the account cannot move it.
	if err := clearACL(path); err != nil {
		return err
	}
	if !samePlace(fd, path) {
		return fmt.Errorf("hand over %s: it moved while it was handed over", path)
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return fmt.Errorf("hand over %s: %w", path, err)
	}
	dir := os.NewFile(uintptr(dup), path)
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("hand over %s: %w", path, err)
	}
	for _, entry := range entries {
		file, err := unix.Openat(fd, entry.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(file, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1 {
			err = unix.Fchown(file, uid, gid)
		}
		_ = unix.Close(file)
		if err != nil {
			return fmt.Errorf("hand over %s/%s: %w", path, entry.Name(), err)
		}
	}
	return changeOwner(path, fd, uid, gid, 0)
}

// samePlace reports whether path, followed as it is, is what fd is open on.
func samePlace(fd int, path string) bool {
	var open, named unix.Stat_t
	return unix.Fstat(fd, &open) == nil && unix.Stat(path, &named) == nil && open.Dev == named.Dev && open.Ino == named.Ino
}

// rootsAlone reports whether the directory fd is open on is root's and
// nobody else may write it, as its mode says (see clearACL for macOS).
func rootsAlone(fd int) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && st.Uid == 0 && st.Mode&0o022 == 0
}

// openDir opens the directory at path from / one name at a time, each a
// directory (O_DIRECTORY), following a link only where nobody but root can
// have made it (rootsLink); false is a directory that is not there.
func openDir(path string) (int, bool, error) {
	if !filepath.IsAbs(path) {
		return -1, false, fmt.Errorf("hand over %s: not an absolute path", path)
	}
	fd, err := openRootDir()
	if err != nil {
		return -1, false, fmt.Errorf("hand over %s: %w", path, err)
	}
	names := splitPath(path)
	for hops := 0; len(names) > 0; {
		name := names[0]
		names = names[1:]
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(fd)
			fd = next
			continue
		}
		if errors.Is(err, unix.ENOENT) {
			_ = unix.Close(fd)
			return -1, false, nil
		}
		target, ok := rootsLink(fd, name)
		if !ok {
			_ = unix.Close(fd)
			return -1, false, fmt.Errorf("hand over %s: %s is not a directory reached without a link only root could have made: %w", path, name, err)
		}
		if hops++; hops > 40 {
			_ = unix.Close(fd)
			return -1, false, fmt.Errorf("hand over %s: too many links", path)
		}
		names = append(splitPath(target), names...)
		if filepath.IsAbs(target) {
			_ = unix.Close(fd)
			if fd, err = openRootDir(); err != nil {
				return -1, false, fmt.Errorf("hand over %s: %w", path, err)
			}
		}
	}
	return fd, true, nil
}

// changeOwner gives what fd is open on to uid and gid, and sets its mode when
// mode is not zero.
func changeOwner(path string, fd, uid, gid int, mode fs.FileMode) error {
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return fmt.Errorf("hand over %s: %w", path, err)
	}
	if mode != 0 {
		if err := unix.Fchmod(fd, uint32(mode.Perm())|unixModeBits(mode)); err != nil {
			return fmt.Errorf("hand over %s: %w", path, err)
		}
	}
	return nil
}

func openRootDir() (int, error) {
	return unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// splitPath is path's names, "." dropped. ".." is kept: it is resolved by
// the directory it is opened in, the one the walk is at.
func splitPath(path string) []string {
	var names []string
	for _, name := range strings.Split(path, "/") {
		if name != "" && name != "." {
			names = append(names, name)
		}
	}
	return names
}

// rootsLink is where the link name in the directory dirfd points, when only
// root can have made it: the link is root's, and so is the directory, which
// nobody else may write. Nobody else can then have put the link there or
// change it while it is read.
func rootsLink(dirfd int, name string) (string, bool) {
	var link unix.Stat_t
	if !rootsAlone(dirfd) {
		return "", false
	}
	if unix.Fstatat(dirfd, name, &link, unix.AT_SYMLINK_NOFOLLOW) != nil || link.Mode&unix.S_IFMT != unix.S_IFLNK || link.Uid != 0 {
		return "", false
	}
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dirfd, name, buf)
	if err != nil || n <= 0 || n >= len(buf) {
		return "", false
	}
	return string(buf[:n]), true
}

// unixModeBits is the setuid, setgid and sticky bits of mode as chmod takes
// them.
func unixModeBits(mode fs.FileMode) uint32 {
	var bits uint32
	if mode&fs.ModeSticky != 0 {
		bits |= unix.S_ISVTX
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= unix.S_ISGID
	}
	if mode&fs.ModeSetuid != 0 {
		bits |= unix.S_ISUID
	}
	return bits
}

// bootstrapIsRoots refuses a bootstrap root cannot trust: one that is not a
// regular file owned by root and written by root alone, or one in a directory
// where anyone but root may replace it (writable by others and not sticky).
// The account ending its own agent starts it as root again, so a bootstrap
// the account could rewrite would choose the account root drops to.
func bootstrapIsRoots(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || ownerOf(info) != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("the bootstrap %s is not root's alone (%s, owner %d): root will not drop to the account it names", path, info.Mode(), ownerOf(info))
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !dir.IsDir() || ownerOf(dir) != 0 || (dir.Mode().Perm()&0o022 != 0 && dir.Mode()&fs.ModeSticky == 0) {
		return fmt.Errorf("the bootstrap's directory %s lets others replace it (%s, owner %d): root will not drop to the account it names", filepath.Dir(path), dir.Mode(), ownerOf(dir))
	}
	return nil
}

func ownerOf(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// closeFD closes a descriptor openDir returned.
func closeFD(fd int) error { return unix.Close(fd) }
