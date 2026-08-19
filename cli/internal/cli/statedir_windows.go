//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// stateHome is %LOCALAPPDATA%, which is where Windows keeps per-machine state a
// program derives — as against %APPDATA%, which roams with the user to other
// machines. None of this should follow anyone anywhere: an SSH identity is this
// machine's, and a path to a generated ssh_config on this disk means nothing on
// another one.
func stateHome() string {
	if value := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); value != "" {
		return value
	}
	// UserCacheDir is %LOCALAPPDATA% too, and knows the ways of finding it that
	// do not go through the environment.
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "AppData", "Local")
	}
	return ""
}

// restrictToUser replaces everything path inherited with an explicit list: this
// user, SYSTEM, and Administrators. Nobody else, and nothing from the parent.
//
// A Windows file has no mode bits. os.WriteFile's 0600 sets the read-only
// attribute at most and never touches the ACL, so a file lands with whatever
// its parent directory grants — and a profile that grants a group anything (a
// sandbox group, a management agent, a shared machine's setup) grants it on
// every key written under there. OpenSSH checks, refuses to read a config or a
// private key any other principal can reach, and reports "Bad owner or
// permissions" without saying who put it there.
//
// PROTECTED_DACL_SECURITY_INFORMATION is the part that matters: it detaches the
// object from its parent's inheritance rather than adding to what is already
// on it.
func restrictToUser(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	// Administrators, because Windows gives it access to everything anyway and
	// OpenSSH accepts it: leaving it off would remove nobody's reach and cost
	// the user their own recovery path.
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}

	// Inheritance is a directory's business. A file that claimed to pass
	// permissions to children it cannot have is a flag Windows tolerates and
	// nothing reads.
	inheritance := uint32(windows.NO_INHERITANCE)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{
		fullAccess(user.User.Sid, inheritance),
		fullAccess(system, inheritance),
		fullAccess(admins, inheritance),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	// The owner is set with it: OpenSSH checks that too, and a file this user
	// created but does not own is one it refuses for the other reason.
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid, nil, acl, nil)
}

func fullAccess(sid *windows.SID, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
