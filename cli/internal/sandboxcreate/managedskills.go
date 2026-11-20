package sandboxcreate

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// ReadManagedSkills resolves publisher-side sources. A Git source is checked
// out in private temporary storage and only its bounded skill files leave it.
func ReadManagedSkills(ctx context.Context, value string, warn func(string)) (sandboxconfig.Skills, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	repository, dir, err := ParseSkills(value, cwd)
	if err != nil {
		return nil, "", err
	}
	if dir != "" {
		skills, err := ReadSkills([]string{dir}, false, warn)
		return skills, "", err
	}
	if err := sandboxconfig.ValidateSkillsRepository(repository.Source.Location, repository.Source.Ref); err != nil {
		return nil, "", err
	}
	checkout, err := os.MkdirTemp("", "discobox-skills-source-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(checkout)
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = checkout
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("read skills repository with git %s: %w", args[0], err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := run("init", "--quiet"); err != nil {
		return nil, "", err
	}
	ref := repository.Source.Ref
	if ref == "" {
		ref = "HEAD"
	}
	if _, err := run("fetch", "--quiet", "--depth=1", "--no-tags", "--", repository.Source.Location, ref); err != nil {
		return nil, "", err
	}
	if _, err := run("checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return nil, "", err
	}
	commit, err := run("rev-parse", "HEAD")
	if err != nil {
		return nil, "", err
	}
	dir = filepath.Join(checkout, filepath.FromSlash(repository.Directory))
	dir, err = confinedSkillPath(checkout, dir)
	if err != nil {
		return nil, "", err
	}
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return fs.SkipDir
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			resolved, err := confinedSkillPath(checkout, path)
			if err != nil {
				return err
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return err
			}
			if info.IsDir() && filepath.Dir(path) == dir {
				// The ordinary reader follows top-level skill directories.
				// Check the target's own links as well.
				return filepath.WalkDir(resolved, func(child string, e fs.DirEntry, err error) error {
					if err == nil && e.Type()&fs.ModeSymlink != 0 {
						_, err = confinedSkillPath(checkout, child)
					}
					return err
				})
			}
		}
		return nil
	}); err != nil {
		return nil, "", err
	}
	skills, err := ReadSkills([]string{dir}, false, warn)
	return skills, commit, err
}

func confinedSkillPath(checkout, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || !filepath.IsLocal(relative) || slices.Contains(strings.Split(relative, string(filepath.Separator)), ".git") {
		return "", fmt.Errorf("skills path %q leaves the published Git tree", path)
	}
	return resolved, nil
}
