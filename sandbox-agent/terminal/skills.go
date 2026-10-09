package terminal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// ProjectSkillsDir is where a repository declares the agent skills it wants the
// harness to have, relative to the primary source's root.
//
// They are the repository's own skills rather than the developer's: a clone on
// a laptop leaves the harness's skill directories untouched, and the same
// checkout inside a sandbox has them installed. That is the point — a
// repository can teach the agent things that only apply when it is being worked
// on here, without asking anyone to install anything.
//
// It sits beside `.discobox/services` (ADR 0070) and `.discobox/hooks`, and is
// read the same way: relative to the primary source directory, and absent for
// almost every repository.
const ProjectSkillsDir = ".discobox/skills"

// BuiltinSkillsDir is where the image installs the skills discobox itself
// ships: the ones for what is in every sandbox whatever it was made from, so
// they cannot come from the repository being worked on (ADR 0080).
//
// `discobox-access`, `discobox-review` and `discobox` are what it holds today,
// for two reasons rather than one. The first two document a tool installed in
// every sandbox, which an agent never told about reaches past for what it was
// trained on instead: it writes a token into a config file or asks the user to
// paste one into the chat, which is the one thing the credential protocol
// exists to avoid; it reviews its own change in its own context, which agrees
// with itself. `discobox` documents no tool — it describes the sandbox itself,
// which an agent has no way to learn from inside and will otherwise guess
// wrong about: that `origin` can be pushed to, that a sentinel is a real
// credential to repair, that a port it is serving needs some further step to
// reach the user.
const BuiltinSkillsDir = "/usr/local/share/discobox/skills"

// skillDirectories are the home-relative directories coding harnesses read
// skills from. Every one of them gets the same copy: the repository declaring
// the skills does not know which harness the sandbox runs, and a directory the
// running harness ignores costs a few files.
var skillDirectories = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".agents", "skills"),
}

// installSkills copies the image's skills, the primary source's declared
// skills, and the skills the sandbox was created with into the run user's
// skill directories.
//
// It runs on the primary terminal's first launch only, and from there rather
// than from the installer that runs before every terminal: the copies are the
// harness's files once they land, and a harness that reorganizes or prunes them
// must not find them restored underneath it on the next launch. It also has to
// run after the source-delivery wait, which is the same reason — before the
// source is in place there is nothing to read.
//
// The image's go first, the repository's second and the sandbox's own last,
// so each wins over the one before on a name they share: each is the more
// specific declaration (ADR 26-10-09-395 §3).
func (s *Service) installSkills() error {
	// The control plane refused what it would not carry; this is the line
	// held again where it matters, before anything is written under home.
	if err := s.skills.Validate(); err != nil {
		return fmt.Errorf("install the discobox's skills: %w", err)
	}
	// The repository root, asked of the exec manager rather than derived here:
	// it is the same answer `services` discovers its declarations under, and a
	// second derivation of "which directory is the sandbox working on" drifts
	// from the one execs actually start in.
	source, err := s.execs.DefaultWorkdir()
	if err != nil {
		return fmt.Errorf("install %s: %w", ProjectSkillsDir, err)
	}
	// An image built before ADR 0080 has no built-in skills, which is an older
	// image rather than a misconfiguration: failing its primary launch would
	// take away the sandbox to protect the documentation. A directory that is
	// there and cannot be read still fails, since at that point something is
	// wrong with the image rather than absent from it.
	builtin, err := declaredSkills(BuiltinSkillsDir)
	if err != nil {
		return err
	}
	// What each side declares is read before the home they would be copied into
	// is resolved, because almost every repository declares nothing. Resolving
	// home is how the copy finds its destination, not part of deciding there is
	// no copy to make — and it can fail. Asking first would fail the primary
	// launch of every sandbox whose run user has no home resolvable, over
	// directories that are empty.
	declared := filepath.Join(source, ProjectSkillsDir)
	project, err := declaredSkills(declared)
	if err != nil {
		return err
	}
	if len(builtin) == 0 && len(project) == 0 && len(s.skills) == 0 {
		return nil
	}
	// HOME as the exec that will read these files resolves it, which is what
	// completes the run user's home when no layer named one.
	env := execs.EnvWithRuntimeDefaults(execs.MergeEnv(s.env, nil), s.defaultUser)
	home, err := resolveHomeDir(s.homeDirectory, s.defaultUser, env)
	if err != nil {
		return fmt.Errorf("install %s: %w", ProjectSkillsDir, err)
	}
	if err := copySkills(BuiltinSkillsDir, builtin, home, s.defaultUser); err != nil {
		return err
	}
	if err := copySkills(declared, project, home, s.defaultUser); err != nil {
		return err
	}
	return writeSkills(s.skills, home, s.defaultUser)
}

