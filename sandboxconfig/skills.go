package sandboxconfig

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
)

// SkillFileName is the file a skill directory is recognized by, and where a
// Skill's Skill text lands inside it.
const SkillFileName = "SKILL.md"

// MaxSkillsBytes bounds the skills one sandbox is created with: every
// SKILL.md, and every file's decoded content and its path. Skills ride the bootstrap, which
// every backend places before the agent starts, and they are the first thing
// in it with no natural size (ADR 26-10-09-395 §1).
const MaxSkillsBytes = 1 << 20

// MaxSkillFiles bounds how many files those skills hold in all, each skill's
// SKILL.md among them: what each costs the bootstrap past its bytes is its
// JSON, and a request of countless one-byte files would be large while
// counting small.
const MaxSkillFiles = 1000

// MaxSkillSegmentBytes bounds one skill name or one segment of a file's path:
// the longest file name the common filesystems hold.
const MaxSkillSegmentBytes = 255

// Skills are the skills a sandbox is created with, by name: one directory each
// in the harness's skill directories, installed after the image's and the
// repository's and winning on a name they share (ADR 26-10-09-395 §3).
type Skills map[string]Skill

// Skill is one skill's content: its SKILL.md, and the files beside it.
type Skill struct {
	Skill string      `json:"skill"`
	Files []SkillFile `json:"files,omitempty"`
}

// SkillFile is one file of a skill other than its SKILL.md. Path is relative
// to the skill's directory, slash-separated whatever the platform.
type SkillFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	// Executable is the one permission a skill's file carries: a helper
	// script has to stay runnable (ADR 0072 §1).
	Executable bool `json:"executable,omitempty"`
}

// Names is the skills' names, sorted.
func (s Skills) Names() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// clone is a copy that shares nothing with s; nil stays nil.
func (s Skills) clone() Skills {
	if s == nil {
		return nil
	}
	out := make(Skills, len(s))
	for name, skill := range s {
		files := slices.Clone(skill.Files)
		for i := range files {
			files[i].Content = slices.Clone(files[i].Content)
		}
		out[name] = Skill{Skill: skill.Skill, Files: files}
	}
	return out
}

// Size is what the skills add to the bootstrap, in bytes (Skill.Size): what
// MaxSkillsBytes bounds.
func (s Skills) Size() int {
	total := 0
	for _, skill := range s {
		total += skill.Size()
	}
	return total
}

// Size is what the skill adds to the bootstrap, in bytes: its SKILL.md, and
// every file's content and path. A path counts because a skill of many empty
// files is as large a bootstrap as one of few full ones.
func (s Skill) Size() int {
	total := len(s.Skill)
	for _, file := range s.Files {
		total += len(file.Path) + len(file.Content)
	}
	return total
}

// Validate is every refusal a set of skills can earn, so the server, the
// client building a request, and the sandbox writing it into a home directory
// hold the same line: a name is one directory, a file stays inside its skill,
// every name and path can be written on every platform a sandbox runs on, and
// the whole is no more than MaxSkillsBytes in MaxSkillFiles files.
func (s Skills) Validate() error {
	folded := make(map[string]string, len(s))
	files := 0
	for _, name := range s.Names() {
		if err := ValidateSkillName(name); err != nil {
			return err
		}
		// Two names one case-insensitive filesystem holds as one directory
		// would be written over each other on macOS and Windows.
		if other, ok := folded[foldKey(name)]; ok {
			return fmt.Errorf("skills %q and %q differ only in case", other, name)
		}
		folded[foldKey(name)] = name
		if err := s[name].validate(); err != nil {
			return fmt.Errorf("skill %q: %w", name, err)
		}
		// Its SKILL.md is a file it installs like any other.
		files += 1 + len(s[name].Files)
	}
	if files > MaxSkillFiles {
		return fmt.Errorf("skills hold %d files; a discobox takes at most %d", files, MaxSkillFiles)
	}
	if size := s.Size(); size > MaxSkillsBytes {
		return fmt.Errorf("skills are %d bytes; a discobox takes at most %d", size, MaxSkillsBytes)
	}
	return nil
}

