package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

const managedSkillJournal = "managed-skills.json"

var errSkillsRolledBack = errors.New("the previous skills revision was restored")

const managedSkillBackups = ".discobox-skill-backups"

type installedSkills struct {
	Owner    string        `json:"owner"`
	Revision int64         `json:"revision"`
	Names    []string      `json:"names"`
	Stage    string        `json:"stage,omitempty"`
	Moves    []skillRename `json:"moves,omitempty"`
}

type skillRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// SetBootSkillsLoader supplies the pool's first applied document before the
// primary terminal can start. It is set while wiring the agent.
func (s *Service) SetBootSkillsLoader(load func(context.Context) error) {
	s.loadBootSkills = load
}

// SetManagedSkills keeps the latest delivered revision. Before the first
// terminal starts, the baseline install runs first so shadowed entries exist.
func (s *Service) SetManagedSkills(ctx context.Context, desired sandboxconfig.ManagedSkills) error {
	s.skillsMu.Lock()
	defer s.skillsMu.Unlock()
	if s.managedSkills == nil || desired.Revision > s.managedSkills.Revision {
		s.managedSkills = &desired
	}
	if s.primaryState != nil {
		launched, err := s.primaryState.PrimaryTerminalLaunched(ctx)
		if err != nil || !launched {
			return err
		}
	}
	return s.applyManagedSkillsLocked()
}

func (s *Service) ManagedSkillsRevision() int64 {
	return s.appliedManagedSkills.Load()
}

// InstalledSkillNames is the harness-installed inventory, including legacy
// copies and linked skills. It never reads the contents of a skill file.
func (s *Service) InstalledSkillNames() []string {
	s.skillsMu.Lock()
	defer s.skillsMu.Unlock()
	home, err := resolveHomeDir(s.homeDirectory, s.defaultUser, s.execs.Environment(nil, s.defaultUser))
	if err != nil {
		return nil
	}
	found := make(map[string]struct{})
	for _, dir := range skillDirectories {
		names, _ := sourceSkillNames(filepath.Join(home, dir))
		for _, name := range names {
			found[name] = struct{}{}
		}
	}
	names := slices.AppendSeq([]string{}, maps.Keys(found))
	slices.Sort(names)
	return names[:min(len(names), sandboxconfig.MaxSkillFiles)]
}

