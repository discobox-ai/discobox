package sandboxcreate

import (
	"context"
	"fmt"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sandboxuser"
	"github.com/discobox-ai/x/gitutil"
)

var runUnixUserNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}\$?$`)

// runUserIdentity is this machine's account as a sandbox can hold it. It always
// carries both ids: a sandbox create refuses an account named without its uid,
// and an id outside root and the guest's account range (ADR 0141), so an id the
// guest cannot give an account is replaced here, by the client, rather than
// dropped or clamped at runtime.
type runUserIdentity struct {
	Name          string
	UID           int64
	GID           int64
	HomeDirectory string
}

// fallbackRunUserName names the sandbox account when this machine has no name a
// Linux sandbox could use: the product, rather than an impersonation of an
// account whose name would only be a coincidence in the sandbox.
const fallbackRunUserName = "discobox"

// windowsRunUser is the identity a create carries from a Windows client.
//
// Windows has nothing POSIX to capture. user.Current() there answers with a SID
// for the ids, a DOMAIN\name for the name, and a C:\Users home -- three values
// a Linux sandbox cannot use, which is why every field below is rejected. What
// remains is an empty identity, and an empty identity means the image's own
// user (ADR 0025 sec 5): the sandbox runs as root on any harness image without a
// USER directive, which is not the sandbox the person on Windows asked for.
// They asked for the one their colleagues on Linux and macOS get.
//
// So Windows supplies a fixed identity instead of a translated one. This is not
// the host inventing an id it should have asked for (ADR 0025 sec 4) -- there is
// no local answer to ask for, and nothing to get wrong. It is the client
// stating, as a whole request, the user it wants the sandbox to have; boot then
// creates that account exactly as it would for one a Linux client named.
//
// 1000 is the first non-system id on every distro the harness images build from,
// and the name is fallbackRunUserName. The home directory is deliberately
// absent: boot resolves it from the account when the image already has a
// "discobox", and defaults to /home/discobox when it does not, which is the same
// "ask where you can, decide only where you must" rule the rest of this follows.
var windowsRunUser = runUserIdentity{
	Name: fallbackRunUserName,
	UID:  sandboxuser.AccountIDMin,
	GID:  sandboxuser.AccountIDMin,
}

func resolveRunUserIdentity() (runUserIdentity, bool, error) {
	if runtime.GOOS == "windows" {
		return windowsRunUser, true, nil
	}
	current, err := user.Current()
	if err != nil {
		return runUserIdentity{}, false, fmt.Errorf("resolve current user: %w", err)
	}
	identity, ok := parseRunUserIdentity(current)
	return identity, ok, nil
}

// parseRunUserIdentity turns this machine's account into the one the sandbox is
// asked for. Root asks for nobody, reported by ok false. Otherwise the name and
// home travel when a Linux sandbox can use them, and each id when it is in the
// guest's account range. One that is not -- a macOS account is 501 in group 20,
// below UID_MIN, and 20 is dialout there -- is replaced: the uid by
// sandboxuser.AccountIDMin, the gid by the uid, as useradd's own private group
// would be. This is the one place such a choice is made; the API refuses the
// out-of-range id rather than making it.
func parseRunUserIdentity(current *user.User) (identity runUserIdentity, ok bool) {
	if current == nil {
		return runUserIdentity{}, false
	}
	uid, uidOK := parseRunNumericUserID(current.Uid)
	if uidOK && uid == 0 {
		return runUserIdentity{}, false
	}
	gid, gidOK := parseRunNumericUserID(current.Gid)
	identity = runUserIdentity{Name: fallbackRunUserName, UID: sandboxuser.AccountIDMin}
	if validRunUnixUserName(current.Username) {
		identity.Name = current.Username
	}
	if validRunHomeDirectory(current.HomeDir) {
		identity.HomeDirectory = current.HomeDir
	}
	if uidOK && sandboxuser.InAccountRange(uid) {
		identity.UID = uid
	}
	identity.GID = identity.UID
	if gidOK && sandboxuser.InAccountRange(gid) {
		identity.GID = gid
	}
	return identity, true
}

func parseRunNumericUserID(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil
}

func validRunUnixUserName(value string) bool {
	return runUnixUserNamePattern.MatchString(value)
}

// validRunHomeDirectory reports whether this machine's home directory is one
// the sandbox could actually use.
//
// The sandbox runs Linux, so only a POSIX absolute path means anything there.
// A home like "C:\Users\alice" is not merely useless: sending it makes the
// request carry a user whose every other field was already rejected as
// unusable, and the sandbox then fails to start because nothing in it can
// resolve a uid. Windows itself does not reach here -- it answers with
// windowsRunUser instead -- but the check is kept: it is the guard for a home
// this sandbox could not use, not a guard for one operating system.
func validRunHomeDirectory(value string) bool {
	return strings.HasPrefix(value, "/")
}

// resolveGitIdentity reads the git authorship the sandbox should commit under,
// with git's own resolution from the local source directory -- so a
// repository-local override wins over the global one (ADR 0042 §3).
//
// sourceArg is the create request's source, @REF suffix and all. A remote
// repository has no local worktree, so the read falls back to the process
// working directory, the way LocalProjectDirectory does for a remote source.
//
// An unconfigured identity is left absent. git is the authority on whether one
// is set, and $USER@$(hostname) here would only relocate the wrong fallback this
// exists to replace.
func resolveGitIdentity(ctx context.Context, sourceArg string) apimodel.SandboxGitIdentity {
	source, _, _ := splitRunSourceRef(sourceArg)
	dir := strings.TrimSpace(source)
	if dir == "" || IsRemoteGitSource(dir) {
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
	sandboxUser.SetUID(apiclientgen.NewOptInt64(u.UID))
	sandboxUser.SetGid(apiclientgen.NewOptInt64(u.GID))
	body.Config.SetUser(apiclientgen.NewOptSandboxUser(sandboxUser))
}
