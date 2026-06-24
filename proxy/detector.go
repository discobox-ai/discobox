package proxy

import (
	"bytes"
	"io"
	"net"
	"time"
)

type protocol int

const (
	protocolUnknown protocol = iota
	protocolHTTP
	protocolSOCKS5
	protocolSOCKS4
)

func (p protocol) String() string {
	switch p {
	case protocolHTTP:
		return "http"
	case protocolSOCKS5:
		return "socks5"
	case protocolSOCKS4:
		return "socks4"
	default:
		return "unknown"
	}
}

type peekedConn struct {
	net.Conn
	reader io.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func detect(conn net.Conn) (protocol, *peekedConn, error) {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return protocolUnknown, nil, err
	}
	buf := make([]byte, 1)
	_, err := io.ReadFull(conn, buf)
	if clearErr := conn.SetReadDeadline(time.Time{}); clearErr != nil && err == nil {
		err = clearErr
	}
	if err != nil {
		return protocolUnknown, nil, err
	}
	peeked := &peekedConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(buf), conn)}
	switch first := buf[0]; {
	case first == 0x05:
		return protocolSOCKS5, peeked, nil
	case first == 0x04:
		return protocolSOCKS4, peeked, nil
	case first >= 'A' && first <= 'Z':
		return protocolHTTP, peeked, nil
	default:
		return protocolUnknown, peeked, nil
	}
}
