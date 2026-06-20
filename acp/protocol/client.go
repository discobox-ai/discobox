// Package protocol implements a minimal ACP newline-delimited JSON-RPC client.
package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const ProtocolVersion = 1

// Client speaks ACP over an agent subprocess's stdio.
type Client struct {
	conn mcp.Connection

	mu      sync.Mutex
	nextID  atomic.Int64
	pending map[string]chan *jsonrpc.Response
	done    chan error
}

// New connects transport and returns an ACP client over its JSON-RPC stream.
func New(ctx context.Context, transport mcp.Transport) (*Client, error) {
	conn, err := transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, pending: map[string]chan *jsonrpc.Response{}, done: make(chan error, 1)}
	go c.readLoop()
	return c, nil
}

// Close closes the underlying transport.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Initialize negotiates ACP protocol version and capabilities.
func (c *Client) Initialize(ctx context.Context) (*InitializeResponse, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  false,
				"writeTextFile": false,
			},
			"terminal": false,
		},
		"clientInfo": map[string]any{
			"name":    "discobox-acp",
			"version": "0.0.0",
		},
	}
	var out InitializeResponse
	if err := c.call(ctx, "initialize", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSessions calls session/list.
func (c *Client) ListSessions(ctx context.Context, cwd, cursor string) (*ListSessionsResponse, error) {
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var out ListSessionsResponse
	if err := c.call(ctx, "session/list", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := fmt.Sprintf("%d", c.nextID.Add(1))
	rpcID, err := jsonrpc.MakeID(id)
	if err != nil {
		return err
	}
	ch := make(chan *jsonrpc.Response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, &jsonrpc.Request{ID: rpcID, Method: method, Params: rawParams}); err != nil {
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out == nil {
			return nil
		}
		if len(resp.Result) == 0 {
			return fmt.Errorf("json-rpc response missing result")
		}
		return json.Unmarshal(resp.Result, out)
	case err := <-c.done:
		if err == nil {
			return io.EOF
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) readLoop() {
	for {
		msg, err := c.conn.Read(context.Background())
		if err != nil {
			c.done <- err
			return
		}
		resp, ok := msg.(*jsonrpc.Response)
		if !ok {
			continue
		}
		id, ok := resp.ID.Raw().(string)
		if !ok {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

// EncodeMessage exposes the official MCP SDK JSON-RPC encoder for tests and
// small transport adapters.
func EncodeMessage(msg jsonrpc.Message) ([]byte, error) {
	return jsonrpc.EncodeMessage(msg)
}

// DecodeMessage exposes the official MCP SDK JSON-RPC decoder for tests and
// small transport adapters.
func DecodeMessage(data []byte) (jsonrpc.Message, error) {
	return jsonrpc.DecodeMessage(data)
}

// InitializeResponse is the subset of ACP initialize result used by this module.
type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []any             `json:"authMethods,omitempty"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
}

// Implementation identifies an ACP client or agent implementation.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// AgentCapabilities is the subset needed for feature gating.
type AgentCapabilities struct {
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
}

// SessionCapabilities exposes optional session methods.
type SessionCapabilities struct {
	List   *map[string]any `json:"list,omitempty"`
	Delete *map[string]any `json:"delete,omitempty"`
	Resume *map[string]any `json:"resume,omitempty"`
}

// SupportsSessionList reports whether session/list is advertised.
func (r InitializeResponse) SupportsSessionList() bool {
	return r.AgentCapabilities.SessionCapabilities.List != nil
}

// ListSessionsResponse is the ACP session/list result.
type ListSessionsResponse struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor *string       `json:"nextCursor,omitempty"`
}

// SessionInfo is one ACP listed session.
type SessionInfo struct {
	SessionID             string   `json:"sessionId"`
	CWD                   string   `json:"cwd"`
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
	Title                 *string  `json:"title,omitempty"`
}
