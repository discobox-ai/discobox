//go:build !unix

package handover

import (
	"context"
	"net"
)

// DrainRequested is never requested where no pool's units can ask for a
// drain.
func DrainRequested(context.Context, string, func(error)) *Drain {
	return &Drain{requested: make(chan struct{})}
}

// Listeners are none where systemd passes none.
func Listeners() (map[string]net.Listener, error) { return map[string]net.Listener{}, nil }
