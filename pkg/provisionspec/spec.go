// Package provisionspec is the provision.yaml contract (ADR 26-10-16-423 §3):
// the typed, idempotent steps a bundle's feature declares, how a bundle tree
// lays its features out, and what makes a declaration valid. Bundle authors
// write it and the sandbox agent's runner (sandbox-agent/provision) applies
// it; it is here so anything in the repository can check a bundle without the
// agent.
package provisionspec

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the only provision.yaml version this runner reads.
const APIVersion = "discobox.ai/provision/v1"

// FileName is a feature's step file, in its own directory.
const FileName = "provision.yaml"

// Core is the platform's feature that is not optional (ADR 26-10-16-423
// §4): it blocks readiness, along with every feature it requires, and a
// repository's settings cannot turn it off. A bundle names it
// features/core.
const Core = "core"

// FeaturesDir is where a bundle tree holds its features, one directory each
// (ADR 26-10-16-423 §2).
const FeaturesDir = "features"

// Spec is one feature's provision.yaml.
type Spec struct {
	APIVersion string `yaml:"apiVersion"`
	// Requires names the features this one is applied after. A feature
	// required by one that is on is on too (ADR 26-10-16-423 §4).
	Requires []string `yaml:"requires,omitempty"`
	// Async provisions the feature in the background, after the box is
	// ready (ADR 26-10-16-423 §5). A feature that is not async blocks
	// readiness, whichever bundle it came in (ADR 26-10-24-953 §1), and is
	// never turned off; so does, whole, every feature it requires, whatever
	// that one says. The platform's optional features are async; a harness's
	// are not.
	Async bool `yaml:"async,omitempty"`
	// Adopt is what makes a copy the box already has usable, so the feature
	// adopts it instead of installing its own (§4). Unset, the feature is
	// always installed.
	Adopt *Adopt `yaml:"adopt,omitempty"`
	// Commands are the feature's commands that stand as placeholders on the
	// system PATH while it provisions in the background: run early, one says
	// it is waiting, waits, and runs the real command (§5).
	Commands []string `yaml:"commands,omitempty"`
	// Snapshot, set false, leaves the whole feature out of a snapshot (§8):
	// none of its steps is applied while one is taken, so each is applied on
	// the box itself, at every boot it is not already recorded. Unset is in.
	Snapshot *bool  `yaml:"snapshot,omitempty"`
	Steps    []Step `yaml:"steps"`
}

// InSnapshot reports whether a snapshot applies step of this feature: unless
// the feature or the step says snapshot: false, it does.
func (s Spec) InSnapshot(step Step) bool {
	return (s.Snapshot == nil || *s.Snapshot) && (step.Snapshot == nil || *step.Snapshot)
}

// Adopt is a feature's usability check. A box's own copy is usable when
// every command is on the system PATH and reachable by every user (no
// directory on the way in a user's home or closed to others), the version
// floor is met, and every writable path can be written. It is the agent's,
// not a script's, so a link into one user's nvm cannot pass it.
type Adopt struct {
	Commands []string `yaml:"commands"`
	Version  *Version `yaml:"version,omitempty"`
	Writable []string `yaml:"writable,omitempty"`
}

// Version is a version floor: Command run with Args prints a version, the
// first dotted number in its output, which must be at least Minimum.
type Version struct {
	Command string `yaml:"command"`
	// Args are what make Command print its version; unset is --version.
	Args    []string `yaml:"args,omitempty"`
	Minimum string   `yaml:"minimum"`
}

// VersionArgs is the arguments that print the command's version.
func (v Version) VersionArgs() []string {
	if len(v.Args) == 0 {
		return []string{"--version"}
	}
	return v.Args
}

