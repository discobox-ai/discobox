//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// defaultStateHome is the XDG default: ~/.local/state.
func defaultStateHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state")
}
