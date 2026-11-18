package vzvm

import (
	"net"
	"sync/atomic"
)

// yamuxVersion is the first byte of every yamux frame: the protocol version a
// framed guest speaks (ADR 26-11-17-182).
const yamuxVersion = 0

// unframedCheck is the raw connection under a framed one the host dialed. It
// calls report once if the guest's first byte is not a yamux frame's. A guest
// whose services do not frame answers in plain bytes, which the session ends
// on without a word, so its consumer would see only EOF; this is what names
// the guest instead (ADR 26-11-17-182 §5). It carries net.Conn's methods and
// nothing more.
type unframedCheck struct {
	net.Conn
	checked atomic.Bool
	report  func()
}

func (c *unframedCheck) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.checked.CompareAndSwap(false, true) && b[0] != yamuxVersion {
		c.report()
	}
	return n, err
}
