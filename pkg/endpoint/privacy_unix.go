//go:build !windows

package endpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// protectDefaultSocket checks both sides of the default IPC boundary. Each
// component is opened without following links, beneath a directory whose
// ownership prevents another user from replacing it. /tmp's system alias on
// macOS is resolved separately, before any per-user path is considered.
func protectDefaultSocket(socket string) error {
	dirs, err := inspectRuntimeDirectories(socket, true, false)
	closeRuntimeDirectories(dirs)
	return err
}

// RuntimeDirectoryChange is one permission change requiring fresh human consent.
type RuntimeDirectoryChange struct {
	Path string
	Mode os.FileMode
}

type runtimeDirectory struct {
	path     string
	fd       int
	stat     unix.Stat_t
	required bool
	acl      string
}

// RuntimeDirectoryRepair retains descriptors for the inspected runtime tree.
// Close it after displaying Changes and, only with explicit consent, Repair.
type RuntimeDirectoryRepair struct {
	socket  string
	dirs    []runtimeDirectory
	changes []RuntimeDirectoryChange
}

// InspectRuntimeDirectories checks a default Unix endpoint without creating or
// modifying paths. Foreign ownership, symlinks, unsafe ancestors and ACLs fail
// closed, even when another component has repairable mode bits.
func InspectRuntimeDirectories(address string) (*RuntimeDirectoryRepair, error) {
	parsed, err := Parse(address)
	if err != nil {
		return nil, err
	}
	def, err := Parse(DefaultEndpoint())
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != def.Scheme || parsed.Value != def.Value {
		return nil, nil
	}
	dirs, err := inspectRuntimeDirectories(parsed.Value, false, true)
	if err != nil {
		closeRuntimeDirectories(dirs)
		return nil, err
	}
	plan := &RuntimeDirectoryRepair{socket: parsed.Value, dirs: dirs}
	for _, d := range dirs {
		if d.required && d.stat.Mode&0077 != 0 {
			plan.changes = append(plan.changes, RuntimeDirectoryChange{d.path, os.FileMode(d.stat.Mode & 0777)})
		}
	}
	if len(plan.changes) == 0 {
		plan.Close()
		return nil, nil
	}
	return plan, nil
}

func (p *RuntimeDirectoryRepair) Changes() []RuntimeDirectoryChange {
	return append([]RuntimeDirectoryChange(nil), p.changes...)
}

func closeRuntimeDirectories(dirs []runtimeDirectory) {
	for _, d := range dirs {
		_ = unix.Close(d.fd)
	}
}

func (p *RuntimeDirectoryRepair) Close() {
	closeRuntimeDirectories(p.dirs)
	p.dirs = nil
}

// Repair applies only the displayed changes to the retained objects. A fresh
// descriptor walk checks the complete path before the first chmod. Failures
// report completed changes; protected directories are never made public again.
func (p *RuntimeDirectoryRepair) Repair() error {
	if len(p.dirs) == 0 {
		return fmt.Errorf("runtime directory repair is closed")
	}
	fresh, err := inspectRuntimeDirectories(p.socket, false, true)
	defer closeRuntimeDirectories(fresh)
	if err != nil {
		return fmt.Errorf("runtime directory path changed: %w", err)
	}
	if len(fresh) != len(p.dirs) {
		return fmt.Errorf("runtime directory path changed")
	}
	for i, d := range fresh {
		old := p.dirs[i]
		if d.path != old.path || d.stat.Dev != old.stat.Dev || d.stat.Ino != old.stat.Ino || d.stat.Uid != old.stat.Uid || d.stat.Gid != old.stat.Gid || d.stat.Mode != old.stat.Mode || d.acl != old.acl {
			return fmt.Errorf("runtime directory path changed: %q", d.path)
		}
	}
	var changed []string
	for _, d := range p.dirs {
		if !d.required || d.stat.Mode&0077 == 0 {
			continue
		}
		if err := unix.Fchmod(d.fd, 0700); err != nil {
			return fmt.Errorf("repair runtime directory %q: %w; already changed to 0700: %q", d.path, err, changed)
		}
		changed = append(changed, d.path)
	}
	if err := protectDefaultSocket(p.socket); err != nil {
		return fmt.Errorf("revalidate repaired runtime directories: %w; already changed to 0700: %q", err, changed)
	}
	return nil
}

