package sshd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// dialFrameWebSocket dials a websocket at u (already built via
// sandboxagentclient.TargetURL) using client, converting an http(s) scheme to
// ws(s), and wraps the result for frame read/write. Shared by the
// session-channel exec attach and the direct-tcpip tunnel attach: both are
// "dial a websocket at the pool-agent target and speak execstream/frame over
// it" (ADR 0024 §§1, 4).
func dialFrameWebSocket(ctx context.Context, client *http.Client, u *url.URL, what string) (*frameConn, error) {
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	wsConn, wsResp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		if wsResp != nil && wsResp.Body != nil {
			defer wsResp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(wsResp.Body, 64*1024))
			return nil, fmt.Errorf("%s: %s: %s", what, wsResp.Status, strings.TrimSpace(string(data)))
		}
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	netConn := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)
	return &frameConn{conn: netConn}, nil
}
