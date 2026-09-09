//go:build !windows

package endpoint

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestSystemdRunArgsStartsUserUnitWithEnvironment(t *testing.T) {
	opts := LaunchOptions{
		Endpoint: "unix:///tmp/discobox/server.sock",
		LogPath:  "/tmp/discobox-state/server.log",
		Env: []string{
			"DISCOBOX_SERVER=unix:///tmp/discobox/server.sock",
		},
	}

	args := systemdRunArgs(opts, Command{Path: "/usr/local/bin/discobox-server"})
	for _, want := range []string{
		"--user",
		"--collect",
		"--unit=discobox-server-30ad8514897f671d",
		"--property=Description=Discobox local API server",
		"--setenv=DISCOBOX_SERVER=unix:///tmp/discobox/server.sock",
		// The unit writes where a directly executed child writes, so reading a
		// server's output does not depend on how this machine started it.
		"--property=StandardOutput=append:/tmp/discobox-state/server.log",
		"--property=StandardError=append:/tmp/discobox-state/server.log",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("systemdRunArgs() missing %q", want)
		}
	}
	if got := args[len(args)-2:]; !slices.Equal(got, []string{"--", "/usr/local/bin/discobox-server"}) {
		t.Fatalf("systemdRunArgs() command tail = %#v", got)
	}
}

// The systemd user manager forks every unit it starts, so a unit's groups are
// the manager's, resolved when it started and never refreshed. A user who joins
// the docker group afterwards has it in their shell and not in the server the
// shell launches — same uid, no docker socket, no explanation. When systemd
// would drop a group this process holds, the caller starts the server itself.
func TestStartUserServiceDeclinesAManagerThatWouldDropAGroup(t *testing.T) {
	record := filepath.Join(t.TempDir(), "systemd-run.args")
	fakeSystemdPath(t, record, []int{gidNotHeld(t)}, 0)

	var log bytes.Buffer
	started, err := startUserService(context.Background(), launchOptionsForGroupProbe(t), probeCommand, &log)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("startUserService used a manager whose units lose one of this process's groups")
	}
	if data, err := os.ReadFile(record); err == nil && len(data) > 0 {
		t.Fatalf("a unit was started anyway: %s", data)
	}
	// Declining is otherwise indistinguishable from a machine with no systemd
	// at all, so the launch log says it happened and which group it was.
	if !strings.Contains(log.String(), "systemd user unit") {
		t.Fatalf("the launch log %q does not say the server was not started as a unit", log.String())
	}
	if !strings.Contains(log.String(), strconv.Itoa(processGroupIDs()[0])) {
		t.Fatalf("the launch log %q does not name the group the unit would lose", log.String())
	}
}

func TestStartUserServiceStartsTheUnitWhenItKeepsOurGroups(t *testing.T) {
	record := filepath.Join(t.TempDir(), "systemd-run.args")
	fakeSystemdPath(t, record, processGroupIDs(), 0)

	var log bytes.Buffer
	started, err := startUserService(context.Background(), launchOptionsForGroupProbe(t), probeCommand, &log)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("startUserService declined a manager that keeps this process's groups")
	}
	if log.Len() != 0 {
		t.Fatalf("a launch that used the manager wrote %q to the log", log.String())
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "--unit=") {
		t.Fatalf("systemd-run was not asked to start the unit: %q", data)
	}
}

// A probe that cannot answer is not evidence of anything, and refusing systemd
// on it would cost every launch that would have worked. The unit starts, and a
// launch that then fails falls back on its own.
func TestStartUserServiceStartsTheUnitWhenTheGroupsCannotBeRead(t *testing.T) {
	record := filepath.Join(t.TempDir(), "systemd-run.args")
	fakeSystemdPath(t, record, nil, 1)

	started, err := startUserService(context.Background(), launchOptionsForGroupProbe(t), probeCommand, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("startUserService declined a manager it could not read the groups of")
	}
}

func TestParseGroupIDsReadsOnlyAGroupList(t *testing.T) {
	if got := parseGroupIDs("1000 4 986\n"); !slices.Equal(got, []int{1000, 4, 986}) {
		t.Fatalf("parseGroupIDs() = %v", got)
	}
	if got := parseGroupIDs("Running as unit: run-u12.service\n"); got != nil {
		t.Fatalf("parseGroupIDs() = %v, want nothing for output that is not a group list", got)
	}
}

// probeCommand is the server these launches would start, which none of them
// gets as far as running.
var probeCommand = Command{Path: "/usr/local/bin/discobox-server"}

// launchOptionsForGroupProbe describes a launch whose only interesting part is
// which of the two ways of starting it is chosen.
func launchOptionsForGroupProbe(t *testing.T) LaunchOptions {
	t.Helper()
	return LaunchOptions{
		Endpoint: "unix://" + testSocketPath(t),
		LogPath:  filepath.Join(t.TempDir(), "server.log"),
	}
}

// fakeSystemdPath puts a systemd on PATH that answers the group probe with
// groups and probeStatus, and records what it was asked to start in record.
func fakeSystemdPath(t *testing.T, record string, groups []int, probeStatus int) {
	t.Helper()
	dir := t.TempDir()
	printed := make([]string, 0, len(groups))
	for _, gid := range groups {
		printed = append(printed, strconv.Itoa(gid))
	}
	write := func(name, script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write("systemctl", "#!/bin/sh\nexit 0\n")
	write("id", "#!/bin/sh\nexit 0\n") // only ever run by the real systemd-run
	write("systemd-run", `#!/bin/sh
for arg in "$@"; do
	if [ "$arg" = "-G" ]; then
		echo `+strconv.Quote(strings.Join(printed, " "))+`
		exit `+strconv.Itoa(probeStatus)+`
	fi
done
echo "$@" >> `+strconv.Quote(record)+`
exit 0
`)
	t.Setenv("PATH", dir)
}

// gidNotHeld is a group this process is not a member of, so a unit holding only
// it is a unit that lost every group this process has.
func gidNotHeld(t *testing.T) int {
	t.Helper()
	held := processGroupIDs()
	for gid := 65500; gid > 0; gid-- {
		if !slices.Contains(held, gid) {
			return gid
		}
	}
	t.Fatal("this process is a member of every group")
	return 0
}
