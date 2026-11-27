//go:build !unix

package handover

import (
	"context"
	"net"
)

// DrainRequested is never closed where no pool's units can ask for a drain.
func DrainRequested(context.Context) <-chan struct{} { return make(chan struct{}) }

// Listeners are none where systemd passes none.
func Listeners() (map[string]net.Listener, error) { return map[string]net.Listener{}, nil }
