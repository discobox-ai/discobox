package sandboxcreate

import (
	"context"
	"fmt"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/internal/gitutil"
)

var runUnixUserNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}\$?$`)

type runUserIdentity struct {
	Name          string
	UID           int64
	GID           int64
	HomeDirectory string
	IDsUsable     bool
}

func resolveRunUserIdentity() (runUserIdentity, bool, error) {
	current, err := user.Current()
	if err != nil {
		return runUserIdentity{}, false, fmt.Errorf("resolve current user: %w", err)
	}
	return parseRunUserIdentity(current)
}

func parseRunUserIdentity(current *user.User) (runUserIdentity, bool, error) {
	if current == nil {
		return runUserIdentity{}, false, nil
	}
	uid, uidOK := parseRunNumericUserID(current.Uid)
	gid, gidOK := parseRunNumericUserID(current.Gid)
	if uidOK && uid == 0 {
		return runUserIdentity{}, false, nil
	}
	identity := runUserIdentity{}
	if validRunUnixUserName(current.Username) {
		identity.Name = current.Username
	}
	identity.HomeDirectory = current.HomeDir
	if uidOK && gidOK && uid != 0 {
		identity.UID = uid
		identity.GID = gid
		identity.IDsUsable = true
	}
	if identity.Name == "" && identity.HomeDirectory == "" && !identity.IDsUsable {
		return runUserIdentity{}, false, nil
	}
	return identity, true, nil
}

func parseRunNumericUserID(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil
}

func validRunUnixUserName(value string) bool {
	return runUnixUserNamePattern.MatchString(value)
}

// resolveGitIdentity reads the git authorship the sandbox should commit under,
// with git's own resolution from the local source directory -- so a
// repository-local override wins over the global one (ADR 0042 §3).
//
// sourceArg is the create request's source, @REF suffix and all. A remote
// repository has no local worktree, so the read falls back to the process
// working directory, mirroring how ResolveOrigin already resolves an origin for
// a remote source.
//
// An unconfigured identity is left absent. git is the authority on whether one
// is set, and $USER@$(hostname) here would only relocate the wrong fallback this
// exists to replace.
func resolveGitIdentity(ctx context.Context, sourceArg string) apimodel.SandboxGitIdentity {
	source, _, _ := splitRunSourceRef(sourceArg)
	dir := strings.TrimSpace(source)
	if dir == "" || isRemoteGitSource(dir) {
		// Empty means git runs in this process's own directory, which is the
		// intended fallback -- no need to fail the create over a cwd lookup.
		dir = ""
	}
	git := apimodel.SandboxGitIdentity{}
	if name, ok := gitutil.ConfigValue(ctx, dir, "user.name"); ok {
		git.SetUserName(apiclientgen.NewOptString(name))
	}
	if email, ok := gitutil.ConfigValue(ctx, dir, "user.email"); ok {
		git.SetUserEmail(apiclientgen.NewOptString(email))
	}
	return git
}

// setCreateSandboxGit attaches the identity only when git actually had one, so
// an unconfigured machine sends no `git` object at all rather than an empty one.
func setCreateSandboxGit(body *apimodel.CreateSandboxBody, git apimodel.SandboxGitIdentity) {
	if !git.UserName.Set && !git.UserEmail.Set {
		return
	}
	body.Config.SetGit(apiclientgen.NewOptSandboxGitIdentity(git))
}

func (u runUserIdentity) setCreateSandboxUser(body *apimodel.CreateSandboxBody) {
	sandboxUser := apimodel.SandboxUser{}
	sandboxUser.SetName(optionalString(u.Name))
	sandboxUser.SetHomeDirectory(optionalString(u.HomeDirectory))
	if u.IDsUsable {
		sandboxUser.SetUID(apiclientgen.NewOptInt64(u.UID))
		sandboxUser.SetGid(apiclientgen.NewOptInt64(u.GID))
	}
	body.Config.SetUser(apiclientgen.NewOptSandboxUser(sandboxUser))
}
