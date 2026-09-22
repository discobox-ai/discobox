//go:build !darwin

package vsock

import (
	"net"

	mdvsock "github.com/mdlayher/vsock"
)

func listen(cid, port uint32) (net.Listener, error) {
	return mdvsock.ListenContextID(cid, port, nil)
}

func dial(cid, port uint32) (net.Conn, error) {
	return mdvsock.Dial(cid, port, nil)
}
