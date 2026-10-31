package monitors

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Dirs is where a box's monitors and events live. Every path is under the run
// user's home: an instance and its events outlive a stop and start of the box
// (ADR 26-10-31-073 §5), and the CLI, the runner and the user's own `ls` all
// read them as that user.
type Dirs struct {
	// Enabled holds one file per enabled instance; the files are the whole of
	// what is on.
	Enabled string
	// State holds a directory per instance: its status, its stderr log, and
	// the monitor's own state directory.
	State string
	// Pending holds the events no pop has returned yet.
	Pending string
	// Read holds the events a pop returned, so `read` still finds them.
	Read string
}

// HomeDirs is Dirs under home.
func HomeDirs(home string) Dirs {
	return Dirs{
		Enabled: filepath.Join(home, ".discobox", "monitors", "enabled"),
		State:   filepath.Join(home, ".discobox", "monitors", "state"),
		Pending: filepath.Join(home, ".discobox", "events", "pending"),
		Read:    filepath.Join(home, ".discobox", "events", "read"),
	}
}

// InstanceStateDir is the directory the runner keeps an instance's status and
// log in.
func (d Dirs) InstanceStateDir(name string) string { return filepath.Join(d.State, name) }

// MonitorStateDir is the directory the monitor itself owns, its
// DISCOBOX_MONITOR_STATE. It is the instance's own by ID, not only by name:
// a runner of the instance the name had before, still finishing its run,
// writes its baseline where the new one never reads it.
func (d Dirs) MonitorStateDir(name, id string) string {
	return filepath.Join(d.State, name, "data-"+id)
}

func (d Dirs) enabledFile(name string) string { return filepath.Join(d.Enabled, name+".json") }

// Queue is the event queue in these directories.
func (d Dirs) Queue() *Queue { return &Queue{Pending: d.Pending, Read: d.Read} }

// instanceName is what an instance may be called: a filename and an exec's
// metadata, typed back by an agent.
var instanceName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidName reports why name cannot name an instance, or nil.
func ValidName(name string) error {
	if !instanceName.MatchString(name) {
		return fmt.Errorf("instance name %q must be 1-64 lowercase letters, digits, '.', '-' and '_', starting with a letter or digit", name)
	}
	return nil
}

// Instance is an enabled monitor: the file in Dirs.Enabled.
type Instance struct {
	// ID is new on every enable. The name is what a person types and can be
	// enabled again with other options the moment it is disabled; the ID is
	// what tells the reconciler and the runner that the file they see is not
	// the instance they started.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Monitor string `json:"monitor"`
	// Path is the declaration the instance runs, resolved when it was enabled,
	// so a runner needs no discovery of its own.
	Path    string `json:"path"`
	Builtin bool   `json:"builtin,omitempty"`
	// Workdir is where the monitor runs: the repository root for a repository
	// monitor, the home directory for the image's.
	Workdir   string            `json:"workdir"`
	Options   map[string]string `json:"options,omitempty"`
	EnabledAt time.Time         `json:"enabledAt"`
}

// ErrNameTaken is an enable under a name another instance already holds with
// different options.
var ErrNameTaken = errors.New("instance name taken")

// ErrNotEnabled is a disable or lookup of a name nothing is enabled under.
var ErrNotEnabled = errors.New("no instance enabled under that name")

// Enable puts the instance's file in place. It reports false, and changes
// nothing, when the same monitor is already enabled under the name with the
// same options; with different ones it is ErrNameTaken, so an instance is
// never replaced by accident.
//
// A new instance starts from an empty state directory, so it reports only what
// happens after it is enabled (ADR 26-10-31-073 §3) rather than comparing
// against whatever a previous instance of the name last saw.
func Enable(d Dirs, inst Instance) (bool, error) {
	if err := ValidName(inst.Name); err != nil {
		return false, err
	}
	existing, err := ReadInstance(d, inst.Name)
	switch {
	case err == nil:
		if existing.Monitor == inst.Monitor && maps.Equal(existing.Options, inst.Options) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %q is %s", ErrNameTaken, inst.Name, describe(existing))
	case !errors.Is(err, ErrNotEnabled):
		return false, err
	}
	if err := os.RemoveAll(d.InstanceStateDir(inst.Name)); err != nil {
		return false, err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return false, err
	}
	inst.ID = hex.EncodeToString(id)
	if err := writeJSONFile(d.enabledFile(inst.Name), inst); err != nil {
		return false, err
	}
	return true, nil
}

// Disable removes the instance's file, and with it the instance: the agent
// stops its runner. Its state directory goes too, so a later instance of the
// name starts from nothing.
func Disable(d Dirs, name string) error {
	if err := os.Remove(d.enabledFile(name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A finished instance has no file left, only its state; disabling
			// it is how that is cleared.
			if _, statErr := os.Stat(d.InstanceStateDir(name)); statErr == nil {
				return os.RemoveAll(d.InstanceStateDir(name))
			}
			return fmt.Errorf("%w: %q", ErrNotEnabled, name)
		}
		return err
	}
	return os.RemoveAll(d.InstanceStateDir(name))
}

// finish removes the instance's file and keeps its state, which is what a
// monitor that is done leaves: status still says how it ended.
func finish(d Dirs, name string) error {
	if err := os.Remove(d.enabledFile(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ReadInstance is the instance enabled under name.
func ReadInstance(d Dirs, name string) (Instance, error) {
	data, err := os.ReadFile(d.enabledFile(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Instance{}, fmt.Errorf("%w: %q", ErrNotEnabled, name)
		}
		return Instance{}, err
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return Instance{}, fmt.Errorf("%s: %w", d.enabledFile(name), err)
	}
	// The file's name is the instance's name, whatever the file says: it is
	// what disable removes.
	inst.Name = name
	return inst, nil
}

// Enabled is every enabled instance, by name, and the files that could not be
// read as one.
func Enabled(d Dirs) ([]Instance, []error) {
	entries, err := os.ReadDir(d.Enabled)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{err}
	}
	var out []Instance
	var problems []error
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || strings.HasPrefix(file, ".") || !strings.HasSuffix(file, ".json") {
			continue
		}
		name := strings.TrimSuffix(file, ".json")
		if err := ValidName(name); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", file, err))
			continue
		}
		inst, err := ReadInstance(d, name)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, problems
}

func describe(inst Instance) string {
	if len(inst.Options) == 0 {
		return inst.Monitor
	}
	keys := make([]string, 0, len(inst.Options))
	for k := range inst.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+inst.Options[k])
	}
	return inst.Monitor + " " + strings.Join(parts, ",")
}

// Describe is the instance as `<monitor> k=v,...`, for a listing.
func (inst Instance) Describe() string { return describe(inst) }

// writeJSONFile writes v whole: under a dot name beside path, then renamed
// over it, so a reader never sees half a file.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
