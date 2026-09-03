//go:build !linux

package childproc

import (
	"context"
	"log/slog"
)

// StartReaper does nothing off Linux: the pool agent is PID 1 only in its own
// container, and nowhere else does a process inherit children it did not start.
func StartReaper(context.Context, *slog.Logger, map[int]string) func() {
	return func() {}
}
