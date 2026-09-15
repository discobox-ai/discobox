//go:build !windows

package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// stateHome is the XDG default: ~/.local/state.
func stateHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state")
}

// discoboxRuntimeDir is the runtime directory a local server and its providers
// put their sockets, locks and VM runtime files in: the directory
// endpoint.DefaultEndpoint's socket is in, and the one libkrun's runtime
// directories are beside it in. Without XDG_RUNTIME_DIR both fall back to a
// per-user directory in the temporary directory, and that whole directory is
// Discobox's.
func discoboxRuntimeDir() string {
	if value := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); value != "" {
		return filepath.Join(value, "discobox")
	}
	return filepath.Join(os.TempDir(), "discobox-"+strconv.Itoa(os.Getuid()))
}

// ownedByThisUser reports whether this user owns the file info describes.
func ownedByThisUser(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(stat.Uid) == os.Getuid()
}

// restrictToUser is what the mode bits already did. A file created 0600 and a
// directory created 0700 are this user's alone, so there is nothing to repair.
func restrictToUser(string) error { return nil }
