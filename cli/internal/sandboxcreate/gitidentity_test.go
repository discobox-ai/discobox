package sandboxcreate

import (
	"context"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// A repository-local identity is the one the caller meant: setting a work
// address on a work repo is how work-versus-personal is normally separated, and
// resolving from the source directory is what honors it (ADR 0042 §3).
func TestResolveGitIdentityPrefersTheSourceRepository(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	git("config", "user.name", "Repo Local")
	git("config", "user.email", "local@example.com")

	identity := resolveGitIdentity(context.Background(), repo)
	if got := identity.UserName.Or(""); got != "Repo Local" {
		t.Fatalf("userName = %q, want %q", got, "Repo Local")
	}
	if got := identity.UserEmail.Or(""); got != "local@example.com" {
		t.Fatalf("userEmail = %q, want %q", got, "local@example.com")
	}
}

// The @REF suffix is part of the source argument every caller passes; it names a
// ref, not a directory, and must not defeat the lookup.
func TestResolveGitIdentityIgnoresTheRefSuffix(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	identity := resolveGitIdentity(context.Background(), repo+"@feature-foo")
	if got := identity.UserEmail.Or(""); got != "test@example.com" {
		t.Fatalf("userEmail = %q, want the repo's configured address", got)
	}
}

// git is the authority on whether an identity exists. A directory that is not a
// repository at all still resolves through git's global config, and when that
// answers nothing the fields stay absent rather than becoming empty strings.
func TestResolveGitIdentityLeavesUnsetFieldsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	identity := resolveGitIdentity(context.Background(), dir)
	if identity.UserName.Set {
		t.Fatalf("userName = %#v, want absent", identity.UserName)
	}
	if identity.UserEmail.Set {
		t.Fatalf("userEmail = %#v, want absent", identity.UserEmail)
	}
}

func TestSetCreateSandboxGitOmitsAnEmptyIdentity(t *testing.T) {
	body := &apimodel.CreateSandboxBody{}
	setCreateSandboxGit(body, apimodel.SandboxGitIdentity{})
	if body.Config.Git.Set {
		t.Fatal("git object was sent for an unconfigured machine, want it omitted entirely")
	}
}

func TestSetCreateSandboxGitAttachesAPartialIdentity(t *testing.T) {
	body := &apimodel.CreateSandboxBody{}
	identity := apimodel.SandboxGitIdentity{}
	identity.SetUserEmail(apiclientgen.NewOptString("ada@example.com"))

	setCreateSandboxGit(body, identity)
	git, ok := body.Config.Git.Get()
	if !ok {
		t.Fatal("git object omitted, want it attached when either field is set")
	}
	if git.UserName.Set {
		t.Fatalf("userName = %#v, want absent", git.UserName)
	}
	if got := git.UserEmail.Or(""); got != "ada@example.com" {
		t.Fatalf("userEmail = %q, want %q", got, "ada@example.com")
	}
}
