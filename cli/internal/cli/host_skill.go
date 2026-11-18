package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/hostskill"
	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/x/gitutil"
	"github.com/spf13/cobra"
)

func newHostSkillCommand() *cobra.Command {
	var install, uninstall bool
	var scope string
	var dirs []string
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print or manage the Discobox skill for your local agent",
		Long: `Print the Discobox skill for the agent on this machine.

Use --install or --uninstall to manage copies. By default these commands use
~/.claude/skills and ~/.agents/skills. Use --scope project to manage copies
at the root of your Git worktree, or --dir to name another skills directory.

To manage skills that go into boxes, use discobox admin skills.`,
		Args: cobra.NoArgs,
		// Printing and installing this local file do not need a server or its configuration.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if !install && !uninstall {
				if cmd.Flags().Changed("scope") || cmd.Flags().Changed("dir") {
					return errors.New("--scope and --dir require --install or --uninstall")
				}
				_, err := io.WriteString(out, hostskill.Content())
				return err
			}
			targets, err := hostSkillTargets(cmd.Context(), scope, dirs)
			if err != nil {
				return err
			}
			var errs []error
			var seen []fs.FileInfo
			for _, target := range targets {
				path := terminalSafe(target.path())
				dir, err := target.openDirectory(install)
				if err == nil && dir == nil {
					continue
				}
				var changed bool
				if err == nil {
					var info fs.FileInfo
					info, err = dir.Stat(".")
					if err == nil && !slices.ContainsFunc(seen, func(other fs.FileInfo) bool { return os.SameFile(info, other) }) {
						seen = append(seen, info)
						changed, err = changeHostSkill(dir, install)
					}
					_ = dir.Close()
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %s", path, terminalSafe(err.Error())))
				} else if changed {
					action := "Installed"
					if uninstall {
						action = "Removed"
					}
					fmt.Fprintf(out, "%s %s\n", action, path)
				}
			}
			return errors.Join(errs...)
		},
	}
	cmd.Flags().BoolVar(&install, "install", false, "Install the skill")
	cmd.Flags().BoolVar(&uninstall, "uninstall", false, "Remove the installed skill")
	cmd.Flags().StringVar(&scope, "scope", "user", "Skill scope (user or project)")
	cmd.Flags().StringArrayVar(&dirs, "dir", nil, "Skills directory to use (repeatable)")
	cmd.MarkFlagsMutuallyExclusive("install", "uninstall")
	cmd.MarkFlagsMutuallyExclusive("scope", "dir")
	return cmd
}

type hostSkillTarget struct {
	project string
	dir     string
}

func (t hostSkillTarget) path() string {
	return filepath.Join(t.project, t.dir, hostskill.Name)
}

func (t hostSkillTarget) openDirectory(install bool) (*os.Root, error) {
	if t.project == "" {
		if install {
			if err := os.MkdirAll(t.dir, 0o755); err != nil {
				return nil, err
			}
		}
		dir, err := os.OpenRoot(t.dir)
		if !install && errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return dir, err
	}

	// The checkout can contain links. Anchor operations at the worktree root,
	// where relative links within the tree still resolve.
	project, err := os.OpenRoot(t.project)
	if err != nil {
		return nil, err
	}
	defer project.Close()
	if install {
		if err := project.MkdirAll(t.dir, 0o755); err != nil {
			return nil, err
		}
	}
	dir, err := project.OpenRoot(t.dir)
	if !install && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return dir, err
}

func hostSkillTargets(ctx context.Context, scope string, dirs []string) ([]hostSkillTarget, error) {
	if len(dirs) == 0 {
		switch scope {
		case "user":
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			dirs = hostskill.Directories(home)
		case "project":
			project, err := gitutil.Root(ctx, ".")
			if err != nil {
				return nil, fmt.Errorf("--scope project requires a Git worktree: %s", terminalSafe(err.Error()))
			}
			var targets []hostSkillTarget
			for _, dir := range hostskill.Directories("") {
				targets = append(targets, hostSkillTarget{project: project, dir: dir})
			}
			return targets, nil
		default:
			return nil, fmt.Errorf("unknown skill scope %q (want user or project)", scope)
		}
	}
	var targets []hostSkillTarget
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			return nil, errors.New("--dir needs a directory")
		}
		path, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		targets = append(targets, hostSkillTarget{dir: path})
	}
	return targets, nil
}

