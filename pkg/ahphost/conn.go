package ahphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/websocket"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// outboxSize is how many messages a client may fall behind by. A client that
// falls further is closed rather than waited on: one stalled reader must not
// hold up the host every other client is reading.
const outboxSize = 4096

// conn is one client's WebSocket: JSON-RPC 2.0, one message per text frame.
type conn struct {
	h      *Host
	ws     *websocket.Conn
	out    chan []byte
	cancel context.CancelFunc
	ctx    context.Context

	// clientID is what initialize named, guarded by h.mu.
	clientID string
}

func newConn(h *Host, ws *websocket.Conn) *conn {
	return &conn{h: h, ws: ws, out: make(chan []byte, outboxSize)}
}

// rpcError is a JSON-RPC error a handler answers with.
type rpcError struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

func errorf(code int32, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// incoming is any message a client sends. A request has an id; a notification
// does not. The id is echoed back as it came, number or string.
type incoming struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// serve reads the client's messages until it goes. Messages are handled in
// the order they came — a subscribe answered before the action sent after it
// — and a handler that has to wait on a server answers from a goroutine of its
// own (handleRequest), so a slow server never holds up a ping.
func (c *conn) serve(ctx context.Context) {
	c.ctx, c.cancel = context.WithCancel(ctx)
	defer c.cancel()
	go c.write()
	for {
		_, data, err := c.ws.Read(c.ctx)
		if err != nil {
			return
		}
		var msg incoming
		if err := json.Unmarshal(data, &msg); err != nil || msg.Method == "" {
			// A response to a request this host never sends, or noise.
			continue
		}
		if len(msg.ID) == 0 {
			c.h.handleNotification(c, msg.Method, msg.Params)
			continue
		}
		c.h.handleRequest(c, msg.ID, msg.Method, msg.Params)
	}
}

// write sends what the host queued, in order.
func (c *conn) write() {
	for {
		select {
		case <-c.ctx.Done():
			_ = c.ws.Close(websocket.StatusNormalClosure, "")
			return
		case data := <-c.out:
			if err := c.ws.Write(c.ctx, websocket.MessageText, data); err != nil {
				c.cancel()
				return
			}
		}
	}
}

// send queues one message. It never blocks: a client too far behind is
// closed.
func (c *conn) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		c.h.cfg.Logf("ahp: encode %T: %v", v, err)
		return
	}
	select {
	case c.out <- data:
	default:
		c.h.cfg.Logf("ahp: closing a client that fell %d messages behind", outboxSize)
		c.cancel()
	}
}

func (c *conn) notify(method string, params any) {
	c.send(notification{JSONRPC: "2.0", Method: method, Params: params})
}

// reply answers a request with its result, or with err.
func (c *conn) reply(id json.RawMessage, result any, err error) {
	res := response{JSONRPC: "2.0", ID: id}
	if err != nil {
		var rerr *rpcError
		if !errors.As(err, &rerr) {
			rerr = &rpcError{Code: ahptypes.ErrorCodeInternalError, Message: err.Error()}
		}
		res.Error = rerr
	} else {
		if result == nil {
			result = struct{}{}
		}
		res.Result = result
	}
	c.send(res)
}
