//go:build !linux

package intake

import "context"

// applyUnitAction does nothing where the sandbox has no systemd: what runs the
// bridges there is that platform's supervision, which starts them itself.
func applyUnitAction(context.Context, unitAction) error {
	return nil
}
