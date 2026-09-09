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

// socketAccessHint says why this process cannot open path, when the reason is
// one the machine can state.
//
// Nothing is claimed unless the socket refuses this process right now. A socket
// that dials — or is missing, or fails for any other reason — is not what this
// explains, and a guess about permissions on a daemon that is simply not
// running would send the reader after the wrong thing.
func socketAccessHint(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err == nil {
		_ = conn.Close()
		return ""
	}
	if !errors.Is(err, fs.ErrPermission) {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	gid := int(stat.Gid)
	return describeSocketAccess(path, gid, holdsGroup(gid))
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
