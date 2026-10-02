package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// HostStateName is the directory under the user's data directory that a host
// pool keeps its state in: a sibling of what the server keeps there, under the
// same "discobox" directory the server's own data defaults to.
const HostStateName = "pool-agent"

// Host is the root of a pool whose agent runs on this machine (ADR 0144 §1):
// the user's data directory, by the convention the server already uses for its
// own data — XDG_DATA_HOME where it is set, and otherwise ~/Library/Application
// Support on macOS, %LOCALAPPDATA% on Windows, and ~/.local/share elsewhere.
//
// It is the user's own directory, so state is owned by the user the agent runs
// as. That is also why a privileged process is refused rather than given root's
// directory: a host pool's agent never runs as root or an administrator (ADR
// 0144 §6), and state it wrote that way would be unreadable by the agent that
// should own it.
func Host() (Root, error) {
	if privileged() {
		return Root{}, errors.New("a host pool's state belongs to the user its agent runs as, and this process is privileged")
	}
	home, _ := os.UserHomeDir()
	dir, err := hostState(runtime.GOOS, os.Getenv, home)
	if err != nil {
		return Root{}, err
	}
	if !filepath.IsAbs(dir) {
		return Root{}, fmt.Errorf("host state directory %q is not absolute", dir)
	}
	return Root{state: dir, host: true, native: true}, nil
}

// hostState resolves Host's directory for goos. It takes the platform and its
// environment as arguments, and builds the path with goos's own separator, so
// each platform's answer is testable on any of them.
func hostState(goos string, getenv func(string) string, home string) (string, error) {
	sep := "/"
	if goos == "windows" {
		sep = `\`
	}
	join := func(base string, elem ...string) string {
		return strings.Join(append([]string{strings.TrimRight(base, `/\`)}, elem...), sep)
	}
	home = strings.TrimSpace(home)

	base := strings.TrimSpace(getenv("XDG_DATA_HOME"))
	if base == "" {
		switch goos {
		case "darwin":
			if home != "" {
				base = join(home, "Library", "Application Support")
			}
		case "windows":
			// %LOCALAPPDATA% rather than %APPDATA%: a pool's state is this
			// machine's, and nothing in it should roam to another one.
			base = strings.TrimSpace(getenv("LOCALAPPDATA"))
			if base == "" && home != "" {
				base = join(home, "AppData", "Local")
			}
		default:
			if home != "" {
				base = join(home, ".local", "share")
			}
		}
	}
	if base == "" {
		return "", errors.New("no data directory for a host pool's state: set XDG_DATA_HOME, or run as a user with a home directory")
	}
	return join(base, "discobox", HostStateName), nil
}
