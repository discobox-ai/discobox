//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultStateHome is %LOCALAPPDATA%, which is where Windows keeps state a
// program derives for one machine — as against %APPDATA%, which roams to
// others. A session's socket, lock and hook database describe processes on this
// machine and mean nothing on another.
//
// The XDG path this used to build by hand — ~/.local/state — is a Unix
// convention that no Windows tool looks in and nothing there cleans up.
func defaultStateHome() string {
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
