package boot

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// A sandbox with no PID-1 flow (a VM guest, ADR 26-10-09-143 §1) is provisioned
// from its bootstrap alone: the working root, the home skeleton, the git
// identity and the direnv whitelist are all seeded for its user, and nothing is
// wired -- it has no primary volumes to wire from, and its sources arrive
// through the intake rather than as binds.
func TestProvisionWithoutWiringSeedsWhatTheBootstrapDeclares(t *testing.T) {
	id := selfIdentity(t)
	b, git := newFakeGitBooter(nil)
	root := t.TempDir()
	workingRoot := filepath.Join(root, "workspace")
	source := filepath.Join(root, "workspace", "repo")
	bootstrap := sandboxconfig.Config{
		AgentRuntime: sandboxconfig.AgentRuntime{WorkingRoot: workingRoot},
		Git:          sandboxconfig.GitIdentity{UserName: "Dev", UserEmail: "dev@example.com"},
		Sources:      []sandboxconfig.Source{{Slug: "primary", Target: source}},
	}

	if err := b.provision(slog.Default(), id, &bootstrap, false); err != nil {
		t.Fatalf("provision: %v", err)
	}

	if info, err := os.Stat(workingRoot); err != nil || !info.IsDir() {
		t.Fatalf("working root %s was not created: %v", workingRoot, err)
	}
	if got := git.configWrites(); got["user.name"] != "Dev" || got["user.email"] != "dev@example.com" {
		t.Fatalf("git config writes = %v, want the bootstrap's identity", got)
	}
	direnv, ok := readDirenvConfig(t, id.home)
	if !ok || !strings.Contains(direnv, `"`+source+`"`) {
		t.Fatalf("direnv.toml = %q (written %v), want the source target whitelisted", direnv, ok)
	}
	// wireSources would have created the target to bind onto; the intake's
	// clone is what fills it here.
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source target %s exists (%v); nothing should have been wired", source, err)
	}
	skel := false
	for _, run := range git.runs {
		if run[0] == "cp" && len(run) > 2 && run[2] == "/etc/skel" {
			skel = true
		}
	}
	if !skel {
		t.Fatalf("runs = %v, want the home seeded from /etc/skel", git.runs)
	}
}

// With no bootstrap at all -- a bare `docker run ... bash` debug session --
// only the user and its home are set up: there is no working root, identity
// or source to seed from.
func TestProvisionWithoutABootstrapSeedsOnlyTheHome(t *testing.T) {
	id := selfIdentity(t)
	b, git := newFakeGitBooter(nil)
	if err := b.provision(slog.Default(), id, nil, false); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := git.configWrites(); len(got) != 0 {
		t.Fatalf("git config writes = %v, want none without a bootstrap", got)
	}
	if _, ok := readDirenvConfig(t, id.home); ok {
		t.Fatal("direnv.toml was written without a bootstrap")
	}
}

// Where no PID-1 flow ran, nothing injected DISCOBOX_USER_*: the user is
// sandbox.json's own, and a stray environment does not override it.
func TestBootstrapIdentityIsTheBootstrapsUser(t *testing.T) {
	skipWithoutPOSIXIDs(t)
	t.Cleanup(runuser.FixedDatabase())
	t.Setenv("DISCOBOX_USER_UID", "1500")
	t.Setenv("DISCOBOX_USER_NAME", "image")
	uid := int64(1000)
	id, err := bootstrapIdentity(sandboxconfig.Config{User: sandboxconfig.User{UID: &uid, Name: "dev"}})
	if err != nil {
		t.Fatalf("bootstrap identity: %v", err)
	}
	if !id.configured || id.uid != 1000 || id.gid != 2000 || id.name != "dev" || id.home != "/home/dev" {
		t.Fatalf("identity = %#v, want the bootstrap's dev 1000/2000", id)
	}
}

func TestBootstrapIdentityRejectsGidAndGroupTogether(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	uid, gid := int64(1000), int64(2000)
	user := sandboxconfig.User{UID: &uid, GID: &gid, GroupName: "docker", Name: "dev"}
	if _, err := bootstrapIdentity(sandboxconfig.Config{User: user}); err == nil {
		t.Fatal("expected an error when gid and group name are both set")
	}
}

// A sandbox is provisioned once: an agent restarted after a crash, or a machine
// stopped and started, finds the marker naming its own sandbox and runs nothing
// -- no account changes under the user's running processes, no walk of home.
func TestProvisionWithoutInitRunsOncePerSandbox(t *testing.T) {
	dir := t.TempDir()
	saved := provisionedPath
	provisionedPath = filepath.Join(dir, "provisioned")
	t.Cleanup(func() { provisionedPath = saved })
	bootstrap := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(bootstrap, []byte(`{"sandboxId":"sbx_1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := markProvisioned(provisionedPath, "sbx_1"); err != nil {
		t.Fatal(err)
	}
	b := &booter{
		run: func(name string, args ...string) error {
			t.Fatalf("ran %s %v on a sandbox already provisioned", name, args)
			return nil
		},
	}
	if err := b.provisionWithoutInit(slog.Default(), bootstrap); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := provisionedFor(provisionedPath); got != "sbx_1" {
		t.Fatalf("provisioned for %q, want sbx_1", got)
	}
}
