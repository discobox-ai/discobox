//go:build !windows

package docker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// probeSocketAccess reports whether the socket denies access and, when its
// metadata is readable, explains its group permissions. On Linux it also
// diagnoses absent sockets and stopped daemons. Moby drops the
// underlying permission error, so a direct dial recovers that information.
func probeSocketAccess(ctx context.Context, path string) (denied bool, hint string) {
	if path == "" {
		return false, ""
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err == nil {
		_ = conn.Close()
		return false, ""
	}
	if runtime.GOOS == "linux" && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)) {
		return false, dockerSocketUnavailableHint(ctx, path, errors.Is(err, fs.ErrNotExist))
	}
	if !errors.Is(err, fs.ErrPermission) {
		return false, ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return true, ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true, ""
	}
	gid := int(stat.Gid)
	return true, describeSocketAccess(path, gid, holdsGroup(gid))
}

// describeSocketAccess is what the reader is told about a socket they were
// refused: the group that owns it, and the command that would add them to it.
//
// A member who is refused anyway is told that instead. Whatever is denying
// them is not the membership, and sending them to add a group they already
// have would send them in a circle.
func describeSocketAccess(path string, gid int, member bool) string {
	name := groupName(gid)
	owner := strconv.Itoa(gid)
	if name != "" {
		owner = name + " (" + owner + ")"
	}
	if member {
		return fmt.Sprintf("%s is owned by group %s, which this process is already in", path, owner)
	}
	if name == "" {
		name = strconv.Itoa(gid)
	}
	return fmt.Sprintf("%s is owned by group %s and this process is not in it: add the user to that group with `sudo usermod -aG %s $USER`, then start a new login session — on WSL, `wsl.exe --shutdown`", path, owner, name)
}

// groupName is the name to type into usermod, or "" for a gid that has none.
func groupName(gid int) string {
	group, err := user.LookupGroupId(strconv.Itoa(gid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(group.Name)
}

// holdsGroup reports whether this process is a member of gid: the supplementary
// groups plus the effective primary one, which is the set a permission check on
// the socket consults.
func holdsGroup(gid int) bool {
	if os.Getegid() == gid {
		return true
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	return slices.Contains(groups, gid)
}

func dockerSocketUnavailableHint(ctx context.Context, path string, missing bool) string {
	var dialer net.Dialer
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	rootless := filepath.Join(runtimeDir, "docker.sock")
	if path != rootless {
		if conn, err := dialer.DialContext(ctx, "unix", rootless); err == nil {
			_ = conn.Close()
			return fmt.Sprintf("rootless Docker is listening at %s; set DOCKER_HOST=unix://%s in the server environment and restart the server; if the Docker provider sets bindDockerSocket explicitly, update it to the rootless socket too", rootless, rootless)
		}
	}
	_, cliErr := exec.LookPath("docker")
	_, daemonErr := exec.LookPath("dockerd")
	if cliErr != nil && daemonErr != nil && missing {
		return "Docker is not installed; install Docker Engine and start it with `sudo systemctl start docker`"
	}
	if path != dockerSocketPath {
		return fmt.Sprintf("no Docker daemon is listening at %s; start the daemon for that socket or correct DOCKER_HOST (for rootless Docker, use `systemctl --user start docker`)", path)
	}
	return "the Docker daemon is stopped or its socket is unavailable; start it with `sudo systemctl start docker`"
}
