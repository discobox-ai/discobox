//go:build !linux

package poolagent

import (
	"context"
	"fmt"
	"net"
	"runtime"

	"github.com/discobox-ai/vm/pkg/machine"
)

// engineDial refuses every connection: a pool's machine runs Linux, and only
// Linux marks a connection past the rule set fenceInMachine loads there.
func engineDial() machine.DialFunc {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("a pool whose driver authenticates in-machine runs on Linux, not %s", runtime.GOOS)
	}
}
