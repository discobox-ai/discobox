//go:build windows

package privatefstest

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// Widen replaces path's permissions with this user's full control and read
// for BUILTIN\Users, inherited by anything created beneath a directory: the
// profile that grants a group access which privatefs.Restrict exists for.
func Widen(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("this process's user: %v", err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatalf("BUILTIN\\Users SID: %v", err)
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		grant(user.User.Sid, windows.GENERIC_ALL, inheritance),
		grant(users, windows.GENERIC_READ, inheritance),
	}, nil)
	if err != nil {
		t.Fatalf("build the access list for %s: %v", path, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatalf("widen %s: %v", path, err)
	}
}

func grant(sid *windows.SID, access windows.ACCESS_MASK, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: access,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