// inspectHostSkill checks both the directory and its marker without accepting
// a symlink in place of either.
func inspectHostSkill(dir *os.Root, name string) (exists, marked bool, err error) {
	info, err := dir.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}
	if err != nil || !info.IsDir() {
		return err == nil, false, err
	}
	marker := filepath.Join(name, sandboxconfig.SkillFileName)
	info, err = dir.Lstat(marker)
	if errors.Is(err, fs.ErrNotExist) {
		return true, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > sandboxconfig.MaxSkillsBytes {
		return true, false, err
	}
	file, err := dir.Open(marker)
	if err != nil {
		return true, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, sandboxconfig.MaxSkillsBytes+1))
	return true, err == nil && hostskill.Marked(content), err
}

const hostSkillPrevious = ".discobox-host.previous"

func changeHostSkill(dir *os.Root, install bool) (bool, error) {
	// A single lock serializes all scopes, including aliases of one directory.
	// Leave it in the user's home so no checkout acquires a tracked lock file.
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	lockFile, err := os.OpenFile(filepath.Join(home, ".discobox-host.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	lock, err := filelock.TryAcquireFile(lockFile)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Release() }()

	exists, marked, err := inspectHostSkill(dir, hostskill.Name)
	if err != nil {
		return false, err
	}
	if exists && !marked {
		return false, errors.New("the skill directory is not managed by Discobox")
	}

	// A hard stop between the two install renames leaves the marked old
	// directory here. Recover it before attempting another install.
	previous, marked, err := inspectHostSkill(dir, hostSkillPrevious)
	if err != nil {
		return false, err
	}
	if previous {
		if !marked {
			return false, fmt.Errorf("%s is not managed by Discobox", hostSkillPrevious)
		}
		if install && !exists {
			if err := dir.Rename(hostSkillPrevious, hostskill.Name); err != nil {
				return false, err
			}
			exists = true
		} else if err := removeHostSkillTree(dir, hostSkillPrevious); err != nil {
			return false, err
		}
	}
	if !install {
		if !exists {
			return previous, nil
		}
		return true, removeHostSkillTree(dir, hostskill.Name)
	}

	// Stage the entire replacement under a private name. Only the marked
	// previous tree participates in recovery, so a hard stop during staging
	// cannot make a subsequent install trust a partially written tree.
	next := ".discobox-host-stage-" + rand.Text()
	if err := dir.Mkdir(next, 0o755); err != nil {
		return false, err
	}
	defer func() { _ = dir.RemoveAll(next) }()
	file, err := fsdurable.CreateIn(dir, filepath.Join(next, sandboxconfig.SkillFileName), 0o644)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Discard() }()
	if _, err := io.WriteString(file, hostskill.Content()); err != nil {
		return false, err
	}
	if err := file.Commit(); err != nil {
		return false, err
	}
	if exists {
		if err := dir.Rename(hostskill.Name, hostSkillPrevious); err != nil {
			return false, err
		}
		if err := fsdurable.SyncIn(dir, "."); err != nil {
			return false, err
		}
	}
	if err := dir.Rename(next, hostskill.Name); err != nil {
		if exists {
			return false, errors.Join(err, dir.Rename(hostSkillPrevious, hostskill.Name))
		}
		return false, err
	}
	if err := fsdurable.SyncIn(dir, "."); err != nil {
		return false, err
	}
	if exists {
		return true, removeHostSkillTree(dir, hostSkillPrevious)
	}
	return true, nil
}

func removeHostSkillTree(dir *os.Root, name string) error {
	trash := ".discobox-host-removed-" + rand.Text()
	if err := dir.Rename(name, trash); err != nil {
		return err
	}
	if err := fsdurable.SyncIn(dir, "."); err != nil {
		return err
	}
	return dir.RemoveAll(trash)
}
