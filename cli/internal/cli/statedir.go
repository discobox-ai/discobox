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

// cliStateDir is where the CLI keeps its own state.
func cliStateDir() string {
	if value := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); value != "" {
		return filepath.Join(value, "discobox", "cli")
	}
	if home := stateHome(); home != "" {
		return filepath.Join(home, "discobox", "cli")
	}
	// Somewhere rather than nowhere: a picker that cannot remember is a
	// smaller failure than a command that cannot run.
	return filepath.Join(os.TempDir(), "discobox-cli-state")
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
