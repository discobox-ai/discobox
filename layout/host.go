package layout

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/adrg/xdg"
)

// HostStateName is the directory under the user's data directory that a host
// pool keeps its state in: a sibling of what the server keeps there, under the
// same "discobox" directory the server's own data defaults to.
const HostStateName = "pool-agent"

// Host is the root of a pool whose agent runs on this machine (ADR 0144 §1):
// the user's data directory exactly as the server resolves its own, through
// xdg.DataHome — XDG_DATA_HOME where it is set, and otherwise ~/Library/
// Application Support on macOS, the LocalAppData known folder on Windows, and
// ~/.local/share elsewhere. Resolving it the same way rather than by a copy of
// the rules keeps the two from ever disagreeing about where "the data
// directory" is.
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
	dir, err := hostState()
	if err != nil {
		return Root{}, err
	}
	return Root{state: dir, host: true, native: true}, nil
}

// hostState is Host's directory, without the privilege check.
func hostState() (string, error) {
	// xdg always answers, but an answer this cannot use as a root is refused
	// here rather than resolved against the working directory.
	if xdg.DataHome == "" || !filepath.IsAbs(xdg.DataHome) {
		return "", fmt.Errorf("no data directory for a host pool's state (resolved %q)", xdg.DataHome)
	}
	return filepath.Join(xdg.DataHome, "discobox", HostStateName), nil
}
