//go:build !linux

package server

import (
	"errors"
	"net"
)

// peerUID is not read here yet — on a macOS or Windows sandbox (ADR 0145 §5)
// — so the local socket refuses everyone there, and the in-box AHP host has
// no execs to serve.
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials are read only on Linux")
}
