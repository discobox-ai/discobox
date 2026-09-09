//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// stopServerProcess asks the server to shut down the way its own signal
// handler expects (server/cmd/discobox-server/main.go installs
// signal.NotifyContext for SIGINT and SIGTERM), so it drains its providers and
// releases the data directory's singleton lock rather than dying holding both.
func stopServerProcess(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
