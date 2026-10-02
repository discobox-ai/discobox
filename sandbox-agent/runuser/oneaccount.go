package runuser

import (
	"fmt"
	osuser "os/user"
	"strings"

	"github.com/discobox-ai/discobox/sandboxuser"
)

// currentOSUser is who this process is, as the OS names it. It is a variable
// for the reason the lookups in runuser.go are: a test supplies a fixed
// account instead of whoever runs it.
var currentOSUser = osuser.Current

// currentAccount is the image layer where a sandbox has one account: this
// process's own account, by name and home. The agent runs as that account, and
// every process it starts inherits it, so who this process is is the whole
// answer (ADR 0145 §5).
//
// It carries no ids. A macOS uid is the OS's to give its account and is
// nothing a request may name, and a Windows one is a SID, which no field of
// User can hold.
func currentAccount() *User {
	found, err := currentOSUser()
	if err != nil {
		return nil
	}
	return &User{Name: accountName(found.Username), HomeDirectory: strings.TrimSpace(found.HomeDir)}
}

// accountName is the account part of an OS user name. Windows qualifies one
// with its domain or machine, DESKTOP\ada, and a manifest names the account
// alone.
func accountName(username string) string {
	username = strings.TrimSpace(username)
	if i := strings.LastIndexByte(username, '\\'); i >= 0 {
		return username[i+1:]
	}
	return username
}

// resolveOneAccount is Resolve where a sandbox on goos has one account and no
// POSIX ids. It completes nothing, because there is nothing to complete: the
// account is the agent's own (the image layer), the manifest names that same
// account and nothing more, and a request may at most name it again. Anything
// else a layer names is refused with a *sandboxuser.OneAccountError saying
// why, never ignored, since a process that asked to run as somebody and ran as
// somebody else has done the wrong thing whatever it printed.
//
// The ids and the group set are not fields an identity has here. Asking for
// one is an *UnresolvedError naming it, which is how a caller that would call
// setuid finds out it is on the wrong platform rather than receiving a zero.
func resolveOneAccount(goos string, l Layers, need Fields) (User, error) {
	for _, layer := range []*User{l.Request, l.Manifest, l.Image} {
		if err := layer.Validate(); err != nil {
			return User{}, err
		}
	}
	var account, home string
	if l.Image != nil {
		account = strings.TrimSpace(l.Image.Name)
		home = strings.TrimSpace(l.Image.HomeDirectory)
	}

	// The manifest names the account the template provisioned. The agent runs
	// as it, so the two are one account or the sandbox was assembled wrong --
	// and nothing here can switch to another account to make up for it.
	if named := strings.TrimSpace(nameOf(l.Manifest)); named != "" {
		if account == "" {
			account = named
		} else if named != account {
			return User{}, fmt.Errorf("the sandbox's manifest names account %q, but the sandbox agent runs as %q: a %s sandbox runs every process as the agent's own account", named, account, goos)
		}
	}
	if err := l.Manifest.ValidateOneAccount(goos, account); err != nil {
		return User{}, fmt.Errorf("the sandbox's manifest: %w", err)
	}
	if err := l.Request.ValidateOneAccount(goos, account); err != nil {
		return User{}, err
	}

	for _, field := range []Fields{sandboxuser.FieldUID, sandboxuser.FieldGID, sandboxuser.FieldGroups} {
		if need.Has(field) {
			return User{}, sandboxuser.Unresolved(field,
				fmt.Sprintf("a %s sandbox has no POSIX ids; it runs as its one account", goos))
		}
	}
	if need.Has(sandboxuser.FieldName) && account == "" {
		return User{}, sandboxuser.Unresolved(sandboxuser.FieldName,
			"neither the manifest nor this process names the sandbox's account")
	}
	if need.Has(sandboxuser.FieldHome) && home == "" {
		return User{}, sandboxuser.Unresolved(sandboxuser.FieldHome,
			fmt.Sprintf("the account %q this process runs as has no home directory", account))
	}
	user := User{Name: account, HomeDirectory: home}
	clearUnrequested(&user, need)
	return user, nil
}

func nameOf(u *User) string {
	if u == nil {
		return ""
	}
	return u.Name
}
