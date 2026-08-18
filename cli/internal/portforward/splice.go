package portforward

import (
	"context"
	"errors"
	"io"
	"net"
)

// closeWriter is the half-close a TCP conn and the tunnel conn both offer. A
// request that ends by closing its write side — an HTTP/1.0 request body, a
// protocol that streams until EOF — needs that end to travel, or the other
// side waits for bytes that are never coming.
type closeWriter interface{ CloseWrite() error }

// splice copies in both directions until both are done, half-closing each
// side as its source ends. It returns the first error that ended a direction,
// or nil when both ended cleanly; a closed connection is not an error, it is
// how a proxied connection ends.
func splice(ctx context.Context, local, remote net.Conn) error {
	done := make(chan error, 2)
	go func() { done <- copyOneWay(remote, local) }()
	go func() { done <- copyOneWay(local, remote) }()

	var first error
	for range 2 {
		select {
		case err := <-done:
			if err != nil && first == nil {
				first = err
			}
		case <-ctx.Done():
			// The caller closes both conns on the way out, which unblocks the
			// copies still running; waiting for them here would outlive the
			// command.
			return ctx.Err()
		}
	}
	return first
}

func copyOneWay(dst, src net.Conn) error {
	_, err := io.Copy(dst, src)
	if closer, ok := dst.(closeWriter); ok {
		_ = closer.CloseWrite()
	}
	if isClosed(err) {
		return nil
	}
	return err
}

func isClosed(err error) bool {
	return err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}
