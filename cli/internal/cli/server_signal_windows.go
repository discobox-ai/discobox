//go:build windows

package cli

import "os"

// stopServerProcess kills the server, because Windows has no signal to ask
// with: os.Process.Signal accepts only Kill there, and the graceful paths that
// do exist — a console control event, a window message — need the server to be
// something other than a detached child.
//
// So the drain a SIGTERM buys elsewhere is not available here. It is the same
// outcome as the timeout that follows on every platform, reached sooner.
func stopServerProcess(p *os.Process) error { return p.Kill() }
