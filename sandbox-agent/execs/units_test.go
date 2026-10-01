package execs

import (
	"reflect"
	"testing"
)

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

// The shim reads back exactly what a unit manager wrote for it: shimArgv and
// ParseShimArgs are the two ends of one contract, whichever unit manager
// starts the shim.
func TestShimArgsRoundTrip(t *testing.T) {
	uid, gid := int64(1000), int64(1001)
	req := StartRequest{
		ID:             "ex_1",
		Unit:           "discobox-exec-ex_1-g2",
		Command:        []string{"sh", "-c", "echo 'it''s'"},
		StartupCommand: []string{"claude", "--resume"},
		Workdir:        "/workspace",
		Env:            map[string]string{"A": "1"},
		User:           &User{Name: "sandbox", UID: &uid, GID: &gid},
		TTY:            true,
		Metadata:       map[string]string{"harnessId": "shell"},
		SocketPath:     "/run/discobox/execs/ex_1.sock",
		RuntimePath:    "/run/discobox/execs/ex_1.json",
		DatabasePath:   "/var/lib/discobox/agent.db",
		Rows:           40,
		Cols:           120,
	}
	argv, err := shimArgv("/bin/agent", req)
	if err != nil {
		t.Fatalf("shim argv: %v", err)
	}
	if argv[0] != "/bin/agent" || argv[1] != "exec-shim" {
		t.Fatalf("argv = %v, want this binary as the exec shim", argv[:2])
	}
	parsed, err := ParseShimArgs(append(argv[2:], "--lifetime", "3"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := ShimConfig{
		ExecID:         req.ID,
		Unit:           req.Unit,
		Command:        req.Command,
		StartupCommand: req.StartupCommand,
		Workdir:        req.Workdir,
		SocketPath:     req.SocketPath,
		RuntimePath:    req.RuntimePath,
		Rows:           req.Rows,
		Cols:           req.Cols,
		TTY:            true,
		Env:            req.Env,
		User:           req.User,
		Metadata:       req.Metadata,
	}
	if !reflect.DeepEqual(parsed.Config, want) {
		t.Errorf("config = %+v, want %+v", parsed.Config, want)
	}
	if parsed.DatabasePath != req.DatabasePath {
		t.Errorf("database = %q, want %q", parsed.DatabasePath, req.DatabasePath)
	}
	if parsed.Lifetime == nil || *parsed.Lifetime != 3 {
		t.Errorf("lifetime = %v, want descriptor 3", parsed.Lifetime)
	}

	withoutLifetime, err := ParseShimArgs(argv[2:])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if withoutLifetime.Lifetime != nil {
		t.Errorf("lifetime = %v, want none when no supervisor passed one", *withoutLifetime.Lifetime)
	}
}

func TestShimArgsRequireACommand(t *testing.T) {
	if _, err := ParseShimArgs([]string{"--exec-id", "ex_1"}); err == nil {
		t.Fatal("a shim with no command parsed")
	}
}
