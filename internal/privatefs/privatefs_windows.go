//go:build windows

package privatefs

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Restrict replaces everything path inherited with an explicit list: this
// user, SYSTEM, and Administrators. Nobody else, and nothing from the parent.
//
// A profile that grants a group anything (a sandbox group, a management agent,
// a shared machine's setup) grants it on every key written under there.
// OpenSSH checks, refuses to read a config or a private key any other
// principal can reach, and reports "Bad owner or permissions" without saying
// who put it there.
//
// PROTECTED_DACL_SECURITY_INFORMATION is the part that matters: it detaches the
// object from its parent's inheritance rather than adding to what is already
// on it.
func Restrict(path string) error {
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

// Check reads the object's actual permissions, because Windows has no mode to
// read: this user owns it, and the only principals on it are this user, SYSTEM
// and Administrators. It reports an error naming the first principal that
// breaks that.
//
// The check that matters is that nothing was inherited. A profile that grants
// a group anything grants it on every file created under there.
func Check(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the permissions of %s: %w", path, err)
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("this process's user: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("owner of %s: %w", path, err)
	}
	allowed := map[string]bool{user.User.Sid.String(): true}
	var admins *windows.SID
	for _, wellKnown := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(wellKnown)
		if err != nil {
			return fmt.Errorf("well-known SID %d: %w", wellKnown, err)
		}
		if wellKnown == windows.WinBuiltinAdministratorsSid {
			admins = sid
		}
		allowed[sid.String()] = true
	}

	// An elevated process creates objects owned by the Administrators group
	// rather than by the user, under the "default owner for objects created by
	// members of the Administrators group" policy — which is how GitHub's
	// Windows runners are set up. The DACL check below already treats that
	// group as allowed, and for the same reason: on such an account it is not
	// a wider audience than the user.
	if !owner.Equals(user.User.Sid) && !owner.Equals(admins) {
		return fmt.Errorf("%s is owned by %s, want %s or %s", path, owner, user.User.Sid, admins)
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("permissions of %s: %w", path, err)
	}
	if dacl == nil {
		return fmt.Errorf("%s has no access list, which grants everyone everything", path)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("entry %d of %s: %w", i, path, err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !allowed[sid.String()] {
			name, _, _, err := sid.LookupAccount("")
			if err != nil {
				name = sid.String()
			}
			return fmt.Errorf("%s is reachable by %s", path, name)
		}
	}
	return nil
}
