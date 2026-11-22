package proxyagent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// connectThrough opens a tunnel to address over conn, a connection to an
// HTTP proxy, with CONNECT, and returns the tunnel: conn itself, or conn
// with what the proxy sent past its answer in front of it.
func connectThrough(ctx context.Context, conn net.Conn, address string) (net.Conn, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	req := &http.Request{Method: http.MethodConnect}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", address, address); err != nil {
		return nil, fmt.Errorf("CONNECT %s through the pool's proxy: %w", address, err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, fmt.Errorf("CONNECT %s through the pool's proxy: %w", address, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CONNECT %s through the pool's proxy: %s", address, resp.Status)
	}
	if reader.Buffered() == 0 {
		return conn, nil
	}
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// bufferedConn is a connection whose first bytes were read into reader.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
