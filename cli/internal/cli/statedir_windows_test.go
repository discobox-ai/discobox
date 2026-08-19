//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// assertPrivateToUser reads the object's actual permissions, because Windows
// has no mode to read: this user owns it, and the only principals on it are
// this user, SYSTEM and Administrators.
//
// The check that matters is that nothing was inherited. A profile that grants a
// group anything grants it on every file created under there, and OpenSSH
// refuses a config or a key any other principal can reach.
func assertPrivateToUser(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the permissions of %s: %v", path, err)
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("this process's user: %v", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("owner of %s: %v", path, err)
	}
	if !owner.Equals(user.User.Sid) {
		t.Fatalf("%s is owned by %s, want %s", path, owner, user.User.Sid)
	}

	allowed := map[string]bool{user.User.Sid.String(): true}
	for _, wellKnown := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(wellKnown)
		if err != nil {
			t.Fatalf("well-known SID %d: %v", wellKnown, err)
		}
		allowed[sid.String()] = true
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("permissions of %s: %v", path, err)
	}
	if dacl == nil {
		t.Fatalf("%s has no access list, which grants everyone everything", path)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("entry %d of %s: %v", i, path, err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !allowed[sid.String()] {
			name, _, _, err := sid.LookupAccount("")
			if err != nil {
				name = sid.String()
			}
			t.Fatalf("%s is reachable by %s, which is what makes ssh refuse it", path, name)
		}
	}
}

// State goes where Windows keeps state a program derives — %LOCALAPPDATA%, the
// one that does not roam — rather than into the XDG path this used to build by
// hand out of the home directory.
func TestStateDirIsLocalAppData(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOCALAPPDATA", `C:\Users\someone\AppData\Local`)
	if got, want := cliStateDir(), `C:\Users\someone\AppData\Local\discobox\cli`; got != want {
		t.Fatalf("state dir = %q, want %q", got, want)
	}
	// Honored here too, because nothing on Windows sets it by accident and a
	// test or a portable install may want it.
	t.Setenv("XDG_STATE_HOME", `D:\state`)
	if got, want := cliStateDir(), `D:\state\discobox\cli`; got != want {
		t.Fatalf("with XDG_STATE_HOME the state dir = %q, want %q", got, want)
	}
}

// The directory a state file lands in is private, and so is the file: a key
// written into a profile that grants a group access inherits that grant, which
// is what ssh refuses.
func TestEnsureStateDirRestrictsWhatItCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "ssh")
	if err := ensureStateDir(dir); err != nil {
		t.Fatalf("ensureStateDir: %v", err)
	}
	assertPrivateToUser(t, dir)

	file := filepath.Join(dir, "identity")
	if err := os.WriteFile(file, []byte("key"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Inherited from the directory above, so it is already private — and
	// restricting it again is what repairs a file an older run left behind.
	assertPrivateToUser(t, file)
	if err := restrictToUser(file); err != nil {
		t.Fatalf("restrictToUser: %v", err)
	}
	assertPrivateToUser(t, file)
}
