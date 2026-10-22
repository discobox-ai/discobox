package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/pkg/execstream"
	"github.com/discobox-ai/discobox/pkg/execstream/client"
)

// Client calls the socket.
type Client struct {
	http *http.Client
}

// NewClient is a client of the socket at path.
func NewClient(path string) *Client {
	dialer := &net.Dialer{}
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		},
	}}}
}

// Box describes the discobox.
func (c *Client) Box(ctx context.Context) (Box, error) {
	var out Box
	return out, c.do(ctx, http.MethodGet, "/box", nil, &out)
}

// Execs lists the execs the socket serves.
func (c *Client) Execs(ctx context.Context) ([]Exec, error) {
	var out []Exec
	return out, c.do(ctx, http.MethodGet, "/execs", nil, &out)
}

// Create creates a shell or a harness terminal, not yet started.
func (c *Client) Create(ctx context.Context, req CreateRequest) (Exec, error) {
	var out Exec
	return out, c.do(ctx, http.MethodPost, "/execs", req, &out)
}

// Start starts a created exec.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/execs/"+url.PathEscape(id)+"/start", nil, nil)
}

// Input types text into an exec and presses Enter.
func (c *Client) Input(ctx context.Context, id, text string) error {
	return c.do(ctx, http.MethodPost, "/execs/"+url.PathEscape(id)+"/input", InputRequest{Text: text}, nil)
}

// Delete ends an exec.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/execs/"+url.PathEscape(id), nil, nil)
}

// Attach attaches to an exec, replaying its screen. The attach lives as long
// as ctx does.
func (c *Client) Attach(ctx context.Context, id string) (*client.Terminal, error) {
	conn, resp, err := websocket.Dial(ctx, "ws://localexec/execs/"+url.PathEscape(id)+"/attach?replay=true", &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
			return nil, responseError(resp)
		}
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	conn.SetReadLimit(-1)
	netConn := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	return client.NewTerminal(execstream.StreamConn(netConn)), nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localexec"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return responseError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error != "" {
		return fmt.Errorf("%s: %s", resp.Status, body.Error)
	}
	return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
}