// Step is one typed step. Exactly one kind is set; Name keys its record, so
// it is unique within its feature.
type Step struct {
	Name string `yaml:"name"`
	// When selects the boxes the step applies to; unset matches every box.
	When *Selector `yaml:"when,omitempty"`
	// Check names a script in the feature's directory that exits 0 when what
	// the step put in place is still there. It is run only for a step already
	// recorded, and a non-zero exit is drift, which applies the step again.
	Check string `yaml:"check,omitempty"`
	// Install marks a step that installs the feature's own copy, skipped
	// when the feature adopts the box's. Every other step is wiring, applied
	// either way. Only a feature with adopt has one.
	Install bool `yaml:"install,omitempty"`
	// Async, in a feature that blocks readiness, runs the step and every
	// step after it in the background, so the steps still run in the order
	// written. It is overridden in a feature a blocking one requires, which
	// blocks whole, and means nothing in one already in the background. A group or
	// env step is never async: it is the cheap wiring that goes first.
	Async bool `yaml:"async,omitempty"`
	// Apt says a run step uses apt or dpkg, so it takes the lock every
	// packages step takes, and no two of them run at once.
	Apt bool `yaml:"apt,omitempty"`
	// Snapshot, set false, leaves the step out of a snapshot (§8): what it
	// does is provisioned on each box, never baked into one shared by every
	// box of the same plan. Unset is in. It is not part of the step's
	// digest: it says when the step runs, not what it does.
	Snapshot *bool `yaml:"snapshot,omitempty" json:"-"`

	File     *FileStep     `yaml:"file,omitempty"`
	Systemd  *SystemdStep  `yaml:"systemd,omitempty"`
	Group    *GroupStep    `yaml:"group,omitempty"`
	Env      *EnvStep      `yaml:"env,omitempty"`
	Download *DownloadStep `yaml:"download,omitempty"`
	Packages *PackagesStep `yaml:"packages,omitempty"`
	// Run names a script in the feature's directory, run with the feature's
	// directory as its working directory.
	Run string `yaml:"run,omitempty"`
	// Inputs names the files and directories of the feature's that a run
	// step's script reads beside itself. The step's digest covers them as it
	// covers the script, so changing one applies the step again (§3).
	Inputs []string `yaml:"inputs,omitempty"`
}

// Selector selects boxes by GOOS, GOARCH and os-release. Each list that is
// set must match; Distro matches the box's os-release ID or any of its
// ID_LIKE.
type Selector struct {
	OS     []string `yaml:"os,omitempty"`
	Arch   []string `yaml:"arch,omitempty"`
	Distro []string `yaml:"distro,omitempty"`
}

// FileStep places a file of the feature's at an absolute path.
type FileStep struct {
	Path   string `yaml:"path"`
	Source string `yaml:"source"`
	// Mode is octal, as a string ("0755"); unset is 0644.
	Mode string `yaml:"mode,omitempty"`
	// Merge, when set, merges Source into a file already at Path rather than
	// replacing it, so a file the box came with keeps what it says that
	// Source does not (ADR 26-10-16-423 §9). Its only value is MergeJSON.
	Merge string `yaml:"merge,omitempty"`
	// Restart names the units that read the file only as they start: on a
	// booted box, each one running is restarted once the feature's steps
	// are applied, so it runs on what the step put there. A unit not running
	// reads it when it next starts.
	Restart []string `yaml:"restart,omitempty" json:",omitempty"`
}

// MergeJSON merges a JSON object as an RFC 7396 merge patch: objects merge
// member by member, any other value of the source's replaces the file's, and
// a null removes the member. A file not there yet is an empty object. A
// source retires a member by setting it to null: one simply dropped stays on
// every box merged before, which cannot tell it from the box's own.
const MergeJSON = "json"

// SystemdStep enables or masks units and writes a drop-in. Any combination
// may be set, and at least one is.
type SystemdStep struct {
	Enable []string     `yaml:"enable,omitempty"`
	Mask   []string     `yaml:"mask,omitempty"`
	DropIn *SystemdDrop `yaml:"dropIn,omitempty"`
}

// SystemdDrop is a drop-in for Unit, from a file of the feature's, written as
// /etc/systemd/system/<Unit>.d/<Name>. On a booted box Unit is restarted, if
// it is running, once the feature's steps are applied (Step.Restarts), since
// a running unit keeps the environment and settings it started with: a
// daemon the box started before the drop-in was placed would otherwise run
// without it until the box's next boot.
type SystemdDrop struct {
	Unit   string `yaml:"unit"`
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
}

// GroupStep makes a group, as a system group unless GID names its id. A group
// that is already there is left as it is.
type GroupStep struct {
	Name string `yaml:"name"`
	GID  *int   `yaml:"gid,omitempty"`
}

