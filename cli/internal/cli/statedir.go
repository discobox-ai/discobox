package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The CLI's own state — the picker's memory, this machine's iroh identity, the
// SSH identity and the generated ssh_config files — lives in one directory per
// platform's convention for state a program derives rather than a user
// configures.
//
// It is not configuration: nothing here is written by hand, and losing it costs
// a re-derivation rather than a setting. XDG_STATE_HOME names it where it is
// set — including on Windows, where nothing sets it by accident and a test or a
// portable install may want to.

// discoboxStateDir is the root of this machine's Discobox state, which the
// CLI's own state and the server binaries it stages are both under. Empty when
// this machine has nowhere to put it — see the callers, which each have their
// own last resort.
func discoboxStateDir() string {
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		return filepath.Join(value, "discobox")
	}
	if home := stateHome(); home != "" {
		return filepath.Join(home, "discobox")
	}
	return ""
}

// cliStateDir is where the CLI keeps its own state.
func cliStateDir() string {
	if root := discoboxStateDir(); root != "" {
		return filepath.Join(root, "cli")
	}
	// Somewhere rather than nowhere: a picker that cannot remember is a
	// smaller failure than a command that cannot run.
	//
	// This exact name, unchanged. What is under here is not only the picker's
	// memory: it is also this machine's iroh identity — the peer ID an
	// operator enrolled against a server — and its SSH key. An install that
	// reaches this branch (no HOME under a systemd unit, a container, a cron
	// job) would otherwise come back from an upgrade with a new identity and
	// an enrolment that no longer matches, for no reason but a tidier path.
	return filepath.Join(os.TempDir(), "discobox-cli-state")
}

// stagedServerRoot holds one directory per platform per staged server version
// (ADR 0099). A sibling of the CLI's own state rather than a subdirectory of
// it: what is staged there is another program, and the CLI is only the thing
// that fetched it.
func stagedServerRoot() string {
	if root := discoboxStateDir(); root != "" {
		return filepath.Join(root, "server")
	}
	return filepath.Join(os.TempDir(), "discobox-server-state")
}

// ensureStateDir creates a directory under the state directory and restricts it
// to this user.
//
// The two go together deliberately. On Windows a new directory inherits its
// parent's permissions and the mode argument does nothing, so a state directory
// is only private if something makes it private — and the private key inside is
// the reason it has to be. See restrictToUser.
func ensureStateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := restrictToUser(path); err != nil {
		return fmt.Errorf("restrict %s to this user: %w", path, err)
	}
	return nil
}
