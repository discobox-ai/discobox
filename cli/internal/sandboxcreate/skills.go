package sandboxcreate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// userSkillDirectories are the home-relative directories --user-skills reads,
// in the order they are read: the same two the sandbox installs into.
var userSkillDirectories = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".agents", "skills"),
}

// ReadSkills reads the skills a create carries, as content, since nothing
// past this machine can read its disk (ADR 26-10-09-395 §4).
//
// With user it reads ~/.claude/skills and then ~/.agents/skills, skipping
// either one that is absent; then each of dirs in the order given, each of
// which must exist. A later declaration of a name replaces an earlier one
// whole, so an explicit directory always beats the home directory. Every
// immediate subdirectory holding a SKILL.md is one skill, named for the
// subdirectory; anything else is not a skill and is skipped.
func ReadSkills(dirs []string, user bool) (sandboxconfig.Skills, error) {
	skills := sandboxconfig.Skills{}
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("--user-skills: %w", err)
		}
		for _, dir := range userSkillDirectories {
			if err := readSkillsDir(skills, filepath.Join(home, dir), true); err != nil {
				return nil, fmt.Errorf("--user-skills: %w", err)
			}
		}
	}
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			return nil, errors.New("--skills needs a directory")
		}
		if err := readSkillsDir(skills, dir, false); err != nil {
			return nil, fmt.Errorf("--skills %s: %w", dir, err)
		}
	}
	if len(skills) == 0 {
		return nil, nil
	}
	// The reads stopped at the limits; this is every other rule.
	if err := skills.Validate(); err != nil {
		return nil, err
	}
	return skills, nil
}

// SetCreateSandboxSkills puts skills on the create request, or nothing when
// there are none.
func SetCreateSandboxSkills(config *apiclientgen.SandboxCreateConfig, skills sandboxconfig.Skills) {
	if len(skills) == 0 {
		return
	}
	out := make(apiclientgen.SandboxCreateConfigSkills, len(skills))
	for name, skill := range skills {
		files := make([]apiclientgen.SandboxSkillFile, 0, len(skill.Files))
		for _, file := range skill.Files {
			apiFile := apiclientgen.SandboxSkillFile{Path: file.Path, Content: file.Content}
			if file.Executable {
				apiFile.SetExecutable(apiclientgen.NewOptBool(true))
			}
			files = append(files, apiFile)
		}
		out[name] = apiclientgen.SandboxSkill{Skill: skill.Skill, Files: files}
	}
	config.SetSkills(apiclientgen.NewOptSandboxCreateConfigSkills(out))
}

// readSkillsDir adds every skill in dir to skills, replacing one of the same
// name. optional is a directory that may be absent.
func readSkillsDir(skills sandboxconfig.Skills, dir string, optional bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Stat, not the entry's own type: a skill in ~/.claude/skills is often
		// a link into a checkout, and it is read where it means something.
		// Only what is not there, or is not a directory, is not a skill: a
		// directory that cannot be read is one the caller asked for and
		// would otherwise go without.
		root := filepath.Join(dir, name)
		info, err := os.Stat(root)
		if errors.Is(err, fs.ErrNotExist) {
			// A dangling link.
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		// What the skills read so far take of the limits, leaving out one of
		// this name, which this one replaces whole.
		usedBytes, usedFiles := skillsBesides(skills, name)
		// Whether it is a skill at all comes before what it would take: a
		// directory with no SKILL.md is skipped, whatever the count.
		textPath := filepath.Join(root, sandboxconfig.SkillFileName)
		textInfo, err := os.Stat(textPath)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("skill %s: %w", name, err)
		}
		if usedFiles >= sandboxconfig.MaxSkillFiles {
			// Its SKILL.md alone would be one file too many.
			return overLimit(skills, name, overLimitError{fmt.Sprintf("it takes the skills past the %d files a discobox takes", sandboxconfig.MaxSkillFiles)})
		}
		if textInfo.Size() > int64(sandboxconfig.MaxSkillsBytes-usedBytes) {
			return overLimit(skills, name, overLimitError{fmt.Sprintf("its %s takes the skills past the %d bytes a discobox takes", sandboxconfig.SkillFileName, sandboxconfig.MaxSkillsBytes)})
		}
		text, err := os.ReadFile(textPath)
		if err != nil {
			return fmt.Errorf("skill %s: %w", name, err)
		}
		if err := sandboxconfig.ValidateSkillName(name); err != nil {
			return err
		}
		// The walk below does not follow a link at its root either.
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		files, err := readSkillFiles(resolved, usedBytes+len(text), usedFiles+1)
		if err != nil {
			return overLimit(skills, name, err)
		}
		skills[name] = sandboxconfig.Skill{Skill: string(text), Files: files}
	}
	return nil
}

