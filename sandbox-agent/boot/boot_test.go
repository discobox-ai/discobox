package boot

import (
	"path/filepath"
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
	id, err := inertBooter(t).resolveIdentity()
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
	id, err := inertBooter(t).resolveIdentity()
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
	id, err := inertBooter(t).resolveIdentity()
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
	id, err := inertBooter(t).resolveIdentity()
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
	if _, err := inertBooter(t).resolveIdentity(); err == nil {
		t.Fatal("expected an error when gid and group name are both set")
	}
}

func TestResolveIdentityRejectsNonNumeric(t *testing.T) {
	t.Setenv("DISCOBOX_USER_UID", "notanumber")
	if _, err := inertBooter(t).resolveIdentity(); err == nil {
		t.Fatalf("expected error for non-numeric uid")
	}
}

// setManifestUser sets the manifest's user as the pool agent forwards it, and
// clears every field not given so this machine's own environment cannot leak
// into the manifest.
func setManifestUser(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range []string{"DISCOBOX_USER_UID", "DISCOBOX_USER_GID", "DISCOBOX_USER_NAME", "DISCOBOX_USER_HOME", "DISCOBOX_USER_GROUP"} {
		t.Setenv(key, env[key])
	}
}

// inertBooter is for resolution that must not touch the system: every command
// is a test failure. The one case that does run a command, an account named
// without ids, has useraddBooter.
func inertBooter(t *testing.T) *booter {
	t.Helper()
	refuse := func(name string, args ...string) {
		t.Helper()
		t.Fatalf("resolution ran %s %v; it must not touch the system", name, args)
	}
	return &booter{
		run:    func(name string, args ...string) error { refuse(name, args...); return nil },
		lookup: func(name string, args ...string) (string, bool) { refuse(name, args...); return "", false },
		exists: func(name string, args ...string) bool { refuse(name, args...); return false },
	}
}

// useraddBooter records the commands boot runs and, when one is useradd, adds
// the account to the fixed database with the ids given here -- what the real
// useradd would have written to /etc/passwd -- so the resolution that follows
// finds it. Its getent knows the fixture's group 2000 and nothing else.
func useraddBooter(t *testing.T, uid, gid int64) (*booter, *[][]string) {
	t.Helper()
	runs := &[][]string{}
	return &booter{
		run: func(name string, args ...string) error {
			*runs = append(*runs, append([]string{name}, args...))
			if name != "useradd" {
				return nil
			}
			account := args[len(args)-1]
			home := filepath.Join("/home", account)
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--home-dir" {
					home = args[i+1]
				}
			}
			t.Cleanup(runuser.FixedAccount(account, uid, gid, home))
			return nil
		},
		lookup: func(name string, args ...string) (string, bool) {
			if name == "getent" && slices.Equal(args, []string{"group", "2000"}) {
				return "dev:x:2000:", true
			}
			return "", false
		},
	}, runs
}

// A manifest that names an account by name alone, one the image does not have,
// is what a macOS client sends: its own ids are below the range a Linux guest
// gives accounts, so it asks by name and leaves the numbers to the sandbox.
// Boot creates the account and useradd's allocation answers for the ids;
// nothing here picks a number.
func TestResolveIdentityCreatesTheAccountAManifestNamesWithoutIDs(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	setManifestUser(t, map[string]string{"DISCOBOX_USER_NAME": "ada", "DISCOBOX_USER_HOME": "/Users/ada"})
	b, runs := useraddBooter(t, 1001, 2001)
	id, err := b.resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	want := []string{"useradd", "--home-dir", "/Users/ada", "--shell", "/bin/bash", "ada"}
	if len(*runs) != 1 || !slices.Equal((*runs)[0], want) {
		t.Fatalf("runs = %v, want [%v]", *runs, want)
	}
	if id != (identity{uid: 1001, gid: 2001, name: "ada", home: "/Users/ada", configured: true}) {
		t.Fatalf("identity = %#v, want the account useradd created", id)
	}
}

