package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// SystemPath is the PATH every user's session starts from. A command a
// feature adopts is found on it, not on any one user's.
var SystemPath = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// userHomes are where a user's own files live. A tool there, or reached
// through a link into there, is that user's and never adopted (ADR
// 26-10-16-423 §§4, 14): the account it belongs to may be deleted.
var userHomes = []string{"/home", "/root"}

// versionTimeout bounds a version command, which runs before anything is
// provisioned.
const versionTimeout = 10 * time.Second

var versionNumber = regexp.MustCompile(`[0-9]+(\.[0-9]+)+|[0-9]+`)

// usable reports whether the box's own copy of what the feature installs is
// usable, and the version its floor read; the error says why it is not.
func (r *Runner) usable(ctx context.Context, feature provisionspec.Feature) (string, error) {
	adopt := feature.Spec.Adopt
	for _, command := range adopt.Commands {
		if _, err := r.systemCommand(command); err != nil {
			return "", err
		}
	}
	for _, path := range adopt.Writable {
		if err := writable(r.path(path)); err != nil {
			return "", fmt.Errorf("%s is not writable: %w", path, err)
		}
	}
	if adopt.Version == nil {
		return "", nil
	}
	path, err := r.systemCommand(adopt.Version.Command)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	var out bytes.Buffer
	if err := r.command(ctx, &out, "/", nil, path, adopt.Version.VersionArgs()...); err != nil {
		return "", fmt.Errorf("%s: %w", adopt.Version.Command, err)
	}
	version := versionNumber.FindString(out.String())
	if version == "" {
		return "", fmt.Errorf("%s printed no version", adopt.Version.Command)
	}
	if compareVersions(version, adopt.Version.Minimum) < 0 {
		return version, fmt.Errorf("%s is %s, older than %s", adopt.Version.Command, version, adopt.Version.Minimum)
	}
	return version, nil
}

// systemCommand finds command on the system PATH, as every user reaches it,
// and is its path under the runner's root. A command that is the agent's own
// personality, or a placeholder, is not the box's copy.
func (r *Runner) systemCommand(command string) (string, error) {
	var why error
	for _, dir := range SystemPath {
		candidate := r.path(filepath.Join(dir, command))
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		if r.Self != "" && sameFile(resolved, r.Self) {
			continue
		}
		if r.Placeholders != "" && filepath.Dir(resolved) == r.path(r.Placeholders) {
			continue
		}
		if err := r.reachable(resolved); err != nil {
			why = fmt.Errorf("%s: %w", filepath.Join(dir, command), err)
			continue
		}
		return candidate, nil
	}
	if why != nil {
		return "", why
	}
	return "", fmt.Errorf("%s is not on the system PATH", command)
}

// reachable reports why every user cannot run the file at resolved, a path
// under the runner's root with no link left in it.
func (r *Runner) reachable(resolved string) error {
	root := filepath.Clean(r.root())
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside the box", resolved)
	}
	boxPath := "/" + filepath.ToSlash(rel)
	for _, home := range userHomes {
		if boxPath == home || strings.HasPrefix(boxPath, home+"/") {
			return fmt.Errorf("it is %s, in a user's home", boxPath)
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("%s cannot be run by every user", boxPath)
	}
	for dir := filepath.Dir(resolved); len(dir) > len(root); dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("%s is closed to other users", "/"+filepath.ToSlash(mustRel(root, dir)))
		}
	}
	return nil
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// writable reports why path cannot be written: a directory takes a new file,
// and a file opens for writing.
func writable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		return file.Close()
	}
	probe, err := os.CreateTemp(path, ".discobox-provision-probe-*")
	if err != nil {
		return err
	}
	probe.Close()
	if err := os.Remove(probe.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// compareVersions compares two dotted versions number by number; a missing
// number is 0.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
