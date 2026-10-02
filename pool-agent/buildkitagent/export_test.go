package buildkitagent

import (
	"log/slog"
	"time"

	"google.golang.org/grpc"
)

// NewTestMediator is a mediator with no upstream connection, for exercising the
// parts of it that do not forward.
func NewTestMediator(logger *slog.Logger) *Mediator { return &Mediator{logger: logger} }

// Drain stops srv the way a shutdown does, bounded by within.
func (m *Mediator) Drain(srv *grpc.Server, within time.Duration) { m.drain(srv, within) }
