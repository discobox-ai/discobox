package shimproxy

import (
	"errors"

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

	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"

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
	// The http.Server's per-request read/write deadlines survive the hijack and
	// would kill this long-lived attach stream mid-session; the attach owns the
	// conn from here on, so clear them.
	_ = clientConn.SetDeadline(time.Time{})
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

// attachPingInterval paces websocket keepalive pings on an attach. The pings
// keep idle attach tunnels alive across NATs and intermediate proxies and
// detect dead peers, which an idle raw-byte tunnel never would.
const attachPingInterval = 30 * time.Second

// attachPingTimeout bounds how long a ping waits for the peer's pong before
// the connection is considered dead.
const attachPingTimeout = 10 * time.Second

func AttachWebSocket(ctx context.Context, w http.ResponseWriter, r *http.Request, socketPath, protocol string, replay bool) error {
	shimConn, shimReader, err := attachShim(ctx, socketPath, protocol, replay)
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

	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go func() {
		if err := pingWebSocket(pingCtx, wsConn); err != nil {
			// The peer is gone; close the tunnel so both copy loops unblock.
			_ = clientConn.Close()
		}
	}()

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

// AttachOneShot runs a non-interactive attach to completion: it writes stdin,
// closes the input side, and returns everything the exec emitted.
//
// A streaming attach is a raw byte tunnel — the frame protocol is spoken
// end-to-end between the remote client and the shim, and this process is only a
// pipe. A one-shot caller has no tunnel to speak over, so this is the one place
// the agent speaks the frame protocol itself, keeping that protocol from leaking
// into callers that only want "feed it bytes, give me its output".
//
// Output and error streams are returned interleaved, as the exec emitted them.
// The exit status is not returned: the exec record is authoritative for that, and
// the caller reads it there.
func AttachOneShot(ctx context.Context, socketPath, protocol string, stdin []byte) ([]byte, error) {
	shimConn, shimReader, err := attachShim(ctx, socketPath, protocol, false)
	if err != nil {
		return nil, err
	}
	defer shimConn.Close()

	if len(stdin) > 0 {
		if err := frame.Write(shimConn, frame.Input, stdin); err != nil {
			return nil, fmt.Errorf("write exec stdin: %w", err)
		}
	}
	// Close the input side so a command reading to EOF (cat, tee) terminates.
	if err := frame.Write(shimConn, frame.CloseInput, nil); err != nil {
		return nil, fmt.Errorf("close exec stdin: %w", err)
	}

	var out []byte
	for {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		f, err := frame.Read(shimReader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The shim hung up without an explicit exit frame; whatever it
				// emitted is still the exec's output.
				return out, nil
			}
			return out, err
		}
		switch f.Type {
		case frame.Output, frame.Error:
			out = append(out, f.Payload...)
		case frame.Exit:
			return out, nil
		}
	}
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

// pingWebSocket sends keepalive pings every attachPingInterval until ctx is
// canceled, returning an error when a ping goes unanswered for
// attachPingTimeout. The peer must be reading the connection for pongs to be
// processed; both ends of an attach always are.
func pingWebSocket(ctx context.Context, conn *websocket.Conn) error {
	ticker := time.NewTicker(attachPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, attachPingTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func closeWrite(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
