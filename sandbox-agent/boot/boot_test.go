package boot

import (
	"slices"
	"testing"
)

func TestResolveIdentityDefaultsToRoot(t *testing.T) {
	t.Setenv("DISCOBOX_USER_UID", "")
	t.Setenv("DISCOBOX_USER_GID", "")
	t.Setenv("DISCOBOX_USER_NAME", "")
	t.Setenv("DISCOBOX_USER_HOME", "")
	id, err := resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id.uid != 0 || id.gid != 0 || id.name != "root" || id.home != "/root" {
		t.Fatalf("identity = %#v, want root defaults", id)
	}
}

func TestResolveIdentityGidDefaultsToUid(t *testing.T) {
	t.Setenv("DISCOBOX_USER_UID", "1000")
	t.Setenv("DISCOBOX_USER_GID", "")
	t.Setenv("DISCOBOX_USER_NAME", "dev")
	t.Setenv("DISCOBOX_USER_HOME", "/home/dev")
	id, err := resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id.uid != 1000 || id.gid != 1000 || id.name != "dev" || id.home != "/home/dev" {
		t.Fatalf("identity = %#v", id)
	}
}

func TestResolveIdentityRejectsNonNumeric(t *testing.T) {
	t.Setenv("DISCOBOX_USER_UID", "notanumber")
	if _, err := resolveIdentity(); err == nil {
		t.Fatalf("expected error for non-numeric uid")
	}
}

func TestExecPlanInitRunsDirectly(t *testing.T) {
	argv, _ := execPlan(identity{uid: 1000, name: "dev", home: "/home/dev"}, []string{"/sbin/init"})
	if !slices.Equal(argv, []string{"/sbin/init"}) {
		t.Fatalf("argv = %#v, want systemd run directly", argv)
	}
}

func TestExecPlanNonRootUsesRunuser(t *testing.T) {
	argv, _ := execPlan(identity{uid: 1000, name: "dev", home: "/home/dev"}, []string{"echo", "hi"})
	want := []string{"runuser", "-u", "dev", "--", "env", "HOME=/home/dev", "USER=dev", "LOGNAME=dev", "echo", "hi"}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv = %#v, want %#v", argv, want)
	}
}

func TestExecPlanRootKeepsArgvWithUserEnv(t *testing.T) {
	argv, env := execPlan(identity{uid: 0, name: "root", home: "/root"}, []string{"echo", "hi"})
	if !slices.Equal(argv, []string{"echo", "hi"}) {
		t.Fatalf("argv = %#v", argv)
	}
	if !slices.Contains(env, "HOME=/root") || !slices.Contains(env, "USER=root") {
		t.Fatalf("env missing user vars: %#v", env)
	}
}

func TestExecPlanEmptyArgsSleeps(t *testing.T) {
	argv, _ := execPlan(identity{uid: 0, name: "root", home: "/root"}, nil)
	if !slices.Equal(argv, []string{"sleep", "infinity"}) {
		t.Fatalf("argv = %#v, want sleep infinity", argv)
	}
}