// EnvStep is environment every login shell gets, written to
// /etc/profile.d/zz-discobox-<feature>--<step>.sh: Vars exported, and Path
// prepended to PATH in order. It sorts after the box's own profile scripts,
// so Path lands ahead of every directory they put on PATH, /usr/local/bin's
// shims included. It reaches login shells only: an exec that is not one gets
// the manifest's env, which this step does not write.
type EnvStep struct {
	Vars map[string]string `yaml:"vars,omitempty"`
	Path []string          `yaml:"path,omitempty"`
}

// DownloadStep fetches one file per architecture and places it at Path once
// its sha256 matches.
type DownloadStep struct {
	Path string `yaml:"path"`
	// Mode is octal, as a string; unset is 0644.
	Mode string `yaml:"mode,omitempty"`
	// Arch maps a GOARCH to what to fetch on it. A box whose architecture is
	// not here fails the step: select the step with `when` instead.
	Arch map[string]Download `yaml:"arch"`
	// Unpack, when set, says what is fetched is a gzipped tar or a zip, told
	// apart by its first bytes: Path is then
	// the directory it is unpacked into, replaced whole once the sha256
	// matches, and Mode is unused.
	Unpack *Unpack `yaml:"unpack,omitempty"`
}

// Unpack is how a downloaded archive is unpacked.
type Unpack struct {
	// Strip is how many leading components of each entry's name are dropped,
	// as tar's --strip-components; an entry with no more than that is
	// skipped.
	Strip int `yaml:"strip,omitempty"`
	// Only, when set, names the entries unpacked, after Strip, each a file or
	// a directory with everything under it; the rest of the archive is
	// skipped.
	Only []string `yaml:"only,omitempty"`
}

// Download is one architecture's URL and the sha256 of what it serves.
type Download struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
}

// PackagesStep installs packages with apt, one list per distribution: a box
// installs the list of its own os-release ID, else that of the first of its
// ID_LIKE that has one. Debian's list is so every Debian derivative's, unless
// the derivative names its own. Only the Debian family is supported.
type PackagesStep struct {
	Debian []string `yaml:"debian,omitempty"`
	// Ubuntu replaces Debian's list on Ubuntu and what derives from it. Left
	// out of a step's digest when unset, so a Debian-only step's is the same
	// as before Ubuntu had a list.
	Ubuntu []string `yaml:"ubuntu,omitempty" json:",omitempty"`
}

// For is the list a box whose os-release says id and idLike installs, and
// false when no list names it.
func (p PackagesStep) For(id string, idLike []string) ([]string, bool) {
	lists := map[string][]string{"debian": p.Debian, "ubuntu": p.Ubuntu}
	for _, distro := range append([]string{id}, idLike...) {
		if list := lists[distro]; len(list) > 0 {
			return list, true
		}
	}
	return nil, false
}

// Restarts is the units the step configures that read it only as they
// start: a drop-in's unit, and the units a file names in Restart. On a
// booted box the runner restarts those running once the feature's steps are
// applied.
func (s Step) Restarts() []string {
	switch {
	case s.File != nil:
		return s.File.Restart
	case s.Systemd != nil && s.Systemd.DropIn != nil:
		return []string{s.Systemd.DropIn.Unit}
	}
	return nil
}

// Wiring reports whether the step is the cheap wiring every feature that is
// on applies first, before the account exists: a group, or env (§5).
func (s Step) Wiring() bool {
	return s.Group != nil || s.Env != nil
}

// Kind names the step's kind.
func (s Step) Kind() string {
	kinds := s.kinds()
	if len(kinds) == 1 {
		return kinds[0]
	}
	return ""
}

func (s Step) kinds() []string {
	var kinds []string
	if s.File != nil {
		kinds = append(kinds, "file")
	}
	if s.Systemd != nil {
		kinds = append(kinds, "systemd")
	}
	if s.Group != nil {
		kinds = append(kinds, "group")
	}
	if s.Env != nil {
		kinds = append(kinds, "env")
	}
	if s.Download != nil {
		kinds = append(kinds, "download")
	}
	if s.Packages != nil {
		kinds = append(kinds, "packages")
	}
	if s.Run != "" {
		kinds = append(kinds, "run")
	}
	return kinds
}

