package boot

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeGroups backs a booter's lookup/run for ensureAdditionalGroups tests: the
// set of groups getent would report present, and the run calls recorded.
type fakeGroups struct {
	present map[string]bool
	runs    [][]string
}

func newFakeGroupsBooter(present ...string) (*booter, *fakeGroups) {
	f := &fakeGroups{present: map[string]bool{}}
	for _, g := range present {
		f.present[g] = true
	}
	return &booter{
		run: func(name string, args ...string) error {
			f.runs = append(f.runs, append([]string{name}, args...))
			return nil
		},
		lookup: func(name string, args ...string) (string, bool) {
			if name == "getent" && len(args) == 2 && args[0] == "group" {
				return args[1] + ":x:1:", f.present[args[1]]
			}
			return "", false
		},
	}, f
}

func TestEnsureAdditionalGroupsAddsPresentGroups(t *testing.T) {
	b, f := newFakeGroupsBooter("docker")
	if err := b.ensureAdditionalGroups(identity{uid: 1000, name: "dev"}, []string{"docker"}); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	want := []string{"usermod", "--append", "--groups", "docker", "dev"}
	if len(f.runs) != 1 || !slices.Equal(f.runs[0], want) {
		t.Fatalf("runs = %v, want [%v]", f.runs, want)
	}
}

func TestEnsureAdditionalGroupsSkipsMissingGroup(t *testing.T) {
	b, f := newFakeGroupsBooter() // "docker" group doesn't exist on this image
	if err := b.ensureAdditionalGroups(identity{uid: 1000, name: "dev"}, []string{"docker"}); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none: a missing group must not fail boot", f.runs)
	}
}

func TestEnsureAdditionalGroupsSkipsRoot(t *testing.T) {
	b, f := newFakeGroupsBooter("docker")
	if err := b.ensureAdditionalGroups(identity{uid: 0, name: "root"}, []string{"docker"}); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none: root needs no supplementary groups", f.runs)
	}
}

