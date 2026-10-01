//go:build !darwin

package autostop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// platformPowerOff is Linux's: it asks systemd, which is PID 1 in the sandbox,
// to power it off, and returns as soon as the job is queued.
//
// It starts poweroff.target directly rather than running `systemctl poweroff`,
// which is the same job reached a worse way. `poweroff` asks logind first — the
// image has none, so every stop logs its failure before falling back to this —
// and then waits for the job, during which the shutdown it started kills this
// unit and the waiting systemctl with it. Every stop that worked was reported
// as one that failed. --no-block is what returns before that, and
// replace-irreversibly is the job mode `poweroff` itself uses, so nothing
// started afterwards can cancel the shutdown.
func platformPowerOff(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "systemctl", "start", "--no-block", "--job-mode=replace-irreversibly", "poweroff.target").CombinedOutput()
	if err != nil {
		return fmt.Errorf("start poweroff.target: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
