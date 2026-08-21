// Package server exposes the Discobox control-plane server runtime.
package server

import (
	"context"

	internalserver "github.com/discobox-ai/discobox/server/internal/server"
)

// Run loads configuration, initializes storage and services, and starts the
// HTTP server.
func Run(ctx context.Context) error {
	return internalserver.Run(ctx)
}