// The home of an account boot creates is /home/<name> when the manifest named
// none: the same decision resolveIdentity makes for an account given ids.
func TestResolveIdentityCreatesAnAccountWithNoHomeUnderHome(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	setManifestUser(t, map[string]string{"DISCOBOX_USER_NAME": "ada"})
	b, runs := useraddBooter(t, 1001, 2001)
	id, err := b.resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	want := []string{"useradd", "--home-dir", "/home/ada", "--shell", "/bin/bash", "ada"}
	if len(*runs) != 1 || !slices.Equal((*runs)[0], want) {
		t.Fatalf("runs = %v, want [%v]", *runs, want)
	}
	if id.home != "/home/ada" {
		t.Fatalf("home = %q, want /home/ada", id.home)
	}
}

// A primary group the manifest chose for an account it names without ids goes
// to useradd rather than being left to its default. A gid the image lacks is
// created first, as for any configured account; a group name has already been
// checked against the image by the resolver.
func TestResolveIdentityCreatesTheAccountInTheGroupTheManifestNames(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	for _, tc := range []struct {
		name string
		env  map[string]string
		want [][]string
	}{
		{
			name: "a gid the image has",
			env:  map[string]string{"DISCOBOX_USER_NAME": "ada", "DISCOBOX_USER_GID": "2000"},
			want: [][]string{{"useradd", "--home-dir", "/home/ada", "--shell", "/bin/bash", "--gid", "dev", "ada"}},
		},
		{
			name: "a gid the image lacks",
			env:  map[string]string{"DISCOBOX_USER_NAME": "ada", "DISCOBOX_USER_GID": "3000"},
			want: [][]string{
				{"groupadd", "--gid", "3000", "ada"},
				{"useradd", "--home-dir", "/home/ada", "--shell", "/bin/bash", "--gid", "ada", "ada"},
			},
		},
		{
			name: "a group name",
			env:  map[string]string{"DISCOBOX_USER_NAME": "ada", "DISCOBOX_USER_GROUP": "docker"},
			want: [][]string{{"useradd", "--home-dir", "/home/ada", "--shell", "/bin/bash", "--gid", "docker", "ada"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setManifestUser(t, tc.env)
			b, runs := useraddBooter(t, 1001, 2000)
			if _, err := b.resolveIdentity(); err != nil {
				t.Fatalf("resolve identity: %v", err)
			}
			if !slices.EqualFunc(*runs, tc.want, slices.Equal) {
				t.Fatalf("runs = %v, want %v", *runs, tc.want)
			}
		})
	}
}

// An account the image already has is resolved, not created, however little
// the manifest said about it: the name alone finds its ids.
func TestResolveIdentityDoesNotCreateAnAccountTheImageHas(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	setManifestUser(t, map[string]string{"DISCOBOX_USER_NAME": "dev"})
	id, err := inertBooter(t).resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id.uid != 1000 || id.gid != 2000 || id.home != "/home/dev" || !id.configured {
		t.Fatalf("identity = %#v, want dev's own 1000/2000 and home", id)
	}
}

// An account the manifest gave ids to is ensureUser's to create, with exactly
// those ids; resolution runs nothing for it.
func TestResolveIdentityLeavesAnAccountWithIDsToEnsureUser(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	setManifestUser(t, map[string]string{"DISCOBOX_USER_NAME": "ada", "DISCOBOX_USER_UID": "4242", "DISCOBOX_USER_GID": "4343"})
	id, err := inertBooter(t).resolveIdentity()
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if id != (identity{uid: 4242, gid: 4343, name: "ada", home: "/home/ada", configured: true}) {
		t.Fatalf("identity = %#v, want the manifest's ids and a default home", id)
	}
}

// A home alone names nobody boot could create. An account needs a name, and
// inventing one is not boot's to do.
func TestResolveIdentityRefusesToCreateAnAccountWithNoName(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	setManifestUser(t, map[string]string{"DISCOBOX_USER_HOME": "/Users/ada"})
	_, err := inertBooter(t).resolveIdentity()
	if err == nil || !strings.Contains(err.Error(), "DISCOBOX_USER_NAME") {
		t.Fatalf("err = %v, want the missing name named", err)
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