// ValidateSkillName refuses a name that is not one visible directory: the
// name is where the skill is written, under a directory the harness lists.
func ValidateSkillName(name string) error {
	if name == "" {
		return errors.New("a skill needs a name")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("skill name %q is hidden or not a directory", name)
	}
	if err := portableSegment(name); err != nil {
		return fmt.Errorf("skill name %q %w", name, err)
	}
	return nil
}

func (s Skill) validate() error {
	if strings.TrimSpace(s.Skill) == "" {
		return fmt.Errorf("%s is empty", SkillFileName)
	}
	// Keyed by the path's case fold: a case-insensitive filesystem holds two
	// paths that differ only in case as one.
	seen := make(map[string]string, len(s.Files))
	for _, file := range s.Files {
		if err := validateSkillFilePath(file.Path); err != nil {
			return err
		}
		if other, ok := seen[foldKey(file.Path)]; ok {
			return fmt.Errorf("files %q and %q are one file where case does not count", other, file.Path)
		}
		seen[foldKey(file.Path)] = file.Path
	}
	// A file may not be a directory another file is under: written in either
	// order, one of them would fail.
	for _, file := range s.Files {
		for dir := path.Dir(file.Path); dir != "."; dir = path.Dir(dir) {
			if other, ok := seen[foldKey(dir)]; ok {
				return fmt.Errorf("file %q is also the directory %q is in", other, file.Path)
			}
		}
	}
	return nil
}

// validateSkillFilePath refuses a path that leaves its skill, names its
// SKILL.md (which is the Skill text), is spelled two ways, or cannot be
// written on every platform a sandbox runs on.
func validateSkillFilePath(p string) error {
	switch {
	case p == "":
		return errors.New("a file needs a path")
	case strings.Contains(p, `\`):
		return fmt.Errorf("file path %q is not slash-separated", p)
	case path.IsAbs(p), path.Clean(p) != p, p == "." || p == ".." || strings.HasPrefix(p, "../"):
		return fmt.Errorf("file path %q is not a clean path inside the skill", p)
	case strings.EqualFold(strings.SplitN(p, "/", 2)[0], SkillFileName):
		// SKILL.md is a file the sandbox writes first; a path under it would
		// need it to be a directory too.
		return fmt.Errorf("file path %q is, or is under, the skill's own text; give that as the skill", p)
	}
	for _, segment := range strings.Split(p, "/") {
		if err := portableSegment(segment); err != nil {
			return fmt.Errorf("file path %q %w", p, err)
		}
	}
	return nil
}

// foldKey is s with every rune replaced by the smallest of its case-fold
// orbit, so two strings strings.EqualFold holds equal have one key. Lowering
// is not that: Σ, σ and ς fold together, and ς lowers to itself.
func foldKey(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}

// windowsReservedNames are the device names Windows refuses as a file or
// directory, with or without an extension.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	// Windows reads the superscript digits as device numbers too.
	"COM¹": true, "COM²": true, "COM³": true, "LPT¹": true, "LPT²": true, "LPT³": true,
}

// portableSegment refuses one path segment that some platform a sandbox runs
// on cannot write: the characters Windows reserves, a control character, a
// trailing dot or space, or a device name. The server refuses the same
// skills, so a create is refused instead of a Windows sandbox's first launch.
// The result reads after the name it is about ("skill name %q %w").
func portableSegment(segment string) error {
	if len(segment) > MaxSkillSegmentBytes {
		return fmt.Errorf("is %d bytes; a file name holds at most %d", len(segment), MaxSkillSegmentBytes)
	}
	for _, r := range segment {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			return fmt.Errorf("holds %q, which a Windows sandbox cannot write", r)
		}
	}
	if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
		return errors.New("ends in a dot or a space, which a Windows sandbox drops")
	}
	stem, _, _ := strings.Cut(segment, ".")
	if windowsReservedNames[strings.ToUpper(stem)] {
		return errors.New("is a device name on Windows")
	}
	return nil
}
