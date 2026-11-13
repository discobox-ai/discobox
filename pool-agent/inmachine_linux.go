//go:build linux

package poolagent

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/discobox-ai/vm/pkg/machine"
)

// engineDial is the pool's engine's machine.Config.Dial on a driver that
// declares in-machine auth: every connection it opens carries engineMark, the
// one way past the rule set fenceInMachine loads. It dials directly, as the
// engine would with no proxy variable set, which a pool's machine has none of.
func engineDial() machine.DialFunc {
	dialer := net.Dialer{Control: func(_, _ string, conn syscall.RawConn) error {
		var serr error
		if err := conn.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, engineMark)
		}); err != nil {
			return err
		}
		return serr
	}}
	return dialer.DialContext
}
