package sandboxconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// ManagedSkills is a complete desired revision. Its sequence belongs to the
// control plane and survives a pool move or a restart of either agent.
type ManagedSkills struct {
	Revision int64  `json:"revision"`
	Skills   Skills `json:"skills,omitempty"`
}

func (m ManagedSkills) Validate() error {
	if m.Revision < 1 {
		return errors.New("managed skills revision must be positive")
	}
	return m.Skills.Validate()
}

// Digest identifies a skill's content independently of the order in which
// the publisher found its helper files. Callers validate before digesting.
func (s Skill) Digest() (string, error) {
	s.Files = slices.Clone(s.Files)
	slices.SortFunc(s.Files, func(a, b SkillFile) int {
		return strings.Compare(a.Path, b.Path)
	})
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode skill: %w", err)
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// SkillOrigin contains the publisher's source identifier and optional
// resolved Git commit. It travels with inventory, never with credentials.
type SkillOrigin struct {
	SourceID string `json:"sourceId"`
	Commit   string `json:"commit,omitempty"`
}

// SkillVersion is the inventory and audit representation of a managed skill.
type SkillVersion struct {
	SkillOrigin
	Digest string `json:"digest"`
}

// ManagedSkillSet is the control plane's durable inventory and content.
// Sources are the IDs the manager has elected to follow, including sources
// whose latest publication contains no skills.
type ManagedSkillSet struct {
	ManagedSkills
	Versions map[string]SkillVersion `json:"versions,omitempty"`
	Sources  map[string]SkillOrigin  `json:"sources,omitempty"`
}

// SkillMutation describes one atomic edit to a set. Sync compares the
// revision the publisher read before fetching its source.
type SkillMutation struct {
	Operation        string
	ExpectedRevision *int64
	Origin           SkillOrigin
	Skills           Skills
	Name             string
}

// ErrSkillRevisionChanged means a publisher must read the latest inventory
// before attempting to publish its source again.
var ErrSkillRevisionChanged = errors.New("the managed skills revision changed")

// ErrInvalidSkillMutation marks a rejected request.
var ErrInvalidSkillMutation = errors.New("invalid skills mutation")

func invalidSkillMutation(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSkillMutation, fmt.Sprintf(format, args...))
}

// Mutate returns the complete resulting set without changing the original.
// An equivalent publication keeps the previous revision.
func (m ManagedSkillSet) Mutate(change SkillMutation) (ManagedSkillSet, error) {
	if change.ExpectedRevision != nil && *change.ExpectedRevision != m.Revision {
		return m, ErrSkillRevisionChanged
	}
	if err := change.Skills.Validate(); err != nil {
		return m, fmt.Errorf("%w: %w", ErrInvalidSkillMutation, err)
	}
	next := ManagedSkillSet{
		ManagedSkills: ManagedSkills{Revision: m.Revision, Skills: make(Skills)},
		Versions:      make(map[string]SkillVersion),
		Sources:       make(map[string]SkillOrigin),
	}
	maps.Copy(next.Skills, m.Skills)
	maps.Copy(next.Versions, m.Versions)
	maps.Copy(next.Sources, m.Sources)
	switch change.Operation {
	case "set":
		clear(next.Skills)
		clear(next.Versions)
		clear(next.Sources)
	case "add":
	case "rm":
		if len(change.Skills) > 0 {
			return m, invalidSkillMutation("removing a skill takes a name only")
		}
		if err := ValidateSkillName(change.Name); err != nil {
			return m, fmt.Errorf("%w: %w", ErrInvalidSkillMutation, err)
		}
		delete(next.Skills, change.Name)
		delete(next.Versions, change.Name)
	case "follow", "sync":
		if change.ExpectedRevision == nil {
			return m, invalidSkillMutation("publishing a followed source requires the current revision")
		}
		if change.Operation == "sync" {
			if _, ok := m.Sources[change.Origin.SourceID]; !ok {
				return m, invalidSkillMutation("the box does not follow this skills source")
			}
		}
		for name, version := range next.Versions {
			if version.SourceID == change.Origin.SourceID {
				delete(next.Skills, name)
				delete(next.Versions, name)
			}
		}
		next.Sources[change.Origin.SourceID] = change.Origin
	default:
		return m, invalidSkillMutation("unknown skills operation %q", change.Operation)
	}
	if change.Operation != "rm" {
		if err := change.Origin.validate(); err != nil {
			return m, fmt.Errorf("%w: %w", ErrInvalidSkillMutation, err)
		}
	}
	for name, skill := range change.Skills {
		digest, err := skill.Digest()
		if err != nil {
			return m, fmt.Errorf("%w: %w", ErrInvalidSkillMutation, err)
		}
		next.Skills[name] = skill
		next.Versions[name] = SkillVersion{SkillOrigin: change.Origin, Digest: digest}
	}
	if len(next.Sources) > MaxSkillFiles {
		return m, invalidSkillMutation("a box can follow at most %d skills sources", MaxSkillFiles)
	}
	if maps.Equal(next.Versions, m.Versions) && maps.Equal(next.Sources, m.Sources) {
		return m, nil
	}
	if err := next.Skills.Validate(); err != nil {
		return m, fmt.Errorf("%w: %w", ErrInvalidSkillMutation, err)
	}
	next.Revision++
	return next, nil
}

// The identifier is an opaque publisher-chosen name. Restricting it to
// simple characters keeps URLs and absolute paths out of audit records.
func (o SkillOrigin) validate() error {
	if len(o.SourceID) > 128 || !skillSourceID.MatchString(o.SourceID) {
		return errors.New("skills source ID must contain only letters, digits, dots, underscores or hyphens")
	}
	if o.Commit != "" {
		if (len(o.Commit) != 40 && len(o.Commit) != 64) || strings.ToLower(o.Commit) != o.Commit {
			return errors.New("skills source commit must be a full Git object ID")
		}
		if _, err := hex.DecodeString(o.Commit); err != nil {
			return errors.New("skills source commit must be a full Git object ID")
		}
	}
	return nil
}

var skillSourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
