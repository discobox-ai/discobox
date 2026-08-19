//go:build linux

package boot

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// bindMount binds source onto target. When readOnly is set the bind is
// remounted read-only (a bind's read-only flag only takes effect on a second
// MS_REMOUNT call).
func bindMount(source, target string, readOnly bool) error {
	if err := syscall.Mount(source, target, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s -> %s: %w", source, target, err)
	}
	if readOnly {
		if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount read-only %s: %w", target, err)
		}
	}
	return nil
}

// recursiveBindMount binds source onto target carrying nested submounts (the
// config volume nests the proxy material), then remounts the top read-only.
func recursiveBindMount(source, target string, readOnly bool) error {
	if err := syscall.Mount(source, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("recursive bind %s -> %s: %w", source, target, err)
	}
	if readOnly {
		if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount read-only %s: %w", target, err)
		}
	}
	return nil
}

// overlayMount stacks upper/work over lower and mounts the merged result onto
// target (which is also lower). Image content shows through; writes persist to
// upper on the backing volume.
func overlayMount(target, lower, upper, work string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := syscall.Mount("overlay", target, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlay mount %s (%s): %w", target, opts, err)
	}
	return nil
}

// execInit replaces this process with the container's real init, keeping PID 1.
func execInit(argv, env []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("exec init: empty argv")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("resolve init target %q: %w", argv[0], err)
	}
	return syscall.Exec(path, argv, env)
}

// fileDevice reports the number of the device fi lives on, which is what
// distinguishes one mounted filesystem from another. Used to keep a recursive
// walk from descending into a mounted volume.
func fileDevice(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Dev, true
}
