//go:build windows

package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// discoboxRuntimeDir is empty: a Windows server listens on a named pipe, so
// there is no socket directory. Its launch lock is in the temporary directory,
// which tempDirLeftovers looks through; wslc falls back to the temporary
// directory too, but only when LOCALAPPDATA is unset, which it never is for a
// Windows user.
func discoboxRuntimeDir() string { return "" }

// ownedByThisUser is true: the temporary directory is in the user's own
// profile on Windows.
func ownedByThisUser(fs.FileInfo) bool { return true }
