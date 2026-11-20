//go:build !unix

package terminal

import (
	"io/fs"
	"os"
	"path/filepath"
)

func isPrivateSkillsStage(info fs.FileInfo) bool {
	return info.IsDir()
}

func renameBetweenSkillsRoots(source *os.Root, from string, target *os.Root, to string) error {
	// The Windows sandbox's agent and terminal run under the same account.
	return os.Rename(filepath.Join(source.Name(), from), filepath.Join(target.Name(), to))
}