// Files is every file of the feature's the step names, which its digest
// covers.
func (s Step) Files() []string {
	var files []string
	if s.Check != "" {
		files = append(files, s.Check)
	}
	if s.Run != "" {
		files = append(files, s.Run)
	}
	if s.File != nil {
		files = append(files, s.File.Source)
	}
	if s.Systemd != nil && s.Systemd.DropIn != nil {
		files = append(files, s.Systemd.DropIn.Source)
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// Feature is one loaded feature: its name, its directory, and its spec.
type Feature struct {
	Name string
	Dir  string
	Spec Spec
}

var (
	// namePattern is a feature, step or group name: separators only between
	// letters and digits, so no name holds "--", which joins a feature's and
	// a step's name where both have to be told apart (an env step's script).
	namePattern    = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)
	digitsPattern  = regexp.MustCompile(`^[0-9]+$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	unitPattern    = regexp.MustCompile(`^[A-Za-z0-9:_.@-]+\.(service|socket|timer|path|mount|target|slice|scope)$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	modePattern    = regexp.MustCompile(`^0?[0-7]{3}$`)
	// commandPattern is a command's file name.
	commandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	// versionPattern is a dotted version floor.
	versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	// packagePattern is Debian policy's package name, with an optional
	// =version pin.
	packagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+(=[A-Za-z0-9.+~:-]+)?$`)
)

var (
	knownOS   = []string{"linux", "darwin", "windows"}
	knownArch = []string{"amd64", "arm64"}
)

// ValidName reports whether name is a feature's name.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

// Load reads the features of each tree's features directory. A feature named
// in two trees is refused: a layer adds features and never replaces another's
// (ADR 26-10-16-423 §2).
func Load(trees ...string) ([]Feature, error) {
	var features []Feature
	seen := map[string]string{}
	for _, tree := range trees {
		dir := filepath.Join(tree, FeaturesDir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read features of %s: %w", tree, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if other, ok := seen[name]; ok {
				return nil, fmt.Errorf("feature %q is in both %s and %s", name, other, tree)
			}
			seen[name] = tree
			feature, err := LoadFeature(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			features = append(features, feature)
		}
	}
	return features, nil
}

// LoadFeature reads and validates one feature directory's provision.yaml. A
// directory with none is a feature with no steps.
func LoadFeature(dir string) (Feature, error) {
	name := filepath.Base(dir)
	feature := Feature{Name: name, Dir: dir}
	if !namePattern.MatchString(name) {
		return feature, fmt.Errorf("feature %q: not a valid feature name", name)
	}
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		feature.Spec = Spec{APIVersion: APIVersion}
		return feature, nil
	}
	if err != nil {
		return feature, fmt.Errorf("feature %q: %w", name, err)
	}
	spec, err := Parse(data)
	if err != nil {
		return feature, fmt.Errorf("feature %q: %s: %w", name, FileName, err)
	}
	for _, file := range spec.files() {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(file)))
		if errors.Is(err, fs.ErrNotExist) {
			// The likeliest cause is a script written inline, which is never
			// allowed: say what a step names, not only that a stat failed.
			return feature, fmt.Errorf("feature %q: %q is not a file in the feature's directory (a step names a file, never a script inline)", name, file)
		}
		if err != nil {
			return feature, fmt.Errorf("feature %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return feature, fmt.Errorf("feature %q: %s is not a file", name, file)
		}
	}
	for _, step := range spec.Steps {
		for _, input := range step.Inputs {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(input))); err != nil {
				return feature, fmt.Errorf("feature %q: step %q: input %s: %w", name, step.Name, input, err)
			}
		}
	}
	feature.Spec = spec
	return feature, nil
}

func (s Spec) files() []string {
	var files []string
	for _, step := range s.Steps {
		files = append(files, step.Files()...)
	}
	return files
}

// Parse decodes and validates a provision.yaml. Unknown fields are refused,
// so a misspelled key fails rather than being dropped.
func Parse(data []byte) (Spec, error) {
	var spec Spec
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, err
	}
	return spec, spec.Validate()
}

// Validate checks everything that can be checked without the box.
func (s Spec) Validate() error {
	if s.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion %q is not %s", s.APIVersion, APIVersion)
	}
	for _, name := range s.Requires {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("requires: %q is not a valid feature name", name)
		}
	}
	for _, command := range s.Commands {
		if !commandPattern.MatchString(command) {
			return fmt.Errorf("commands: %q is not a command name", command)
		}
	}
	if s.Adopt != nil {
		if err := s.Adopt.validate(); err != nil {
			return fmt.Errorf("adopt: %w", err)
		}
	}
	names := map[string]bool{}
	for i, step := range s.Steps {
		if !namePattern.MatchString(step.Name) {
			return fmt.Errorf("step %d: name %q is not a valid step name", i+1, step.Name)
		}
		if names[step.Name] {
			return fmt.Errorf("step %q: the name is used twice", step.Name)
		}
		names[step.Name] = true
		if err := step.validate(); err != nil {
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
		if step.Install && s.Adopt == nil {
			return fmt.Errorf("step %q: install: the feature has no adopt check, so it is always installed", step.Name)
		}
	}
	return nil
}

func (a *Adopt) validate() error {
	if len(a.Commands) == 0 {
		return errors.New("names no command")
	}
	for _, command := range a.Commands {
		if !commandPattern.MatchString(command) {
			return fmt.Errorf("%q is not a command name", command)
		}
	}
	if v := a.Version; v != nil {
		if !commandPattern.MatchString(v.Command) {
			return fmt.Errorf("version: %q is not a command name", v.Command)
		}
		if !versionPattern.MatchString(v.Minimum) {
			return fmt.Errorf("version: minimum %q is not a dotted version", v.Minimum)
		}
	}
	for _, p := range a.Writable {
		if err := validAbsolute("writable", p); err != nil {
			return err
		}
	}
	return nil
}

func (s Step) validate() error {
	switch kinds := s.kinds(); len(kinds) {
	case 0:
		return errors.New("names no kind (file, systemd, group, env, download, packages or run)")
	case 1:
	default:
		return fmt.Errorf("names more than one kind: %s", strings.Join(kinds, ", "))
	}
	if s.Wiring() && (s.Install || s.Async) {
		return errors.New("a group or env step is wiring: never install, never async")
	}
	if s.Apt && s.Run == "" {
		return errors.New("apt: only a run step says it uses apt; a packages step always takes the lock")
	}
	if len(s.Inputs) > 0 && s.Run == "" {
		return errors.New("inputs: only a run step's script reads files beside it; every other kind names its own")
	}
	for _, input := range s.Inputs {
		if err := validRelative(input); err != nil {
			return fmt.Errorf("inputs: %w", err)
		}
	}
	if s.When != nil {
		if err := s.When.validate(); err != nil {
			return fmt.Errorf("when: %w", err)
		}
	}
	for _, file := range s.Files() {
		if err := validRelative(file); err != nil {
			return err
		}
	}
	switch {
	case s.File != nil:
		return s.File.validate()
	case s.Systemd != nil:
		return s.Systemd.validate()
	case s.Group != nil:
		return s.Group.validate()
	case s.Env != nil:
		return s.Env.validate()
	case s.Download != nil:
		return s.Download.validate()
	case s.Packages != nil:
		return s.Packages.validate()
	}
	return nil
}

func (s *Selector) validate() error {
	for _, os := range s.OS {
		if !slices.Contains(knownOS, os) {
			return fmt.Errorf("os %q is not one of %s", os, strings.Join(knownOS, ", "))
		}
	}
	for _, arch := range s.Arch {
		if !slices.Contains(knownArch, arch) {
			return fmt.Errorf("arch %q is not one of %s", arch, strings.Join(knownArch, ", "))
		}
	}
	for _, distro := range s.Distro {
		if !namePattern.MatchString(distro) {
			return fmt.Errorf("distro %q is not an os-release ID", distro)
		}
	}
	return nil
}

func (f *FileStep) validate() error {
	if err := validAbsolute("file: path", f.Path); err != nil {
		return err
	}
	if f.Source == "" {
		return errors.New("file: names no source")
	}
	if f.Merge != "" && f.Merge != MergeJSON {
		return fmt.Errorf("file: merge %q is not %q", f.Merge, MergeJSON)
	}
	for _, unit := range f.Restart {
		if !unitPattern.MatchString(unit) {
			return fmt.Errorf("file: restart: %q is not a unit name", unit)
		}
	}
	return validMode("file", f.Mode)
}

func (s *SystemdStep) validate() error {
	if len(s.Enable) == 0 && len(s.Mask) == 0 && s.DropIn == nil {
		return errors.New("systemd: names no unit to enable or mask and no drop-in")
	}
	for _, unit := range slices.Concat(s.Enable, s.Mask) {
		if !unitPattern.MatchString(unit) {
			return fmt.Errorf("systemd: %q is not a unit name", unit)
		}
	}
	if s.DropIn != nil {
		if !unitPattern.MatchString(s.DropIn.Unit) {
			return fmt.Errorf("systemd: dropIn: %q is not a unit name", s.DropIn.Unit)
		}
		if !namePattern.MatchString(s.DropIn.Name) || !strings.HasSuffix(s.DropIn.Name, ".conf") {
			return fmt.Errorf("systemd: dropIn: name %q is not a .conf file name", s.DropIn.Name)
		}
		if s.DropIn.Source == "" {
			return errors.New("systemd: dropIn: names no source")
		}
	}
	return nil
}

func (g *GroupStep) validate() error {
	// A name of digits alone would be read as a gid wherever a group entry is
	// resolved (runuser.LookupGroupID).
	if !namePattern.MatchString(g.Name) || digitsPattern.MatchString(g.Name) {
		return fmt.Errorf("group: %q is not a group name", g.Name)
	}
	if g.GID != nil && *g.GID <= 0 {
		return fmt.Errorf("group: gid %d is not a group id", *g.GID)
	}
	return nil
}

func (e *EnvStep) validate() error {
	if len(e.Vars) == 0 && len(e.Path) == 0 {
		return errors.New("env: sets no variable and no path")
	}
	for name := range e.Vars {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("env: %q is not a variable name", name)
		}
		if name == "PATH" {
			return errors.New("env: PATH is set with path, not vars")
		}
	}
	for _, dir := range e.Path {
		if err := validAbsolute("env: path", dir); err != nil {
			return err
		}
		if strings.Contains(dir, ":") {
			return fmt.Errorf("env: path %q holds a colon", dir)
		}
	}
	return nil
}

func (d *DownloadStep) validate() error {
	if err := validAbsolute("download: path", d.Path); err != nil {
		return err
	}
	if len(d.Arch) == 0 {
		return errors.New("download: names no architecture")
	}
	for arch, download := range d.Arch {
		if !slices.Contains(knownArch, arch) {
			return fmt.Errorf("download: arch %q is not one of %s", arch, strings.Join(knownArch, ", "))
		}
		if !strings.HasPrefix(download.URL, "https://") {
			return fmt.Errorf("download: %s: url %q is not https", arch, download.URL)
		}
		if !sha256Pattern.MatchString(download.SHA256) {
			return fmt.Errorf("download: %s: sha256 %q is not a lowercase hex sha256", arch, download.SHA256)
		}
	}
	if d.Unpack != nil {
		if d.Unpack.Strip < 0 {
			return fmt.Errorf("download: unpack: strip %d is negative", d.Unpack.Strip)
		}
		if d.Mode != "" {
			return errors.New("download: an unpacked archive keeps its own modes; mode is for a single file")
		}
		for _, only := range d.Unpack.Only {
			if err := validRelative(only); err != nil {
				return fmt.Errorf("download: unpack: only: %w", err)
			}
		}
	}
	return validMode("download", d.Mode)
}

func (p *PackagesStep) validate() error {
	if len(p.Debian) == 0 && len(p.Ubuntu) == 0 {
		return errors.New("packages: names no package")
	}
	for _, pkg := range slices.Concat(p.Debian, p.Ubuntu) {
		if !packagePattern.MatchString(pkg) {
			return fmt.Errorf("packages: %q is not a Debian package name", pkg)
		}
	}
	return nil
}

// validRelative is a file in the feature's own directory: relative, clean,
// and not climbing out of it.
func validRelative(file string) error {
	if file == "" || path.IsAbs(file) || path.Clean(file) != file || file == ".." || strings.HasPrefix(file, "../") || strings.Contains(file, "\\") {
		return fmt.Errorf("%q is not a file in the feature's directory", file)
	}
	return nil
}

func validAbsolute(what, p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("%s %q is not a clean absolute path", what, p)
	}
	return nil
}

func validMode(what, mode string) error {
	if mode != "" && !modePattern.MatchString(mode) {
		return fmt.Errorf("%s: mode %q is not octal", what, mode)
	}
	return nil
}
