//go:build !windows

package endpoint

import (
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
	parsed, err := Parse(DefaultEndpoint())
	if err != nil || socket != parsed.Value {
		return err
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join("/tmp", "discobox-"+strconv.Itoa(os.Getuid()))
	}
	if !filepath.IsAbs(base) {
		return fmt.Errorf("unsafe runtime directory %q: must be absolute", base)
	}
	path := filepath.Join(base, "discobox")
	private := filepath.Clean(base)
	// These system-owned aliases are standard on macOS. Resolve only their
	// system-controlled prefix, never a link in the per-user runtime tree.
	for _, alias := range []string{"/tmp", "/var"} {
		if path == alias || strings.HasPrefix(path, alias+"/") {
			resolved, err := filepath.EvalSymlinks(alias)
			if err != nil {
				return err
			}
			path = resolved + strings.TrimPrefix(path, alias)
			private = resolved + strings.TrimPrefix(private, alias)
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var rootStat unix.Stat_t
	if err := unix.Fstat(fd, &rootStat); err != nil {
		return err
	}
	if rootStat.Uid != 0 || rootStat.Mode&0o022 != 0 {
		return fmt.Errorf("unsafe runtime directory ancestor /: untrusted ownership or permissions")
	}
	if err := checkDirectoryACL(fd); err != nil {
		return fmt.Errorf("unsafe runtime directory ancestor /: %w", err)
	}
	current := ""
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current += "/" + part
		required := current == private || strings.HasPrefix(current, private+"/")
		if required {
			if err := unix.Mkdirat(fd, part, 0o700); err != nil && err != unix.EEXIST {
				return fmt.Errorf("create runtime directory %q: %w", current, err)
			}
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("unsafe runtime directory %q: %w", current, err)
		}
		var st unix.Stat_t
		err = unix.Fstat(next, &st)
		if err != nil {
			_ = unix.Close(next)
			return err
		}
		owner := st.Uid == uint32(os.Geteuid())
		safe := (owner || st.Uid == 0) && (st.Mode&0o022 == 0 || (st.Uid == 0 && st.Mode&unix.S_ISVTX != 0))
		if required {
			safe = owner && st.Mode&0o077 == 0
		}
		if !safe {
			_ = unix.Close(next)
			return fmt.Errorf("unsafe runtime directory %q: must have trusted ownership and private permissions", current)
		}
		if err := checkDirectoryACL(next); err != nil {
			_ = unix.Close(next)
			return fmt.Errorf("unsafe runtime directory %q: %w", current, err)
		}
		_ = unix.Close(fd)
		fd = next
	}
	return nil
}
