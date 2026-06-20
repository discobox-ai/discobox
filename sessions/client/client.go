package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	sessions "github.com/obot-platform/discobox/sessions"
	sessionapigen "github.com/obot-platform/discobox/sessions/api/gen"
)

const defaultTimeout = 10 * time.Second

var ErrNotRunning = errors.New("session daemon not running")

type Client struct {
	socketPath string
	httpClient *http.Client
	baseURL    string
	generated  *sessionapigen.Client
}

func New(socketPath string) *Client {
	c := &Client{socketPath: socketPath, baseURL: "http://unix", httpClient: unixHTTPClient(socketPath, defaultTimeout)}
	_ = c.resetGeneratedClient()
	return c
}

func (c *Client) SocketPath() string { return c.socketPath }

func (c *Client) resetGeneratedClient() error {
	if c.httpClient == nil {
		c.httpClient = unixHTTPClient(c.socketPath, defaultTimeout)
	}
	generated, err := sessionapigen.NewClient(c.baseURL, sessionapigen.WithClient(c.httpClient))
	if err != nil {
		c.generated = nil
		return err
	}
	c.generated = generated
	return nil
}

func (c *Client) generatedClient() (*sessionapigen.Client, error) {
	if c.generated == nil {
		if err := c.resetGeneratedClient(); err != nil {
			return nil, err
		}
	}
	return c.generated, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.PingInfo(ctx)
	return err
}

func (c *Client) PingInfo(ctx context.Context) (*sessions.PingResponse, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	resp, err := generated.SessionsPing(ctx)
	if err != nil {
		return nil, classifyError(err)
	}
	var out sessions.PingResponse
	if err := convertGenerated(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Status(ctx context.Context) (*sessions.StatusResponse, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	resp, err := generated.SessionsStatus(ctx)
	if err != nil {
		return nil, classifyError(err)
	}
	var out sessions.StatusResponse
	if err := convertGenerated(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Agents(ctx context.Context) ([]sessions.Agent, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	resp, err := generated.SessionsAgents(ctx)
	if err != nil {
		return nil, classifyError(err)
	}
	var out sessions.AgentsResponse
	if err := convertGenerated(resp, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

func (c *Client) List(ctx context.Context) ([]sessions.Session, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	resp, err := generated.SessionsList(ctx)
	if err != nil {
		return nil, classifyError(err)
	}
	var out sessions.SessionsResponse
	if err := convertGenerated(resp, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func (c *Client) Create(ctx context.Context, req sessions.CreateRequest) (sessions.Session, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return sessions.Session{}, err
	}
	body := &sessionapigen.CreateRequest{
		AgentId: req.AgentID,
		Args:    req.Args,
	}
	if req.Workdir != "" {
		body.Workdir = sessionapigen.NewOptString(req.Workdir)
	}
	if req.Cols != 0 {
		body.Cols = sessionapigen.NewOptInt(int(req.Cols))
	}
	if req.Rows != 0 {
		body.Rows = sessionapigen.NewOptInt(int(req.Rows))
	}
	resp, err := generated.SessionsCreate(ctx, body)
	if err != nil {
		return sessions.Session{}, classifyError(err)
	}
	created, ok := resp.(*sessionapigen.CreateResponse)
	if !ok {
		return sessions.Session{}, responseFromGeneratedError(resp)
	}
	var out sessions.CreateResponse
	if err := convertGenerated(created, &out); err != nil {
		return sessions.Session{}, err
	}
	return out.Session, nil
}

func (c *Client) Resize(ctx context.Context, sessionID string, req sessions.ResizeRequest) error {
	generated, err := c.generatedClient()
	if err != nil {
		return err
	}
	resp, err := generated.SessionsResize(ctx, &sessionapigen.ResizeRequest{Cols: int(req.Cols), Rows: int(req.Rows)}, sessionapigen.SessionsResizeParams{SessionId: sessionID})
	if err != nil {
		return classifyError(err)
	}
	if _, ok := resp.(*sessionapigen.ActionResponse); !ok {
		return responseFromGeneratedError(resp)
	}
	return nil
}

func (c *Client) Signal(ctx context.Context, sessionID, signal string) error {
	generated, err := c.generatedClient()
	if err != nil {
		return err
	}
	resp, err := generated.SessionsSignal(ctx, &sessionapigen.SignalRequest{Signal: signal}, sessionapigen.SessionsSignalParams{SessionId: sessionID})
	if err != nil {
		return classifyError(err)
	}
	if _, ok := resp.(*sessionapigen.ActionResponse); !ok {
		return responseFromGeneratedError(resp)
	}
	return nil
}

func (c *Client) Shutdown(ctx context.Context) error {
	generated, err := c.generatedClient()
	if err != nil {
		return err
	}
	_, err = generated.SessionsShutdown(ctx)
	return classifyError(err)
}

type AttachStream struct {
	conn    net.Conn
	rw      *bufio.ReadWriter
	writeMu sync.Mutex
}

func (c *Client) Attach(ctx context.Context, sessionID string) (*AttachStream, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, classifyError(err)
	}
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions/"+sessionID+"/attach", nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-session")
	if err := req.Write(rw); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(rw.Reader, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, responseError(resp.StatusCode, body)
	}
	return &AttachStream{conn: conn, rw: rw}, nil
}

func (s *AttachStream) ReadFrame() (sessions.Frame, error) {
	return sessions.ReadFrame(s.rw)
}

func (s *AttachStream) WriteInput(payload []byte) error {
	return s.writeFrame(sessions.FrameInput, payload)
}

func (s *AttachStream) Resize(cols, rows uint16) error {
	payload, err := sessions.EncodeResize(cols, rows)
	if err != nil {
		return err
	}
	return s.writeFrame(sessions.FrameResize, payload)
}

func (s *AttachStream) Signal(signal string) error {
	return s.writeFrame(sessions.FrameSignal, []byte(signal))
}

func (s *AttachStream) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.rw.Flush(); err != nil {
		return err
	}
	if conn, ok := s.conn.(*net.UnixConn); ok {
		return conn.CloseWrite()
	}
	return nil
}

func (s *AttachStream) Close() error {
	return s.conn.Close()
}

func (s *AttachStream) writeFrame(typ byte, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := sessions.WriteFrame(s.rw, typ, payload); err != nil {
		return err
	}
	return s.rw.Flush()
}

func convertGenerated(in any, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("convert generated value: %w", err)
	}
	return nil
}

func responseFromGeneratedError(resp any) error {
	var out struct {
		Error string `json:"error"`
	}
	if err := convertGenerated(resp, &out); err == nil && out.Error != "" {
		return fmt.Errorf("daemon returned error: %s", out.Error)
	}
	return fmt.Errorf("daemon returned unexpected response %T", resp)
}

func unixHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}},
	}
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) {
		return ErrNotRunning
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ErrNotRunning
	}
	if strings.Contains(err.Error(), "connect: no such file") || strings.Contains(err.Error(), "connection refused") {
		return ErrNotRunning
	}
	return err
}

func responseError(status int, body []byte) error {
	var wrapped struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error != "" {
		return fmt.Errorf("daemon returned %d: %s", status, wrapped.Error)
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return fmt.Errorf("daemon returned %d: %s", status, trimmed)
	}
	return fmt.Errorf("daemon returned %d", status)
}