// writeSkills writes skills, which the caller has validated, into every skill
// directory under home: the same overlay copySkillTree makes, from content
// rather than a tree.
func writeSkills(skills sandboxconfig.Skills, home string, user *execs.User) error {
	uid, gid := userIDs(user)
	for _, dir := range skillDirectories {
		for _, name := range skills.Names() {
			skill := skills[name]
			root := filepath.Join(home, dir, name)
			if err := writeSkillFile(filepath.Join(root, sandboxconfig.SkillFileName), strings.NewReader(skill.Skill), false, uid, gid); err != nil {
				return fmt.Errorf("install skill %s into %s: %w", name, dir, err)
			}
			for _, file := range skill.Files {
				target := filepath.Join(root, filepath.FromSlash(file.Path))
				if err := writeSkillFile(target, bytes.NewReader(file.Content), file.Executable, uid, gid); err != nil {
					return fmt.Errorf("install skill %s into %s: %w", name, dir, err)
				}
			}
		}
	}
	return nil
}

// declaredSkills is what a repository declares under ProjectSkillsDir, and
// nothing at all when it declares none — no directory, or an empty one.
func declaredSkills(from string) ([]fs.DirEntry, error) {
	entries, err := os.ReadDir(from)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", from, err)
	}
	return entries, nil
}

// copySkills copies the tree at from, whose entries the caller has already
// read, into every skill directory under home.
func copySkills(from string, entries []fs.DirEntry, home string, user *execs.User) error {
	if len(entries) == 0 {
		return nil
	}
	uid, gid := userIDs(user)
	for _, dir := range skillDirectories {
		if err := copySkillTree(from, filepath.Join(home, dir), uid, gid); err != nil {
			return fmt.Errorf("install %s into %s: %w", from, dir, err)
		}
	}
	return nil
}

// copySkillTree copies from onto to, overwriting a file of the same name and
// leaving everything else in the destination alone. The repository is copied
// after the image, so it wins on a name they share — it is the more specific
// declaration — but neither owns the directory, which the harness config's
// files and the harness itself also write into.
func copySkillTree(from, to string, uid, gid *int64) error {
	return filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		switch {
		case entry.IsDir():
			created, err := mkdirAllTracked(target, 0o755)
			if err != nil {
				return err
			}
			return chownAll(created, uid, gid)
		case entry.Type().IsRegular():
			return copySkillFile(path, target, entry, uid, gid)
		default:
			// A symlink, socket, or device is not a skill. A symlink is the
			// only one a repository plausibly holds, and copying it would
			// either dangle or point at something outside the tree once it is
			// resolved from a home directory instead of the checkout.
			return nil
		}
	})
}

func copySkillFile(from, to string, entry fs.DirEntry, uid, gid *int64) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	// The walk created the directory before reaching its files.
	return writeSkillFileIn(to, source, info.Mode()&0o111 != 0, uid, gid)
}

// writeSkillFile writes one skill file at to, creating the directories it is
// in and giving the ones it created to the run user.
func writeSkillFile(to string, content io.Reader, executable bool, uid, gid *int64) error {
	created, err := mkdirAllTracked(filepath.Dir(to), 0o755)
	if err != nil {
		return err
	}
	if err := chownAll(created, uid, gid); err != nil {
		return err
	}
	return writeSkillFileIn(to, content, executable, uid, gid)
}

// writeSkillFileIn writes one skill file at to, in a directory that exists.
func writeSkillFileIn(to string, content io.Reader, executable bool, uid, gid *int64) error {
	// A skill's helper script has to stay runnable, so the executable bit
	// carries over. Nothing else does: the destination is another user's home
	// directory, not a copy of the checkout's permissions.
	perm := fs.FileMode(0o644)
	if executable {
		perm = 0o755
	}
	destination, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, content); err != nil {
		destination.Close()
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	// O_CREATE leaves an existing file's mode alone, so the mode is set
	// outright rather than left to whatever the previous copy had.
	if err := os.Chmod(to, perm); err != nil {
		return err
	}
	return chownAll([]string{to}, uid, gid)
}

// userIDs is the run user's ids, or none when the sandbox named nobody.
func userIDs(user *execs.User) (uid, gid *int64) {
	if user == nil {
		return nil, nil
	}
	return user.UID, user.GID
}

// chownAll gives paths to the run user, or leaves them to whoever created them
// when the sandbox named nobody and the agent's own identity stands.
func chownAll(paths []string, uid, gid *int64) error {
	if uid == nil || gid == nil {
		return nil
	}
	for _, path := range paths {
		if err := os.Chown(path, int(*uid), int(*gid)); err != nil {
			return err
		}
	}
	return nil
}