func TestEnsureAdditionalGroupsNoGroups(t *testing.T) {
	b, f := newFakeGroupsBooter("docker")
	if err := b.ensureAdditionalGroups(identity{uid: 1000, name: "dev"}, nil); err != nil {
		t.Fatalf("ensureAdditionalGroups: %v", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none", f.runs)
	}
}

// sudo runs with env_reset, so without env_keep the sandbox's proxy variables
// are stripped on the way to root and anything under sudo tries to reach the
// network directly -- which a sandbox has no route for.
func TestSudoersKeepsProxyEnvironment(t *testing.T) {
	content := sudoersContent("dev")
	for _, name := range []string{"HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "no_proxy", "NODE_USE_ENV_PROXY"} {
		if !strings.Contains(content, name) {
			t.Fatalf("env_keep is missing %s:\n%s", name, content)
		}
	}
	if !strings.Contains(content, "Defaults env_keep +=") {
		t.Fatalf("expected an env_keep default:\n%s", content)
	}
	// The grant itself must survive alongside it.
	if !strings.Contains(content, "dev ALL=(ALL) NOPASSWD:ALL") {
		t.Fatalf("sudo grant lost:\n%s", content)
	}
}

// fakeAccounts backs a booter for the account tests: the passwd entries getent
// answers by name and by uid, which userdel removes, and every command run.
type fakeAccounts struct {
	passwd []string
	groups []string
	runs   [][]string
}

func newFakeAccountsBooter(entries ...string) (*booter, *fakeAccounts) {
	f := &fakeAccounts{passwd: entries}
	record := func(name string, args []string) {
		f.runs = append(f.runs, append([]string{name}, args...))
	}
	return &booter{
		run: func(name string, args ...string) error {
			record(name, args)
			switch name {
			case "userdel":
				// userdel takes the account's own group with it, as
				// USERGROUPS_ENAB has it do on Debian and Ubuntu.
				user := args[len(args)-1]
				f.passwd = slices.DeleteFunc(f.passwd, func(e string) bool { return firstField(e) == user })
				f.groups = slices.DeleteFunc(f.groups, func(e string) bool { return firstField(e) == user })
			case "groupadd":
				f.groups = append(f.groups, fmt.Sprintf("%s:x:%s:", args[len(args)-1], args[1]))
			}
			return nil
		},
		lookup: func(name string, args ...string) (string, bool) {
			if name != "getent" || len(args) != 2 {
				return "", false
			}
			table := f.passwd
			if args[0] == "group" {
				table = f.groups
			}
			for _, e := range table {
				fields := strings.Split(e, ":")
				if fields[0] == args[1] || fields[2] == args[1] {
					return e + "\n", true
				}
			}
			return "", false
		},
		exists: func(name string, args ...string) bool {
			record(name, args)
			return false
		},
	}, f
}

// noMounts points mountInfoPath at a mountinfo listing only the given mount
// points, and logind's state at an empty directory.
func noMounts(t *testing.T, mounts ...string) {
	t.Helper()
	dir := t.TempDir()
	var lines strings.Builder
	lines.WriteString("22 1 0:21 / / rw,relatime - overlay overlay rw\n")
	for i, m := range mounts {
		fmt.Fprintf(&lines, "%d 22 0:%d / %s rw,relatime - ext4 /dev/sda1 rw\n", 30+i, 30+i, strings.ReplaceAll(m, " ", `\040`))
	}
	path := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(path, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	prevMounts, prevUsers := mountInfoPath, loginUsersDir
	mountInfoPath, loginUsersDir = path, filepath.Join(dir, "users")
	t.Cleanup(func() { mountInfoPath, loginUsersDir = prevMounts, prevUsers })
}

// accountHome makes a directory owned by this test's own uid, the only owner
// a test can give it, for an account at that uid.
func accountHome(t *testing.T) (string, int) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "provider")
	if err := os.MkdirAll(filepath.Join(home, ".nvm"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home, os.Getuid()
}

func ran(runs [][]string, name string) []string {
	for _, r := range runs {
		if r[0] == name {
			return r
		}
	}
	return nil
}

// The provider's uid-1000 account is deleted, home and all, not renamed into
// the sandbox's (ADR 26-10-16-423 §14).
func TestClearAccountsDeletesTheAccountHoldingTheUid(t *testing.T) {
	skipWithoutPOSIXIDs(t)
	home, uid := accountHome(t)
	noMounts(t)
	b, f := newFakeAccountsBooter(fmt.Sprintf("provider:x:%d:%d::%s:/bin/bash", uid, uid+7, home))
	id := identity{uid: uid, gid: uid + 3, name: "dev", home: filepath.Join(t.TempDir(), "dev"), configured: true}

	kept, err := b.clearAccountsInTheWay(id)
	if err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	if kept {
		t.Fatal("kept = true, want false: the account at the uid is another's")
	}
	if got, want := ran(f.runs, "userdel"), []string{"userdel", "--force", "provider"}; !slices.Equal(got, want) {
		t.Fatalf("userdel = %v, want %v (runs %v)", got, want, f.runs)
	}
	if r := ran(f.runs, "usermod"); r != nil {
		t.Fatalf("ran %v: an existing account is never modified in place", r)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want the deleted account's home gone", home, err)
	}
}

// An account under the sandbox's name at another uid is in the way just the
// same.
func TestClearAccountsDeletesTheAccountHoldingTheName(t *testing.T) {
	noMounts(t)
	b, f := newFakeAccountsBooter("dev:x:1001:1201::/nonexistent/dev:/bin/sh")
	id := identity{uid: 1500, gid: 1600, name: "dev", home: "/home/dev", configured: true}

	kept, err := b.clearAccountsInTheWay(id)
	if err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	if kept {
		t.Fatal("kept = true, want false: the name's account has another uid")
	}
	if got, want := ran(f.runs, "userdel"), []string{"userdel", "--force", "dev"}; !slices.Equal(got, want) {
		t.Fatalf("userdel = %v, want %v (runs %v)", got, want, f.runs)
	}
}

// The sandbox's own account from an earlier boot is exactly what it asks for,
// so it is kept rather than deleted and made again.
func TestClearAccountsKeepsTheSandboxsOwnAccount(t *testing.T) {
	noMounts(t)
	b, f := newFakeAccountsBooter("dev:x:1500:1600::/home/dev:/bin/bash")
	id := identity{uid: 1500, gid: 1600, name: "dev", home: "/home/dev/", configured: true}

	kept, err := b.clearAccountsInTheWay(id)
	if err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	if !kept || len(f.runs) != 0 {
		t.Fatalf("kept = %v, runs = %v; want the account kept and nothing run", kept, f.runs)
	}
}

// Its own name and uid at another gid is not the sandbox's account: it is
// deleted and made again, never given a new gid in place.
func TestClearAccountsRemakesAnAccountWithAnotherGid(t *testing.T) {
	noMounts(t)
	b, f := newFakeAccountsBooter("dev:x:1500:1700::/home/dev:/bin/bash")
	id := identity{uid: 1500, gid: 1600, name: "dev", home: "/home/dev", configured: true}

	kept, err := b.clearAccountsInTheWay(id)
	if err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	if kept || ran(f.runs, "userdel") == nil {
		t.Fatalf("kept = %v, runs = %v; want the account deleted", kept, f.runs)
	}
}

// A home that is a mount point, or has one beneath it, is not the deleted
// account's to lose: removing it would empty what is mounted.
func TestClearAccountsKeepsAHomeWithAMount(t *testing.T) {
	skipWithoutPOSIXIDs(t)
	for _, tc := range []struct {
		name  string
		mount func(home string) string
	}{
		{"mount point", func(home string) string { return home }},
		{"mount beneath", func(home string) string { return filepath.Join(home, "with space") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, uid := accountHome(t)
			noMounts(t, tc.mount(home))
			b, _ := newFakeAccountsBooter(fmt.Sprintf("provider:x:%d:%d::%s:/bin/bash", uid, uid+7, home))
			id := identity{uid: uid, gid: uid + 3, name: "dev", home: filepath.Join(t.TempDir(), "dev"), configured: true}
			if _, err := b.clearAccountsInTheWay(id); err != nil {
				t.Fatalf("clearAccountsInTheWay: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, ".nvm")); err != nil {
				t.Fatalf("stat: %v; want a home with a mount left whole", err)
			}
		})
	}
}

// A plain directory at the path the sandbox's home will be is the old
// account's and goes with it, so the sandbox's account gets the skeleton
// rather than files of a uid that no longer exists.
func TestClearAccountsRemovesAPlainHomeAtTheSandboxsPath(t *testing.T) {
	skipWithoutPOSIXIDs(t)
	home, uid := accountHome(t)
	noMounts(t)
	// dev at another uid is in the way of the sandbox's dev, and its home is
	// the one the sandbox's dev will have.
	b, f := newFakeAccountsBooter(fmt.Sprintf("dev:x:%d:%d::%s:/bin/bash", uid, uid+7, home))
	id := identity{uid: uid + 1, gid: uid + 3, name: "dev", home: home, configured: true}
	if _, err := b.clearAccountsInTheWay(id); err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	if ran(f.runs, "userdel") == nil {
		t.Fatalf("runs = %v, want the account deleted", f.runs)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want the old account's plain home removed", home, err)
	}
}

// A home the account does not own -- a system account's, such as daemon's
// /usr/sbin -- is never removed with it.
func TestClearAccountsKeepsAHomeTheAccountDoesNotOwn(t *testing.T) {
	skipWithoutPOSIXIDs(t)
	home, uid := accountHome(t)
	noMounts(t)
	b, _ := newFakeAccountsBooter(fmt.Sprintf("daemon:x:%d:%d::%s:/usr/sbin/nologin", uid+1, uid+9, home))
	id := identity{uid: uid, gid: uid + 3, name: "daemon", home: "/home/daemon", configured: true}
	if _, err := b.clearAccountsInTheWay(id); err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".nvm")); err != nil {
		t.Fatalf("stat: %v; want a home owned by someone else left whole", err)
	}
}

// A logged-in provider user has its sessions ended and its processes killed
// before userdel, which would otherwise refuse it (PR #180).
func TestClearAccountsEndsALoggedInUsersProcessesFirst(t *testing.T) {
	noMounts(t)
	if err := os.MkdirAll(loginUsersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(loginUsersDir, "1000"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b, f := newFakeAccountsBooter("boxd:x:1000:1100::/nonexistent/boxd:/bin/bash")
	id := identity{uid: 1000, gid: 2000, name: "dev", home: "/home/dev", configured: true}
	if _, err := b.clearAccountsInTheWay(id); err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	want := [][]string{
		{"loginctl", "terminate-user", "1000"},
		{"pkill", "--signal", "KILL", "--uid", "1000"},
		{"userdel", "--force", "boxd"},
	}
	if !slices.EqualFunc(f.runs, want, slices.Equal) {
		t.Fatalf("runs = %v, want %v", f.runs, want)
	}
}

// With nobody logged in there is no logind session to end: Wire runs before
// logind does.
func TestClearAccountsSkipsLoginctlWithNoSession(t *testing.T) {
	noMounts(t)
	b, f := newFakeAccountsBooter("boxd:x:1000:1100::/nonexistent/boxd:/bin/bash")
	id := identity{uid: 1000, gid: 2000, name: "dev", home: "/home/dev", configured: true}
	if _, err := b.clearAccountsInTheWay(id); err != nil {
		t.Fatalf("clearAccountsInTheWay: %v", err)
	}
	if r := ran(f.runs, "loginctl"); r != nil {
		t.Fatalf("ran %v with no session to end", r)
	}
}

// Root is never deleted, whichever way it is in the way.
func TestClearAccountsNeverDeletesRoot(t *testing.T) {
	noMounts(t)
	b, f := newFakeAccountsBooter("toor:x:0:0::/root:/bin/sh")
	id := identity{uid: 1000, gid: 2000, name: "toor", home: "/home/toor", configured: true}
	if _, err := b.clearAccountsInTheWay(id); err == nil {
		t.Fatal("clearAccountsInTheWay deleted a uid-0 account")
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none", f.runs)
	}
}

// A client account named root at a non-zero uid is refused before anything is
// deleted; root itself, uid 0, needs no account.
func TestEnsureUserRefusesRootAtAnotherUid(t *testing.T) {
	noMounts(t)
	b, f := newFakeAccountsBooter("root:x:0:0::/root:/bin/sh", "provider:x:1000:1100::/home/provider:/bin/bash")
	err := b.ensureUser(identity{uid: 1000, gid: 2000, name: "root", home: "/root", configured: true})
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("ensureUser = %v, want root at uid 1000 refused", err)
	}
	if len(f.runs) != 0 {
		t.Fatalf("runs = %v, want none", f.runs)
	}
	if err := b.ensureUser(identity{uid: 0, gid: 0, name: "root", home: "/root", configured: true}); err != nil || len(f.runs) != 0 {
		t.Fatalf("ensureUser(root) = %v, runs %v; want nothing to do", err, f.runs)
	}
}

// sudoersIn points sudoersPath into a test directory.
func sudoersIn(t *testing.T) {
	t.Helper()
	prev := sudoersPath
	sudoersPath = filepath.Join(t.TempDir(), "sudoers.d", "discobox-user")
	t.Cleanup(func() { sudoersPath = prev })
}

// The account in the way is deleted before the group is resolved, since
// userdel takes its group with it: resolved first, the group at the sandbox's
// gid would be the provider's, and useradd would name a group already gone.
// Then the account is created, from nothing.
func TestEnsureUserDeletesBeforeResolvingTheGroup(t *testing.T) {
	noMounts(t)
	sudoersIn(t)
	b, f := newFakeAccountsBooter("ubuntu:x:1000:1100::/nonexistent/ubuntu:/bin/bash")
	f.groups = []string{"ubuntu:x:1100:"}
	id := identity{uid: 1000, gid: 1100, name: "dev", home: "/home/dev", configured: true}
	if err := b.ensureUser(id); err != nil {
		t.Fatalf("ensureUser: %v", err)
	}
	want := [][]string{
		{"pkill", "--signal", "KILL", "--uid", "1000"},
		{"userdel", "--force", "ubuntu"},
		{"groupadd", "--gid", "1100", "dev"},
		{"useradd", "--uid", "1000", "--gid", "dev", "--home-dir", "/home/dev", "--shell", "/bin/bash", "dev"},
		{"visudo", "-cf", sudoersPath},
	}
	if !slices.EqualFunc(f.runs, want, slices.Equal) {
		t.Fatalf("runs = %v, want %v", f.runs, want)
	}
}

// The sandbox's own account from an earlier boot is kept as it is: no
// useradd, and nothing modified.
func TestEnsureUserKeepsTheSandboxsOwnAccount(t *testing.T) {
	noMounts(t)
	sudoersIn(t)
	b, f := newFakeAccountsBooter("dev:x:1000:1100::/home/dev:/bin/bash")
	f.groups = []string{"dev:x:1100:"}
	id := identity{uid: 1000, gid: 1100, name: "dev", home: "/home/dev", configured: true}
	if err := b.ensureUser(id); err != nil {
		t.Fatalf("ensureUser: %v", err)
	}
	if want := [][]string{{"visudo", "-cf", sudoersPath}}; !slices.EqualFunc(f.runs, want, slices.Equal) {
		t.Fatalf("runs = %v, want %v", f.runs, want)
	}
}