// applyManagedSkillsLocked replaces directory entries as a whole, including
// symlinks into checkouts. Terminal startup holds the same lock.
func (s *Service) applyManagedSkillsLocked() (retErr error) {
	if s.managedSkills != nil && s.ManagedSkillsRevision() >= s.managedSkills.Revision {
		return nil
	}
	if s.managedSkillsStateDir == "" {
		if s.managedSkills == nil {
			return nil
		}
		return errors.New("managed skills require persistent agent state")
	}
	journal := filepath.Join(s.managedSkillsStateDir, managedSkillJournal)
	var previous installedSkills
	data, err := os.ReadFile(journal)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &previous); err != nil {
			return fmt.Errorf("read installed skills revision: %w", err)
		}
	case errors.Is(err, fs.ErrNotExist):
		if s.managedSkills == nil {
			return nil
		}
		previous.Owner = s.managedSkillsOwner
	default:
		return err
	}
	desired := sandboxconfig.ManagedSkills{}
	if s.managedSkills != nil {
		desired = *s.managedSkills
		if err := desired.Validate(); err != nil {
			return err
		}
	}
	home, err := resolveHomeDir(s.homeDirectory, s.defaultUser, s.execs.Environment(nil, s.defaultUser))
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer root.Close()
	if previous.Stage != "" {
		if err := recoverSkillPublication(root, journal, &previous); err != nil {
			return err
		}
	}
	if previous.Owner == s.managedSkillsOwner && previous.Revision >= desired.Revision {
		s.appliedManagedSkills.Store(previous.Revision)
		return nil
	}
	ownedPrevious := previous.Owner == s.managedSkillsOwner
	safeFallback := ownedPrevious
	defer func() {
		if retErr != nil && safeFallback {
			retErr = errors.Join(retErr, errSkillsRolledBack)
		}
	}()
	for _, name := range previous.Names {
		if err := sandboxconfig.ValidateSkillName(name); err != nil {
			return err
		}
	}
	staged, err := os.MkdirTemp(home, ".discobox-skills-stage-")
	if err != nil {
		return err
	}
	stage := filepath.Base(staged)
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.RemoveAll(stage)
		}
	}()
	stageRoot, err := root.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer stageRoot.Close()
	info, err := stageRoot.Stat(".")
	if err != nil {
		return err
	}
	if !isPrivateSkillsStage(info) {
		return errors.New("the managed skills staging directory is not owned by the agent")
	}
	uid, gid := userIDs(s.defaultUser)
	for _, directory := range skillDirectories {
		for name, skill := range desired.Skills {
			skillRoot := filepath.Join(directory, name)
			if err := stageSkillFile(stageRoot, filepath.Join(skillRoot, sandboxconfig.SkillFileName), []byte(skill.Skill), 0o644, uid, gid); err != nil {
				return err
			}
			for _, file := range skill.Files {
				mode := fs.FileMode(0o644)
				if file.Executable {
					mode = 0o755
				}
				if err := stageSkillFile(stageRoot, filepath.Join(skillRoot, filepath.FromSlash(file.Path)), file.Content, mode, uid, gid); err != nil {
					return err
				}
			}
		}
	}
	names := desired.Skills.Names()
	if err := fsdurable.SyncIn(stageRoot, "."); err != nil {
		return err
	}
	previous.Stage = stage
	move := func(from, to string) error {
		currentStage, err := root.Lstat(stage)
		if err != nil || !os.SameFile(info, currentStage) {
			return errors.New("the managed skills staging directory moved during publication")
		}
		if _, err := root.Lstat(to); err == nil {
			return fmt.Errorf("the managed skills destination %q is occupied", to)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if _, err := root.Lstat(from); err != nil {
			return err
		}
		previous.Moves = append(previous.Moves, skillRename{From: from, To: to})
		if err := saveSkillPublication(journal, previous); err != nil {
			previous.Moves = previous.Moves[:len(previous.Moves)-1]
			return err
		}
		return renameSkillEntry(root, stageRoot, stage, from, to)
	}
	publish := func() error {
		for _, directory := range skillDirectories {
			for _, path := range []string{filepath.Dir(directory), directory} {
				err := root.Mkdir(path, 0o755)
				if errors.Is(err, fs.ErrExist) {
					info, err := root.Lstat(path)
					if err != nil {
						return err
					}
					if !info.IsDir() {
						return fmt.Errorf("skill directory %s is not a directory", path)
					}
				} else if err != nil {
					return err
				} else if uid != nil && gid != nil {
					if err := root.Lchown(path, int(*uid), int(*gid)); err != nil {
						return err
					}
				}
			}
			backup := filepath.Join(directory, managedSkillBackups)
			if err := root.MkdirAll(backup, 0o700); err != nil {
				return err
			}
			if info, err := root.Lstat(backup); err != nil || !info.IsDir() {
				return fmt.Errorf("invalid managed skill backup directory %s", backup)
			}
			removed := filepath.Join(stage, "previous", directory)
			if err := root.MkdirAll(removed, 0o700); err != nil {
				return err
			}
			for _, name := range previous.Names {
				if err := move(filepath.Join(directory, name), filepath.Join(removed, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				if !slices.Contains(names, name) {
					if err := move(filepath.Join(backup, name), filepath.Join(directory, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				}
			}
			for _, name := range names {
				destination := filepath.Join(directory, name)
				if !slices.Contains(previous.Names, name) {
					if err := move(destination, filepath.Join(backup, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				}
				if err := move(filepath.Join(stage, directory, name), destination); err != nil {
					return err
				}
			}
		}
		return saveSkillPublication(journal, installedSkills{Owner: s.managedSkillsOwner, Revision: desired.Revision, Names: names, Stage: stage})
	}
	safeFallback = false
	if err := publish(); err != nil {
		cleanup = false
		if recovery := recoverSkillPublication(root, journal, &previous); recovery != nil {
			return errors.Join(err, recovery)
		}
		if ownedPrevious {
			return errors.Join(err, errSkillsRolledBack)
		}
		return err
	}
	if err := root.RemoveAll(stage); err != nil {
		cleanup = false
		return err
	}
	cleanup = false
	s.appliedManagedSkills.Store(desired.Revision)
	return nil
}

func saveSkillPublication(path string, state installedSkills) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return fsdurable.WriteFile(path, data, 0o600)
}

func renameSkillEntry(home, stageRoot *os.Root, stage, from, to string) error {
	rootFor := func(path string) (*os.Root, string) {
		if rel, ok := strings.CutPrefix(path, stage+string(filepath.Separator)); ok {
			return stageRoot, rel
		}
		return home, path
	}
	source, fromPath := rootFor(from)
	target, toPath := rootFor(to)
	if err := renameBetweenSkillsRoots(source, fromPath, target, toPath); err != nil {
		return err
	}
	return errors.Join(fsdurable.SyncIn(source, filepath.Dir(fromPath)), fsdurable.SyncIn(target, filepath.Dir(toPath)))
}

func recoverSkillPublication(root *os.Root, journal string, state *installedSkills) error {
	var stageRoot *os.Root
	if len(state.Moves) > 0 {
		opened, err := root.OpenRoot(state.Stage)
		if err != nil {
			return err
		}
		defer opened.Close()
		info, err := opened.Stat(".")
		if err != nil {
			return err
		}
		if !isPrivateSkillsStage(info) {
			return errors.New("cannot recover an untrusted skills staging directory")
		}
		stageRoot = opened
	}
	for len(state.Moves) > 0 {
		i := len(state.Moves) - 1
		move := state.Moves[i]
		_, source := root.Lstat(move.From)
		_, target := root.Lstat(move.To)
		switch {
		case source == nil && errors.Is(target, fs.ErrNotExist):
			// The move did not start, or its rollback already happened.
		case errors.Is(source, fs.ErrNotExist) && target == nil:
			if err := renameSkillEntry(root, stageRoot, state.Stage, move.To, move.From); err != nil {
				return err
			}
		default:
			return fmt.Errorf("cannot recover skill publication for %q", move.From)
		}
		state.Moves = state.Moves[:i]
		if err := saveSkillPublication(journal, *state); err != nil {
			return err
		}
	}
	if err := root.RemoveAll(state.Stage); err != nil {
		return err
	}
	state.Stage = ""
	return saveSkillPublication(journal, *state)
}

func stageSkillFile(root *os.Root, path string, data []byte, mode fs.FileMode, uid, gid *int64) error {
	parent := filepath.Dir(path)
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if uid == nil || gid == nil {
		return nil
	}
	for name := path; name != "."; name = filepath.Dir(name) {
		if err := root.Lchown(name, int(*uid), int(*gid)); err != nil {
			return err
		}
		if err := fsdurable.SyncIn(root, filepath.Dir(name)); err != nil {
			return err
		}
	}
	return nil
}
