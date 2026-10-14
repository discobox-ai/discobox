//go:build darwin || linux

package terminal

import "os"

// linkSkillDirectory makes link a symbolic link to the skill directory
// target, owned by the run user like everything else in its home.
func linkSkillDirectory(target, link string, uid, gid *int64) error {
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	if uid == nil || gid == nil {
		return nil
	}
	return os.Lchown(link, int(*uid), int(*gid))
}
