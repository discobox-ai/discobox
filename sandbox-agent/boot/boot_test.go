package boot

import (
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

// An empty environment means the manifest named nobody, so boot provisions no
// account and the image's own user stands (ADR 0025 §5) -- but it must still
// answer who that is, resolved from /etc/passwd against this process's own
// current uid/gid rather than invented (§6): nothing here has called setuid
// yet, so those are the image's starting identity already. A blank answer
// used to reach harness.ResolveVolumes and crash boot the moment a harness
// image declared a %HOME%-templated volume (e.g. claude-code's), since an
// empty expansion is refused as a missing path.
func TestResolveIdentityWithNoUserConfiguredResolvesTheImagesOwnUser(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	for _, key := range []string{"DISCOBOX_USER_UID", "DISCOBOX_USER_GID", "DISCOBOX_USER_NAME", "DISCOBOX_USER_HOME", "DISCOBOX_USER_GROUP"} {
		t.Setenv(key, "")
	}
	id, err := resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id.configured {
		t.Fatalf("identity = %#v, want unconfigured: nobody asked to provision a user", id)
	}
	// The fixture's effective ids, which deliberately differ from each other so
	// a uid/gid transposition cannot pass. Asserting against os.Getuid here --
	// as this test used to -- would have held for any implementation.
	if id.uid != 1500 || id.gid != 1600 {
		t.Fatalf("identity = %#v, want the image's own 1500/1600", id)
	}
	if id.name != "image" || id.home != "/home/image" {
		t.Fatalf("identity = %#v, want the image account's name and home", id)
	}
}

// An image running as a uid its own /etc/passwd has no entry for cannot answer
// for its name and home. Boot needs both -- they build the process environment
// and expand a harness's %HOME%-templated volumes -- so it must fail here,
// naming the cause, rather than carry blanks downstream to resurface as a
// missing path.
func TestResolveIdentityRejectsAnImageUidWithNoPasswdEntry(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	t.Cleanup(runuser.FixedEffectiveIDs(4242, 4242))
	for _, key := range []string{"DISCOBOX_USER_UID", "DISCOBOX_USER_GID", "DISCOBOX_USER_NAME", "DISCOBOX_USER_HOME", "DISCOBOX_USER_GROUP"} {
		t.Setenv(key, "")
	}
	id, err := resolveIdentity()
	if err == nil {
		t.Fatalf("identity = %#v, want an error: uid 4242 has no passwd entry", id)
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Fatalf("error = %q, want the uid named in it", err)
	}
}

// A missing gid is read from the account's own entry, never copied from the
// uid: uid==gid is a useradd coincidence, not a rule (ADR 0025 §6).
func TestResolveIdentityReadsAMissingGidFromTheAccount(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	t.Setenv("DISCOBOX_USER_UID", "1000")
	t.Setenv("DISCOBOX_USER_GID", "")
	t.Setenv("DISCOBOX_USER_GROUP", "")
	t.Setenv("DISCOBOX_USER_NAME", "dev")
	t.Setenv("DISCOBOX_USER_HOME", "/home/dev")
	id, err := resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id.uid != 1000 || id.gid != 2000 {
		t.Fatalf("identity = %#v, want uid 1000 with the account's own gid 2000", id)
	}
	if !id.configured {
		t.Fatal("identity must be configured when the manifest named a user")
	}
}

// A primary group given by name resolves against the image's group file.
func TestResolveIdentityResolvesAGroupName(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	t.Setenv("DISCOBOX_USER_UID", "1000")
	t.Setenv("DISCOBOX_USER_GID", "")
	t.Setenv("DISCOBOX_USER_GROUP", "docker")
	t.Setenv("DISCOBOX_USER_NAME", "dev")
	t.Setenv("DISCOBOX_USER_HOME", "/home/dev")
	id, err := resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id.gid != 997 {
		t.Fatalf("gid = %d, want 997 resolved from the group name", id.gid)
	}
}

func TestResolveIdentityRejectsGidAndGroupTogether(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	t.Setenv("DISCOBOX_USER_UID", "1000")
	t.Setenv("DISCOBOX_USER_GID", "2000")
	t.Setenv("DISCOBOX_USER_GROUP", "docker")
	t.Setenv("DISCOBOX_USER_NAME", "dev")
	if _, err := resolveIdentity(); err == nil {
		t.Fatal("expected an error when gid and group name are both set")
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
	argv, _ := execPlan(identity{uid: 1000, name: "dev", home: "/home/dev", configured: true}, []string{"echo", "hi"})
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
