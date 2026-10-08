//go:build linux

package intake

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// unitActionTimeout bounds one systemctl call. A restart waits for the unit's
// start job, which for a bridge is a fork and for the trust store a few file
// writes; a delivery is not held up longer than this by any one of them.
const unitActionTimeout = 30 * time.Second

// applyUnitAction asks systemd, PID 1 in the sandbox, to restart or stop a
// unit, and waits for the job, so a restarted unit is up before readiness is
// published.
func applyUnitAction(ctx context.Context, action unitAction) error {
	ctx, cancel := context.WithTimeout(ctx, unitActionTimeout)
	defer cancel()
	verb := "restart"
	if action.stop {
		verb = "stop"
	} else if action.unit.withDocker && !systemdUnitActive(ctx, "docker.service") {
		// dockerd brings this one up itself when it starts, from the files
		// that are now in place.
		return nil
	}
	//nolint:gosec // G204: the verb and unit name come from proxyUnits, a fixed list.
	out, err := exec.CommandContext(ctx, "systemctl", verb, action.unit.name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", verb, action.unit.name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func systemdUnitActive(ctx context.Context, unit string) bool {
	return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", unit).Run() == nil
}
