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
