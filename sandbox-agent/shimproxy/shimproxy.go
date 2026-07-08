package shimproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const defaultDialTimeout = 5 * time.Second

func StartJSON[T any](ctx context.Context, socketPath string) (T, error) {
	var zero T
	shimConn, err := Dial(ctx, socketPath, defaultDialTimeout)
	if err != nil {
		return zero, err
	}
	defer shimConn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/start", nil)
	if err != nil {
		return zero, err
	}
	if err := req.Write(shimConn); err != nil {
		return zero, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(shimConn), req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return zero, fmt.Errorf("start shim: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}

func AttachHTTPUpgrade(ctx context.Context, w http.ResponseWriter, socketPath, protocol string, replay bool) error {
	shimConn, shimReader, err := attachShim(ctx, socketPath, protocol, replay)
	if err != nil {
		return err
	}
	defer shimConn.Close()

	clientConn, clientRW, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return err
	}
	defer clientConn.Close()
	_, _ = clientRW.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + protocol + "\r\n\r\n")
	if err := clientRW.Flush(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(shimConn, clientRW)
		closeWrite(shimConn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, shimReader)
		closeWrite(clientConn)
	}()
	wg.Wait()
	return nil
}

func AttachWebSocket(ctx context.Context, w http.ResponseWriter, r *http.Request, socketPath, protocol string) error {
	shimConn, shimReader, err := attachShim(ctx, socketPath, protocol, false)
	if err != nil {
		return err
	}
	defer shimConn.Close()

	wsConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return err
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	clientConn := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)
	defer clientConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(shimConn, clientConn)
		closeWrite(shimConn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, shimReader)
		_ = clientConn.Close()
	}()
	wg.Wait()
	return nil
}

func attachShim(ctx context.Context, socketPath, protocol string, replay bool) (net.Conn, *bufio.Reader, error) {
	shimConn, err := Dial(ctx, socketPath, defaultDialTimeout)
	if err != nil {
		return nil, nil, err
	}
	attachURL := "http://unix/attach"
	if replay {
		attachURL += "?replay=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, attachURL, nil)
	if err != nil {
		_ = shimConn.Close()
		return nil, nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol)
	if err := req.Write(shimConn); err != nil {
		_ = shimConn.Close()
		return nil, nil, err
	}
	shimReader := bufio.NewReader(shimConn)
	resp, err := http.ReadResponse(shimReader, req)
	if err != nil {
		_ = shimConn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		_ = shimConn.Close()
		return nil, nil, fmt.Errorf("attach shim: %s", resp.Status)
	}
	return shimConn, shimReader, nil
}

func Dial(ctx context.Context, socketPath string, timeout time.Duration) (net.Conn, error) {
	if strings.TrimSpace(socketPath) == "" {
		return nil, fmt.Errorf("shim socket path is required")
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for shim socket")
	}
	return nil, lastErr
}

func closeWrite(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
