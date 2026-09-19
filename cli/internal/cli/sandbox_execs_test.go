package cli

import (
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/sandboxshell"
)

func TestCreateSandboxExecBodyParsesUserObject(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		user: "darren:1001",
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, ok := body.User.Get()
	if !ok {
		t.Fatal("user was not set")
	}
	if user.Name.Or("") != "darren" || user.Gid.Or(0) != 1001 || user.UID.Set {
		t.Fatalf("user = %#v, want name darren and gid 1001", user)
	}
}

func TestCreateSandboxExecBodyParsesNumericUserObject(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		user: "1000:1001",
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, ok := body.User.Get()
	if !ok {
		t.Fatal("user was not set")
	}
	if user.UID.Or(0) != 1000 || user.Gid.Or(0) != 1001 || user.Name.Set {
		t.Fatalf("user = %#v, want uid 1000 and gid 1001", user)
	}
}

func TestCreateSandboxExecBodyRequestsShellWithoutNamingIt(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{shell: true}, nil)
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	if !body.Shell.Or(false) {
		t.Fatal("shell was not requested")
	}
	// The sandbox resolves the shell, so the client must not send a command.
	if len(body.Command) != 0 {
		t.Fatalf("command = %v, want none", body.Command)
	}

	if _, err := createSandboxExecBody(sandboxExecCreateOptions{shell: true}, []string{"ls"}); err == nil {
		t.Fatal("a command combined with --shell was accepted")
	}
}

func TestCreateSandboxExecBodyUIDGIDFlagsPopulateUserObject(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		uid: "2000",
		gid: []string{"2001"},
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, ok := body.User.Get()
	if !ok {
		t.Fatal("user was not set")
	}
	if user.UID.Or(0) != 2000 || user.Gid.Or(0) != 2001 {
		t.Fatalf("user = %#v, want uid 2000 and gid 2001", user)
	}
}

// --gid is a slice: the first entry is the primary group and the rest are
// supplementary. Each may be a name or a numeric GID, and a name goes over the
// wire unresolved because only the sandbox has the group file.
func TestCreateSandboxExecBodyGroupSliceSplitsPrimaryFromSupplementary(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		uid: "2000",
		gid: []string{"docker", "video", "44"},
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, _ := body.User.Get()
	if user.GroupName.Or("") != "docker" {
		t.Fatalf("groupName = %#v, want docker as the primary group", user.GroupName)
	}
	if user.Gid.Set {
		t.Fatalf("gid = %#v, want unset when the primary group is a name", user.Gid)
	}
	if len(user.AdditionalGroups) != 2 || user.AdditionalGroups[0] != "video" || user.AdditionalGroups[1] != "44" {
		t.Fatalf("additionalGroups = %#v, want [video 44]", user.AdditionalGroups)
	}
}

// A numeric first entry is the gid, not a group name.
func TestCreateSandboxExecBodyNumericPrimaryGroupSetsGid(t *testing.T) {
	body, err := createSandboxExecBody(sandboxExecCreateOptions{
		uid: "2000",
		gid: []string{"2001", "docker"},
	}, []string{"id"})
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	user, _ := body.User.Get()
	if user.Gid.Or(0) != 2001 || user.GroupName.Set {
		t.Fatalf("user = %#v, want gid 2001 and no group name", user)
	}
	if len(user.AdditionalGroups) != 1 || user.AdditionalGroups[0] != "docker" {
		t.Fatalf("additionalGroups = %#v, want [docker]", user.AdditionalGroups)
	}
}

// With neither DISCOBOX_SHELL nor NU_VERSION set, the preferred shell is this
// machine's $SHELL as it stands — which shell that
// means inside the sandbox is the sandbox's call — and nothing at all when
// $SHELL is unset, leaving the sandbox's login shell.
func TestPreferredShellEnvForwardsLocalShell(t *testing.T) {
	t.Setenv(sandboxshell.PreferredEnv, "")
	t.Setenv("NU_VERSION", "")
	t.Setenv("SHELL", "/opt/homebrew/bin/fish")
	if got, want := preferredShellEnv(), []string{sandboxshell.PreferredEnv + "=/opt/homebrew/bin/fish"}; !slices.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
	body, err := createSandboxExecBody(sandboxExecCreateOptions{shell: true, env: preferredShellEnv()}, nil)
	if err != nil {
		t.Fatalf("create body: %v", err)
	}
	env, _ := body.Env.Get()
	if env[sandboxshell.PreferredEnv] != "/opt/homebrew/bin/fish" {
		t.Fatalf("body env = %v, want the preferred shell", env)
	}

	t.Setenv("SHELL", "")
	if got := preferredShellEnv(); got != nil {
		t.Fatalf("env = %v, want none with $SHELL unset", got)
	}
}

// A local DISCOBOX_SHELL is read before $SHELL, so a person can want nu in a
// discobox while keeping zsh on their own machine.
func TestPreferredShellEnvPrefersLocalDiscoboxShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("NU_VERSION", "0.115.1")
	t.Setenv(sandboxshell.PreferredEnv, "/home/linuxbrew/.linuxbrew/bin/nu")
	if got, want := preferredShellEnv(), []string{sandboxshell.PreferredEnv + "=/home/linuxbrew/.linuxbrew/bin/nu"}; !slices.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

// Run from nushell, $SHELL still names the login shell nu was started from;
// NU_VERSION is what says the person is in nu.
func TestPreferredShellEnvRecognizesNushell(t *testing.T) {
	t.Setenv(sandboxshell.PreferredEnv, "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("NU_VERSION", "0.115.1")
	if got, want := preferredShellEnv(), []string{sandboxshell.PreferredEnv + "=nu"}; !slices.Equal(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}
