package cli

import (
	"fmt"
	"os/user"
	"regexp"
	"strconv"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
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

func (u runUserIdentity) setCreateSandboxUser(body *apimodel.CreateSandboxBody) {
	sandboxUser := apimodel.SandboxUser{}
	sandboxUser.SetName(optString(u.Name))
	sandboxUser.SetHomeDirectory(optString(u.HomeDirectory))
	if u.IDsUsable {
		sandboxUser.SetUID(apiclientgen.NewOptInt64(u.UID))
		sandboxUser.SetGid(apiclientgen.NewOptInt64(u.GID))
	}
	body.Config.SetUser(apiclientgen.NewOptSandboxUser(sandboxUser))
}
