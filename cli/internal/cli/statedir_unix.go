//go:build !windows

package cli

import (
	"os"
	"path/filepath"
)

// stateHome is the XDG default: ~/.local/state.
func stateHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state")
}

// restrictToUser is what the mode bits already did. A file created 0600 and a
// directory created 0700 are this user's alone, so there is nothing to repair.
func restrictToUser(string) error { return nil }
