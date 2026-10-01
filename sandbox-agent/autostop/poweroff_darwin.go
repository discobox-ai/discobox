package autostop

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// shutdownPath is where macOS ships shutdown, named absolutely so a sandbox
// user's PATH cannot put another program in its place.
const shutdownPath = "/sbin/shutdown"

// platformPowerOff is darwin's: shutdown(8) halts the guest, which a VM's host
// sees as the machine powering off.
//
// shutdown hands the halt to launchd, and the halt it starts may end shutdown
// itself before it has exited — the trap `systemctl poweroff` sets on Linux.
// So a shutdown ended by a signal, while this context still stands, is a halt
// already under way and reported as one; only an exit with a status is
// shutdown refusing.
func platformPowerOff(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, shutdownPath, "-h", "now").CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && !exitErr.Exited() && ctx.Err() == nil {
		return nil
	}
	return fmt.Errorf("shutdown -h now: %w: %s", err, strings.TrimSpace(string(out)))
}
