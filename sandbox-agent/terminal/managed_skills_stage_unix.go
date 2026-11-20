//go:build unix

package terminal

import (
	"io/fs"
	"os"
	"path/filepath"

	"syscall"

	"golang.org/x/sys/unix"
)

func isPrivateSkillsStage(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid() && info.IsDir() && info.Mode().Perm()&0o077 == 0
}

func renameBetweenSkillsRoots(source *os.Root, from string, target *os.Root, to string) error {
	sourceParent, err := source.Open(filepath.Dir(from))
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	targetParent, err := target.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer targetParent.Close()
	return unix.Renameat(int(sourceParent.Fd()), filepath.Base(from), int(targetParent.Fd()), filepath.Base(to))
}
