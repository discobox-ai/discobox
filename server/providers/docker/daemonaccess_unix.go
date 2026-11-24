//go:build !windows

package docker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// probeSocketAccess reports whether the socket denies access and, when its
// metadata is readable, explains its group permissions. Moby drops the
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
	return true, describeSocketAccess(path, gid, holdsGroup(gid), info.Mode())
}

// describeSocketAccess explains the owning group and whether membership would
// help after a socket has refused the connection.
func describeSocketAccess(path string, gid int, member bool, mode fs.FileMode) string {
	name := groupName(gid)
	owner := strconv.Itoa(gid)
	if name != "" {
		owner = name + " (" + owner + ")"
	}
	groupAccess := fmt.Sprintf("%s is owned by group %s", path, owner)
	const groupReadWrite = 0o060
	groupCanReadWrite := mode&groupReadWrite == groupReadWrite
	if !groupCanReadWrite {
		groupAccess += fmt.Sprintf(" and socket mode %#04o does not grant the group read and write access", mode.Perm())
	}
	if gid == 0 {
		return groupAccess + ". Docker may have started without a docker group. Create it if missing with `sudo groupadd docker`. Ensure the docker group has a nonzero GID and configure Docker to give that group read and write access to its socket, then restart Docker. Add your user with `sudo usermod -aG docker $USER` and start a new login session. For a local Discobox server, run `discobox admin server shutdown --wait` from that session before retrying. On WSL, `wsl.exe --shutdown` also stops the server"
	}
	if !groupCanReadWrite {
		return groupAccess
	}
	if member {
		return groupAccess + ", which this process is already in"
	}
	if name == "" {
		name = strconv.Itoa(gid)
	}
	// A process's groups are fixed when it starts, and the CLI reuses a running
	// local server, so a new login session alone leaves the server that hit
	// this refusal running without the group (#1471).
	return fmt.Sprintf("%s and this process is not in it: add the user to that group with `sudo usermod -aG %s $USER` and start a new login session. For a local Discobox server, run `discobox admin server shutdown --wait` from that session before retrying. On WSL, `wsl.exe --shutdown` also stops the server", groupAccess, name)
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