// overLimitError is a read that stopped at the limits a discobox takes.
type overLimitError struct{ reason string }

func (e overLimitError) Error() string { return e.reason }

// overLimit names the skill a read of failed, and when it stopped at the
// limits, the largest of the skills already read: what there is to leave out.
func overLimit(skills sandboxconfig.Skills, name string, err error) error {
	if errors.As(err, new(overLimitError)) && len(skills) > 0 {
		return fmt.Errorf("skill %s: %w; the largest read before it are %s", name, err, largestSkills(skills, 3))
	}
	return fmt.Errorf("skill %s: %w", name, err)
}

// skillsBesides is what every skill but name takes of the limits: its bytes,
// and its files with its SKILL.md.
func skillsBesides(skills sandboxconfig.Skills, name string) (bytes, files int) {
	for other, skill := range skills {
		if other != name {
			bytes += skill.Size()
			files += 1 + len(skill.Files)
		}
	}
	return bytes, files
}

// readSkillFiles reads every file under a skill's root but its SKILL.md. A
// link to a file is read as the file; a link to a directory is skipped, so a
// link cannot loop, and so is .git, which is a checkout's and not the skill's.
//
// It stops at the first file past what a discobox takes, before reading it: a
// link to a whole checkout reads as one skill, build output and all, and the
// refusal should not wait for every byte of it.
//
// size and count are what the skills already take, this one's SKILL.md
// included, so the stop is at the limits on the whole request rather than on
// one skill: a directory of many skills each under the limit stops too.
func readSkillFiles(root string, size, count int) ([]sandboxconfig.SkillFile, error) {
	var files []sandboxconfig.SkillFile
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if rel == sandboxconfig.SkillFileName || path.Base(rel) == ".git" {
			return nil
		}
		info, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			// A dangling link is not a file of the skill.
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			// A link to a directory, a socket: not a file of the skill.
			return nil
		}
		if size += len(rel) + int(info.Size()); size > sandboxconfig.MaxSkillsBytes {
			return overLimitError{fmt.Sprintf("it takes the skills past the %d bytes a discobox takes, at %s", sandboxconfig.MaxSkillsBytes, rel)}
		}
		if count+len(files) >= sandboxconfig.MaxSkillFiles {
			return overLimitError{fmt.Sprintf("it takes the skills past the %d files a discobox takes", sandboxconfig.MaxSkillFiles)}
		}
		//nolint:gosec // following links is the point: these are the caller's own files, read as the caller
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, sandboxconfig.SkillFile{
			Path:    rel,
			Content: content,
			// Windows has no executable bit to read, and a script there is
			// one by its name, which the sandbox's own platform decides.
			Executable: runtime.GOOS != "windows" && info.Mode()&0o111 != 0,
		})
		return nil
	})
	return files, err
}

// largestSkills names the n largest skills with their sizes, for a refusal
// that has to say what to leave out.
func largestSkills(skills sandboxconfig.Skills, n int) string {
	names := skills.Names()
	slices.SortStableFunc(names, func(a, b string) int { return skills[b].Size() - skills[a].Size() })
	if len(names) > n {
		names = names[:n]
	}
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s (%d bytes)", name, skills[name].Size())
	}
	return strings.Join(parts, ", ")
}
