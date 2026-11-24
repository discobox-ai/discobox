package poolruntime

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A pool's snapshot builds are reached through its pool agent, which holds
// each build's guest (ADR 26-11-23-004 §4): the server forwards the caller's
// request and the build's key, and keeps nothing of the build.

// OpenSnapshotBuildLogs streams a build's feature logs from the pool agent.
func (p *Provider) OpenSnapshotBuildLogs(ctx context.Context, pool *model.Pool, key string, opts sandbox.SnapshotBuildLogOptions) (io.ReadCloser, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool is required")
	}
	client, err := p.agentClientForPool(ctx, pool)
	if err != nil {
		return nil, err
	}
	return client.OpenSnapshotBuildLogs(ctx, pool.ProjectID, key, opts)
}

// DialSnapshotBuildExec opens an exec in a build's box through the pool agent.
func (p *Provider) DialSnapshotBuildExec(ctx context.Context, pool *model.Pool, key string, opts sandbox.ConsoleOptions) (net.Conn, error) {
	if pool == nil {
		return nil, fmt.Errorf("pool is required")
	}
	client, err := p.agentClientForPool(ctx, pool)
	if err != nil {
		return nil, err
	}
	return client.DialSnapshotBuildExec(ctx, pool.ProjectID, key, opts)
}

// snapshotBuildPath is the pool agent's route for the build keyed key.
func (p *poolAgentClient) snapshotBuildPath(projectID, key, route string, query url.Values) string {
	path := fmt.Sprintf("/api/project/%s/pool/%s/snapshot-builds/%s/%s",
		url.PathEscape(projectID), url.PathEscape(p.poolID), url.PathEscape(key), route)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	return path
}

// OpenSnapshotBuildLogs reads a build's feature logs from the pool agent's
// hand-wired route, as a stream the lease is held for until it is closed.
func (p *poolAgentClient) OpenSnapshotBuildLogs(ctx context.Context, projectID, key string, opts sandbox.SnapshotBuildLogOptions) (io.ReadCloser, error) {
	lease, err := p.treeLease(sandbox.SandboxRef{ProjectID: projectID}, poolagentauth.ScopeSnapshotBuildRead)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if opts.Feature != "" {
		query.Set("feature", opts.Feature)
	}
	if opts.Tail > 0 {
		query.Set("tail", strconv.Itoa(opts.Tail))
	}
	if opts.Follow {
		query.Set("follow", "true")
	}
	resp, err := p.agentRequest(ctx, http.MethodGet, p.snapshotBuildPath(projectID, key, "logs", query), lease, nil)
	if err != nil {
		lease.Release()
		return nil, poolAgentTransportError("read a snapshot build's logs from", p.poolID, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		lease.Release()
		return nil, snapshotBuildStatusError(resp)
	}
	return &leasedReader{ReadCloser: resp.Body, lease: lease}, nil
}

// DialSnapshotBuildExec opens the pool agent's exec WebSocket for a build,
// as a connection of execstream frames. The lease is held until it closes.
func (p *poolAgentClient) DialSnapshotBuildExec(ctx context.Context, projectID, key string, opts sandbox.ConsoleOptions) (net.Conn, error) {
	lease, err := p.treeLease(sandbox.SandboxRef{ProjectID: projectID}, poolagentauth.ScopeSnapshotBuildExec)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for _, arg := range opts.Command {
		query.Add("command", arg)
	}
	if opts.TTY {
		query.Set("tty", "true")
		if opts.Rows > 0 && opts.Cols > 0 {
			query.Set("rows", strconv.Itoa(opts.Rows))
			query.Set("cols", strconv.Itoa(opts.Cols))
		}
	}
	header := http.Header{}
	if lease.AuthTokenProvider != nil {
		token, err := lease.AuthTokenProvider(ctx)
		if err != nil {
			lease.Release()
			return nil, err
		}
		header.Set("Authorization", "Bearer "+token)
	}
	transport, closeIdle := http1Transport(lease.Client)
	// The stream outlives the request that opened it: it is the connection's
	// own context that ends it, when it is closed.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	socket, resp, err := websocket.Dial(ctx, leaseBaseURL(lease.BaseURL)+p.snapshotBuildPath(projectID, key, "exec", query), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport}, HTTPHeader: header, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		cancel()
		closeIdle()
		lease.Release()
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSwitchingProtocols {
				return nil, snapshotBuildStatusError(resp)
			}
		}
		return nil, poolAgentTransportError("open a shell in a snapshot build on", p.poolID, err)
	}
	socket.SetReadLimit(-1)
	return &leasedConn{Conn: websocket.NetConn(streamCtx, socket, websocket.MessageBinary), release: func() {
		cancel()
		closeIdle()
		lease.Release()
	}}, nil
}

// http1Transport is client's transport made to speak HTTP/1.1 alone, which a
// WebSocket upgrade needs, and what closes its idle connections.
func http1Transport(client *http.Client) (http.RoundTripper, func()) {
	transport := http.DefaultTransport
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	t, ok := transport.(*http.Transport)
	if !ok {
		return transport, func() {}
	}
	t = t.Clone()
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	t.Protocols = &protocols
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if t.TLSClientConfig != nil {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
		t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return t, t.CloseIdleConnections
}

// leasedConn releases a pool-agent lease once the connection over it closes.
type leasedConn struct {
	net.Conn
	release func()
}

func (c *leasedConn) Close() error {
	err := c.Conn.Close()
	if c.release != nil {
		c.release()
		c.release = nil
	}
	return err
}

// snapshotBuildStatusError is the pool agent's refusal of a build's route,
// as the control plane answers it: its status, and the detail of its
// problem+json. The router's text/plain 404 is an agent too old to have the
// route.
func snapshotBuildStatusError(resp *http.Response) error {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode == http.StatusNotFound && mediaType != "application/problem+json" {
		return fmt.Errorf("%w: the pool agent has no snapshot build routes", sandbox.ErrPoolAgentUnsupported)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUnexpectedBody))
	message := strings.TrimSpace(string(body))
	var problem struct {
		Detail string `json:"detail"`
	}
	if mediaType == "application/problem+json" && json.Unmarshal(body, &problem) == nil && problem.Detail != "" {
		message = problem.Detail
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", sandbox.ErrNotFound, message)
	}
	return apperrors.NewStatusError(resp.StatusCode, message)
}