func inspectRuntimeDirectories(socket string, create, allowMode bool) (dirs []runtimeDirectory, result error) {
	parsed, err := Parse(DefaultEndpoint())
	if err != nil || socket != parsed.Value {
		return nil, err
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join("/tmp", "discobox-"+strconv.Itoa(os.Getuid()))
	}
	if !filepath.IsAbs(base) {
		return dirs, fmt.Errorf("unsafe runtime directory %q: must be absolute", base)
	}
	path := filepath.Join(base, "discobox")
	private := filepath.Clean(base)
	// These system-owned aliases are standard on macOS. Resolve only their
	// system-controlled prefix, never a link in the per-user runtime tree.
	for _, alias := range []string{"/tmp", "/var"} {
		if path == alias || strings.HasPrefix(path, alias+"/") {
			resolved, err := filepath.EvalSymlinks(alias)
			if err != nil {
				return dirs, err
			}
			path = resolved + strings.TrimPrefix(path, alias)
			private = resolved + strings.TrimPrefix(private, alias)
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return dirs, err
	}
	defer func() { _ = unix.Close(fd) }()
	var rootStat unix.Stat_t
	if err := unix.Fstat(fd, &rootStat); err != nil {
		return dirs, err
	}
	if rootStat.Uid != 0 || rootStat.Mode&0o022 != 0 {
		return dirs, fmt.Errorf("unsafe runtime directory ancestor /: untrusted ownership or permissions")
	}
	if err := checkDirectoryACL(fd); err != nil {
		return dirs, fmt.Errorf("unsafe runtime directory ancestor /: %w", err)
	}
	current := ""
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current += "/" + part
		required := current == private || strings.HasPrefix(current, private+"/")
		if required && create {
			if err := unix.Mkdirat(fd, part, 0o700); err != nil && err != unix.EEXIST {
				return dirs, fmt.Errorf("create runtime directory %q: %w", current, err)
			}
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && !create {
			return dirs, nil
		}
		if err != nil {
			return dirs, fmt.Errorf("unsafe runtime directory %q: %w", current, err)
		}
		var st unix.Stat_t
		err = unix.Fstat(next, &st)
		if err != nil {
			_ = unix.Close(next)
			return dirs, err
		}
		owner := st.Uid == uint32(os.Geteuid())
		safe := (owner || st.Uid == 0) && (st.Mode&0o022 == 0 || (st.Uid == 0 && st.Mode&unix.S_ISVTX != 0))
		if required {
			safe = owner && (allowMode || st.Mode&0o077 == 0)
		}
		if !safe {
			_ = unix.Close(next)
			return dirs, fmt.Errorf("unsafe runtime directory %q: must have trusted ownership and private permissions; rerun the CLI interactively or inspect and repair permissions manually", current)
		}
		if err := checkDirectoryACL(next); err != nil {
			_ = unix.Close(next)
			return dirs, fmt.Errorf("unsafe runtime directory %q: %w", current, err)
		}
		var acl string
		if allowMode && required {
			acl, err = repairDirectoryACL(next)
			if err != nil {
				_ = unix.Close(next)
				return dirs, fmt.Errorf("unsafe runtime directory %q: %w", current, err)
			}
		}
		saved, err := unix.Dup(next)
		if err != nil {
			_ = unix.Close(next)
			return dirs, err
		}
		unix.CloseOnExec(saved)
		dirs = append(dirs, runtimeDirectory{current, saved, st, required, acl})
		_ = unix.Close(fd)
		fd = next
	}
	return dirs, nil
}
