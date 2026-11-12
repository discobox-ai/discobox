package autostop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// shutdownPath is where macOS ships shutdown, and sudoPath sudo, named
// absolutely so a sandbox user's PATH cannot put another program in their
// place.
const (
	shutdownPath = "/sbin/shutdown"
	sudoPath     = "/usr/bin/sudo"
)

// shutdownCommand is how this process halts the guest. shutdown(8) is root's
// (its operator group aside), and a macOS sandbox's agent runs as the
// sandbox's account (the drop-to-account flag), which is given sudo, as a
// Linux sandbox's user is: so an agent that is not root asks sudo, never
// waiting on a password (-n). An account without sudo cannot halt the guest
// itself; its pool still can, once it holds the sandbox's power.
func shutdownCommand(euid int) []string {
	if euid == 0 {
		return []string{shutdownPath, "-h", "now"}
	}
	return []string{sudoPath, "-n", shutdownPath, "-h", "now"}
}

// platformPowerOff is darwin's: shutdown(8) halts the guest, which a VM's host
// sees as the machine powering off.
//
// shutdown hands the halt to launchd, and the halt it starts may end shutdown
// itself before it has exited — the trap `systemctl poweroff` sets on Linux.
// So a shutdown ended by a signal, while this context still stands, is a halt
// already under way and reported as one; only an exit with a status is
// shutdown refusing.
func platformPowerOff(ctx context.Context) error {
	argv := shutdownCommand(os.Geteuid())
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput() //nolint:gosec // G204: fixed programs, by absolute path.
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && !exitErr.Exited() && ctx.Err() == nil {
		return nil
	}
	return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
}
