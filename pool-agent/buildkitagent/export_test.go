package buildkitagent

import (
	"log/slog"
	"time"

	"google.golang.org/grpc"
)

// SetTestRoot relocates every path this package writes under dir, so tests can
// exercise the real rendering without touching the container's absolute paths.
func SetTestRoot(dir string) { testRoot = dir }

// NewTestMediator is a mediator with no upstream connection, for exercising the
// parts of it that do not forward.
func NewTestMediator(logger *slog.Logger) *Mediator { return &Mediator{logger: logger} }

// Drain stops srv the way a shutdown does, bounded by within.
func (m *Mediator) Drain(srv *grpc.Server, within time.Duration) { m.drain(srv, within) }
