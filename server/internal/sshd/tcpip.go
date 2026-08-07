package sshd

import (
	"context"

	"golang.org/x/crypto/ssh"
)

// handleDirectTCPIPChannel serves an SSH direct-tcpip channel (ssh -L / -D)
// by dialing host:port from inside the target sandbox's network namespace
// (ADR 0024 §3). Implemented in M5.
func (s *Server) handleDirectTCPIPChannel(_ context.Context, newChannel ssh.NewChannel, _, _ string) {
	_ = newChannel.Reject(ssh.Prohibited, "direct-tcpip is not yet implemented")
}
