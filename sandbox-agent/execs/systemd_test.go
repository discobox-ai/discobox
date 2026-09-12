package execs

import (
	"slices"
	"strings"
	"testing"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// The unit runs as root and the shim drops to the run user itself, so the unit
// must carry no User=/Group= and the shim must be handed the user to become.
func TestUnitStartKeepsShimRootAndPassesUserToShim(t *testing.T) {
	uid := int64(1000)
	gid := int64(1001)
	req := StartRequest{
		ID:           "exec-test",
		Unit:         "discobox-exec-test",
		Command:      []string{"true"},
		Workdir:      "/workspace",
		User:         &User{Name: "darren", UID: &uid, GID: &gid},
		SocketPath:   "/run/discobox/agent-terminals/exec-test.sock",
		RuntimePath:  "/run/discobox/agent-terminals/exec-test.json",
		DatabasePath: "/var/lib/discobox/sandbox-agent.db",
	}
	argv, err := shimArgv("/usr/local/bin/discobox-sandbox-agent", req)
	if err != nil {
		t.Fatalf("shim argv: %v", err)
	}
	if !slices.Contains(argv, "exec-shim") || !slices.Contains(argv, "--user") {
		t.Fatalf("shim argv did not pass the user to the exec shim: %v", argv)
	}
	if i := slices.Index(argv, "--database"); i < 0 || argv[i+1] != req.DatabasePath {
		t.Fatalf("shim argv did not pass the database path to the exec shim: %v", argv)
	}

	props := unitProperties(req, argv)
	byName := map[string]sddbus.Property{}
	for _, prop := range props {
		byName[prop.Name] = prop
	}
	for _, forbidden := range []string{"User", "Group"} {
		if _, ok := byName[forbidden]; ok {
			t.Fatalf("unit properties set %s; the shim drops privileges itself", forbidden)
		}
	}
	if got := byName["KillMode"].Value.Value(); got != "control-group" {
		t.Errorf("KillMode = %v, want control-group", got)
	}
	// --collect: a transient unit that failed is unloaded rather than kept for a
	// reset-failed, which is what lets every run take a fresh unit generation.
	if got := byName["CollectMode"].Value.Value(); got != "inactive-or-failed" {
		t.Errorf("CollectMode = %v, want inactive-or-failed", got)
	}
	if got := byName["WorkingDirectory"].Value.Value(); got != "/workspace" {
		t.Errorf("WorkingDirectory = %v, want /workspace", got)
	}
	if _, ok := byName["ExecStart"]; !ok {
		t.Error("unit properties carry no ExecStart")
	}
}

func TestUnitEnvironmentIsSortedKeyValuePairs(t *testing.T) {
	env := unitEnvironment(map[string]string{"ZED": "1", "ALPHA": "2", "  ": "skipped"})
	want := []string{"ALPHA=2", "ZED=1"}
	if !slices.Equal(env, want) {
		t.Fatalf("environment = %v, want %v", env, want)
	}
}

// systemd answers for a unit it has never heard of with a full property set
// reporting it inactive — the same answer `systemctl show` gave. Only LoadState
// separates that from a unit that ran and stopped, so the parser must carry it
// through.
func TestUnitStatusFromPropertiesReportsNotFoundUnitUnloaded(t *testing.T) {
	missing := unitStatusFromProperties(map[string]any{
		"Id":          "discobox-exec-ex_gone.service",
		"LoadState":   "not-found",
		"ActiveState": "inactive",
	})
	if missing.Loaded {
		t.Fatalf("not-found unit reported loaded: %#v", missing)
	}
	stopped := unitStatusFromProperties(map[string]any{
		"Id":          "discobox-exec-ex_ran.service",
		"LoadState":   "loaded",
		"ActiveState": "inactive",
	})
	if !stopped.Loaded {
		t.Fatalf("loaded unit reported unloaded: %#v", stopped)
	}
	// An unexpected property set must not read as a vanished unit.
	if !unitStatusFromProperties(map[string]any{"Id": "x", "ActiveState": "active"}).Loaded {
		t.Fatal("unit without LoadState reported unloaded")
	}
}

// systemd types its properties narrowly over D-Bus: a PID is uint32, an exit
// status int32, a timestamp uint64 microseconds since the epoch.
func TestUnitStatusFromPropertiesReadsSystemdTypes(t *testing.T) {
	activeEnter := time.Date(2026, 9, 12, 3, 4, 5, 0, time.UTC)
	status := unitStatusFromProperties(map[string]any{
		"Id":                     "discobox-exec-ex_live.service",
		"LoadState":              "loaded",
		"ActiveState":            "active",
		"SubState":               "running",
		"MainPID":                uint32(498),
		"ExecMainStatus":         int32(7),
		"Result":                 "exit-code",
		"ActiveEnterTimestamp":   uint64(activeEnter.UnixMicro()),
		"InactiveEnterTimestamp": uint64(0),
	})
	// The stored unit name is bare: nextUnitGeneration parses it, and a stored
	// ".service" would make every relaunch collide on generation 2.
	if status.Unit != "discobox-exec-ex_live" {
		t.Errorf("unit = %q, want the bare name", status.Unit)
	}
	if status.PID != 498 {
		t.Errorf("pid = %d, want 498", status.PID)
	}
	if status.ExitCode == nil || *status.ExitCode != 7 {
		t.Errorf("exit code = %v, want 7", status.ExitCode)
	}
	if status.Error != "exit-code" {
		t.Errorf("error = %q, want exit-code", status.Error)
	}
	if status.StartedAt == nil || !status.StartedAt.Equal(activeEnter) {
		t.Errorf("started at = %v, want %v", status.StartedAt, activeEnter)
	}
	// A zero timestamp is a transition that never happened, not the epoch.
	if status.ExitedAt != nil {
		t.Errorf("exited at = %v, want none", status.ExitedAt)
	}
}

func TestUnitNameRoundTrip(t *testing.T) {
	if got := unitFullName("discobox-exec-ex_1"); got != "discobox-exec-ex_1.service" {
		t.Errorf("full name = %q", got)
	}
	// A name already carrying the suffix — as older records hold — must not
	// grow a second one.
	if got := unitFullName("discobox-exec-ex_1.service"); got != "discobox-exec-ex_1.service" {
		t.Errorf("full name doubled the suffix: %q", got)
	}
	if got := unitFullName("  "); got != "" {
		t.Errorf("blank unit = %q, want empty", got)
	}
	if got := unitBaseName("discobox-exec-ex_1.service"); got != "discobox-exec-ex_1" {
		t.Errorf("base name = %q", got)
	}
}

func TestExecIDFromUnit(t *testing.T) {
	for unit, want := range map[string]string{
		"discobox-exec-ex_1.service":    "ex_1",
		"discobox-exec-ex_1":            "ex_1",
		"discobox-exec-ex_1-g2.service": "ex_1",
		"discobox-exec-ex_1-g17":        "ex_1",
		// Not a generation suffix, so it is part of the id.
		"discobox-exec-ex_1-gx": "ex_1-gx",
		"sshd.service":          "",
	} {
		if got := execIDFromUnit(unit); got != want {
			t.Errorf("execIDFromUnit(%q) = %q, want %q", unit, got, want)
		}
	}
}

// Concluding a unit is gone demotes its exec to lost, and a lost terminal is
// relaunched over whatever is really running — so only systemd saying so in a
// typed error counts. Guessing from error text is good enough for Stop, where a
// false positive means reporting a no-op stop, and nowhere else.
func TestMissingUnitNeedsTheTypedErrorButStopWillGuess(t *testing.T) {
	text := errString("Unit discobox-exec-ex_1.service not loaded.")
	if isMissingUnitError(text) {
		t.Error("error text alone must not conclude a unit is gone")
	}
	if !isAlreadyStoppedError(text) {
		t.Error("stop should read not-loaded text as already stopped")
	}

	typed := godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}
	if !isMissingUnitError(typed) {
		t.Error("systemd's own NoSuchUnit must read as missing")
	}
	if !isAlreadyStoppedError(typed) {
		t.Error("stop must also accept NoSuchUnit")
	}

	if isMissingUnitError(errString("Access denied")) || isAlreadyStoppedError(errString("Access denied")) {
		t.Error("an unrelated failure must not read as a missing unit")
	}
	if isMissingUnitError(nil) || isAlreadyStoppedError(nil) {
		t.Error("no error is not a missing unit")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestIsExecUnit(t *testing.T) {
	if !isExecUnit("discobox-exec-ex_1.service") {
		t.Error("an exec unit was not recognized")
	}
	if isExecUnit("docker.service") || isExecUnit(strings.ToUpper("discobox-exec-ex_1")) {
		t.Error("a unit that is not an exec was recognized as one")
	}
}
