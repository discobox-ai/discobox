//go:build !windows

package endpoint

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

func acquireLaunchLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func startUserService(ctx context.Context, opts LaunchOptions, command Command, log io.Writer) (bool, error) {
	if !systemdUserManagerAvailable(ctx) {
		return false, nil
	}
	if missing := groupsAUserUnitWouldLose(ctx); len(missing) > 0 {
		// Declining the manager is otherwise indistinguishable from a machine
		// that has none: the server is simply not a unit here and is on the
		// next box over. The launch log is where an autolaunched server's
		// account of itself already goes, so the decision goes there too,
		// naming the groups that drove it.
		ids := make([]string, 0, len(missing))
		for _, gid := range missing {
			ids = append(ids, strconv.Itoa(gid))
		}
		fmt.Fprintf(log, "starting the server directly rather than as a systemd user unit: the user manager's units do not have group %s, which this process does\n", strings.Join(ids, " "))
		return false, nil
	}
	args := systemdRunArgs(opts, command)
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	_, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	return true, nil
}

func systemdUserManagerAvailable(ctx context.Context) bool {
	if _, lookupErr := exec.LookPath("systemd-run"); lookupErr != nil {
		return false
	}
	if _, lookupErr := exec.LookPath("systemctl"); lookupErr != nil {
		return false
	}
	return exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Run() == nil
}

// groupsAUserUnitWouldLose is every group this process holds that a unit
// started by the systemd user manager would not.
//
// A user unit is forked by user@<uid>.service, whose supplementary groups were
// resolved when that manager started and are never refreshed. Someone who joins
// a group afterwards — `usermod -aG docker`, the last step of every Docker
// install — has it in each new shell while the manager, and so every unit it
// starts, does not. The server launched that way then cannot open
// /var/run/docker.sock that the CLI which launched it can, and nothing says so:
// the two processes run as the same user.
//
// A user manager cannot be told to fix this. SupplementaryGroups= is a
// privileged unit setting, so the unit takes the groups it is given. The
// credentials are therefore what decides who starts the server: when systemd
// would drop one, the caller forks the server itself and it inherits the
// caller's.
//
// Asking the manager to run `id` is the only answer that is the unit's own —
// the manager's pid is not addressable and its /proc entry is not a contract.
// It is asked on the caller's deadline and no other: it starts a transient
// unit through the manager startUserService is about to hand the server to —
// and waits it out, which that launch does not — so a budget of its own could
// only decide that a manager too slow to run a unit should be handed the
// server anyway.
//
// Silence is not evidence. A probe that cannot run says nothing about groups,
// and it costs a launch that would have worked, so only a group set that is
// actually missing one of ours diverts the launch.
func groupsAUserUnitWouldLose(ctx context.Context) []int {
	idPath, err := exec.LookPath("id")
	if err != nil {
		return nil
	}
	// --wait --pipe to read the command's own output rather than start a unit
	// and leave, --collect so the transient unit is gone either way.
	out, err := exec.CommandContext(ctx, "systemd-run", "--user", "--quiet", "--collect", "--wait", "--pipe", "--", idPath, "-G").Output()
	if err != nil {
		return nil
	}
	unitGroups := parseGroupIDs(string(out))
	if len(unitGroups) == 0 {
		return nil
	}
	var missing []int
	for _, gid := range processGroupIDs() {
		if !slices.Contains(unitGroups, gid) {
			missing = append(missing, gid)
		}
	}
	return missing
}

// processGroupIDs is every group this process opens a file as: the
// supplementary list plus the effective primary group, which is the one a
// permission check consults and the one a shell entered with `newgrp docker`
// carries the new group in.
func processGroupIDs() []int {
	groups, err := os.Getgroups()
	if err != nil {
		return nil
	}
	return append(groups, os.Getegid())
}

// parseGroupIDs reads the space-separated list `id -G` prints. Output that is
// not that list yields nothing, which the caller reads as no evidence.
func parseGroupIDs(out string) []int {
	fields := strings.Fields(out)
	groups := make([]int, 0, len(fields))
	for _, field := range fields {
		gid, err := strconv.Atoi(field)
		if err != nil {
			return nil
		}
		groups = append(groups, gid)
	}
	return groups
}

func systemdRunArgs(opts LaunchOptions, command Command) []string {
	// The unit appends to the same file the directly-executed child writes, so
	// where a server's output lives does not depend on which of the two ways
	// this machine happened to start it. journald has the same lines, but only
	// for as long as it keeps them and only for someone who knows the unit name.
	args := []string{
		"--user",
		"--collect",
		"--unit=" + userServiceUnitName(opts, command),
		"--property=Description=Discobox local API server",
		"--property=StandardOutput=append:" + opts.logPath(),
		"--property=StandardError=append:" + opts.logPath(),
	}
	for _, entry := range opts.Env {
		args = append(args, "--setenv="+entry)
	}
	args = append(args, "--", command.Path)
	args = append(args, command.Args...)
	return args
}
