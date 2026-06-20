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
	"net/url"
	"os"
	"strings"
	"time"

	hookapigen "github.com/obot-platform/discobox/hooks/api/gen"
	"github.com/obot-platform/discobox/hooks/api/model"
)

const defaultTimeout = 10 * time.Second

var (
	// ErrNotRunning reports that the daemon socket is missing or cannot be reached.
	ErrNotRunning = errors.New("hook daemon not running")
	// ErrTimeout reports that a daemon request timed out.
	ErrTimeout = errors.New("hook daemon request timed out")
)

// Client talks to a session hook daemon over HTTP on a Unix domain socket.
type Client struct {
	socketPath string
	httpClient *http.Client
	baseURL    string
	generated  *hookapigen.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient overrides the HTTP client, primarily for tests.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithBaseURL overrides the request base URL, primarily for tests.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithTimeout sets the HTTP client timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if c.httpClient == nil {
			c.httpClient = unixHTTPClient(c.socketPath, d)
		} else {
			c.httpClient.Timeout = d
		}
	}
}

// New returns a daemon client for socketPath.
func New(socketPath string, opts ...Option) *Client {
	c := &Client{socketPath: socketPath, baseURL: "http://unix", httpClient: unixHTTPClient(socketPath, defaultTimeout)}
	for _, opt := range opts {
		opt(c)
	}
	_ = c.resetGeneratedClient()
	return c
}

func (c *Client) resetGeneratedClient() error {
	if c.httpClient == nil {
		c.httpClient = unixHTTPClient(c.socketPath, defaultTimeout)
	}
	generated, err := hookapigen.NewClient(c.baseURL, hookapigen.WithClient(c.httpClient))
	if err != nil {
		c.generated = nil
		return err
	}
	c.generated = generated
	return nil
}

func (c *Client) generatedClient() (*hookapigen.Client, error) {
	if c.generated == nil {
		if err := c.resetGeneratedClient(); err != nil {
			return nil, err
		}
	}
	return c.generated, nil
}

func unixHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if socketPath == "" {
			return nil, fmt.Errorf("socket path is required")
		}
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// SocketPath is the Unix domain socket path used by this client.
func (c *Client) SocketPath() string { return c.socketPath }

// Ping verifies daemon reachability.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.PingInfo(ctx)
	return err
}

// PingInfo verifies daemon reachability and returns daemon metadata.
func (c *Client) PingInfo(ctx context.Context) (*PingResponse, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	pong, err := generated.HooksPing(ctx)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	return convertGeneratedPtr[PingResponse](pong)
}

// Status returns daemon/session status.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	status, err := generated.HooksStatus(ctx)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	return convertGeneratedPtr[StatusResponse](status)
}

// Wait waits for daemon hook work and pending watcher batches to reach a
// terminal state. The returned response has Settled=false when the timeout
// expires before terminal state is reached.
func (c *Client) Wait(ctx context.Context, timeout time.Duration) (*WaitResponse, error) {
	params := url.Values{}
	if timeout >= 0 {
		params.Set("timeout_seconds", fmt.Sprintf("%d", timeoutSeconds(timeout)))
	}
	reqURL := c.baseURL + "/wait"
	if encoded := params.Encode(); encoded != "" {
		reqURL += "?" + encoded
	}
	httpClient := unixHTTPClient(c.socketPath, 0)
	if timeout > 0 {
		httpClient = unixHTTPClient(c.socketPath, timeout+5*time.Second)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, responseError(resp.StatusCode, body)
	}
	var out WaitResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode wait response: %w", err)
	}
	return &out, nil
}

func timeoutSeconds(timeout time.Duration) int {
	if timeout <= 0 {
		return 0
	}
	seconds := int(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	return seconds
}

// ListHooks returns discovered hooks and current status metadata.
func (c *Client) ListHooks(ctx context.Context) ([]HookStatus, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	hooks, err := generated.HooksList(ctx)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	var wrapped HooksResponse
	if err := convertGenerated(hooks, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Hooks, nil
}

// ListEvents returns durable daemon audit events.
func (c *Client) ListEvents(ctx context.Context, opts EventOptions) ([]Event, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	params := hookapigen.HooksListEventsParams{}
	if opts.HookID != "" {
		params.HookID = hookapigen.NewOptString(opts.HookID)
	}
	if opts.Limit > 0 {
		params.Limit = hookapigen.NewOptInt(opts.Limit)
	}
	events, err := generated.HooksListEvents(ctx, params)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	var wrapped EventsResponse
	if err := convertGenerated(events, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Events, nil
}

// ListRuns returns hook run history.
func (c *Client) ListRuns(ctx context.Context, opts RunListOptions) ([]Run, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	params := hookapigen.HooksListRunsParams{}
	if opts.HookID != "" {
		params.HookID = hookapigen.NewOptString(opts.HookID)
	}
	if opts.hasLimit() {
		params.Limit = hookapigen.NewOptInt(opts.Limit)
	}
	runs, err := generated.HooksListRuns(ctx, params)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	var wrapped RunsResponse
	if err := convertGenerated(runs, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Runs, nil
}

// ListObservedChanges returns daemon-observed filesystem changes.
func (c *Client) ListObservedChanges(ctx context.Context, opts ListOptions) ([]ObservedFileChange, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	params := hookapigen.HooksListChangesParams{}
	if opts.hasLimit() {
		params.Limit = hookapigen.NewOptInt(opts.Limit)
	}
	changes, err := generated.HooksListChanges(ctx, params)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	var wrapped ChangesResponse
	if err := convertGenerated(changes, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Changes, nil
}

// ListWorkspaceSnapshots returns captured workspace snapshots.
func (c *Client) ListWorkspaceSnapshots(ctx context.Context, opts ListOptions) ([]WorkspaceSnapshot, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	params := hookapigen.HooksListSnapshotsParams{}
	if opts.hasLimit() {
		params.Limit = hookapigen.NewOptInt(opts.Limit)
	}
	snapshots, err := generated.HooksListSnapshots(ctx, params)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	var wrapped SnapshotsResponse
	if err := convertGenerated(snapshots, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Snapshots, nil
}

// GetWorkspaceSnapshot returns one captured workspace snapshot, including patch data.
func (c *Client) GetWorkspaceSnapshot(ctx context.Context, snapshotID string) (*WorkspaceSnapshot, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	snapshot, err := generated.HooksGetSnapshot(ctx, hookapigen.HooksGetSnapshotParams{SnapshotId: snapshotID})
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	if err := generatedResultError(snapshot); err != nil {
		return nil, err
	}
	return convertGeneratedPtr[WorkspaceSnapshot](snapshot)
}

// ListQueue returns queued hook work.
func (c *Client) ListQueue(ctx context.Context, opts ListOptions) ([]QueuedHook, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	params := hookapigen.HooksListQueueParams{}
	if opts.hasLimit() {
		params.Limit = hookapigen.NewOptInt(opts.Limit)
	}
	queue, err := generated.HooksListQueue(ctx, params)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	var wrapped QueueResponse
	if err := convertGenerated(queue, &wrapped); err != nil {
		return nil, err
	}
	return wrapped.Queue, nil
}

// ListDiagnostics returns current LSP diagnostics.
func (c *Client) ListDiagnostics(ctx context.Context, opts DiagnosticOptions) ([]Diagnostic, error) {
	params := url.Values{}
	if opts.HookID != "" {
		params.Set("hook_id", opts.HookID)
	}
	if opts.hasLimit() {
		params.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	reqURL := c.baseURL + "/diagnostics"
	if encoded := params.Encode(); encoded != "" {
		reqURL += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, responseError(resp.StatusCode, body)
	}
	var wrapped DiagnosticsResponse
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("decode diagnostics response: %w", err)
	}
	return wrapped.Diagnostics, nil
}

// FollowEvents streams daemon audit events until ctx is canceled or the stream
// fails. Existing events are not replayed unless LastEventID is set.
func (c *Client) FollowEvents(ctx context.Context, opts EventOptions, fn func(Event) error) error {
	if fn == nil {
		return fmt.Errorf("event callback is required")
	}
	if c.httpClient == nil {
		c.httpClient = unixHTTPClient(c.socketPath, defaultTimeout)
	}
	query := eventQuery(opts)
	path := "/events/stream"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if opts.LastEventID != "" {
		req.Header.Set("Last-Event-ID", opts.LastEventID)
	}
	httpClient := *c.httpClient
	httpClient.Timeout = 0
	resp, err := httpClient.Do(req)
	if err != nil {
		return classifyError(c.socketPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return responseError(resp.StatusCode, b)
	}
	return readSSEEvents(resp.Body, fn)
}

func eventQuery(opts EventOptions) url.Values {
	query := url.Values{}
	if opts.HookID != "" {
		query.Set("hook_id", opts.HookID)
	}
	if opts.Limit > 0 {
		query.Set("limit", fmt.Sprint(opts.Limit))
	}
	return query
}

func readSSEEvents(r io.Reader, fn func(Event) error) error {
	reader := bufio.NewReader(r)
	var data []string
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		defer func() { data = data[:0] }()
		var event Event
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &event); err != nil {
			return fmt.Errorf("decode event stream data: %w", err)
		}
		return fn(event)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if errors.Is(err, io.EOF) {
				return dispatch()
			}
			return err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, ":") {
			// Ignore SSE comments/keepalives.
		} else if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
		}
		if errors.Is(err, io.EOF) {
			return dispatch()
		}
		if err != nil {
			return err
		}
	}
}

// PauseAll pauses global hook execution.
func (c *Client) PauseAll(ctx context.Context) error { return c.setExecution(ctx, true) }

// ResumeAll resumes global hook execution.
func (c *Client) ResumeAll(ctx context.Context) error {
	return c.setExecution(ctx, false)
}

// PauseHook pauses one hook.
func (c *Client) PauseHook(ctx context.Context, id string) error {
	return c.setHookExecution(ctx, id, true)
}

// ResumeHook resumes one hook.
func (c *Client) ResumeHook(ctx context.Context, id string) error {
	return c.setHookExecution(ctx, id, false)
}

func (c *Client) setExecution(ctx context.Context, paused bool) error {
	generated, err := c.generatedClient()
	if err != nil {
		return err
	}
	req := &hookapigen.ExecutionPatchRequest{Paused: paused}
	_, err = generated.HooksSetExecution(ctx, req)
	return classifyError(c.socketPath, err)
}

func (c *Client) setHookExecution(ctx context.Context, id string, paused bool) error {
	generated, err := c.generatedClient()
	if err != nil {
		return err
	}
	req := &hookapigen.ExecutionPatchRequest{Paused: paused}
	res, err := generated.HooksSetHookExecution(ctx, req, hookapigen.HooksSetHookExecutionParams{HookId: id})
	if err != nil {
		return classifyError(c.socketPath, err)
	}
	return generatedResultError(res)
}

// RunHook requests a hook run if it has not succeeded yet, unless Force is set.
func (c *Client) RunHook(ctx context.Context, id string, opts RunOptions) (*RunResponse, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	req := hookapigen.RunRequest{}
	req.Force = hookapigen.NewOptBool(opts.Force)
	if opts.Phase != "" {
		req.Phase = hookapigen.NewOptString(opts.Phase)
	}
	res, err := generated.HooksRunHook(ctx, hookapigen.NewOptRunRequest(req), hookapigen.HooksRunHookParams{HookId: id})
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	if err := generatedResultError(res); err != nil {
		return nil, err
	}
	out, ok := res.(*hookapigen.RunResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected run response %T", res)
	}
	return convertGeneratedPtr[RunResponse](out)
}

// Output returns captured output for a hook's latest run.
func (c *Client) Output(ctx context.Context, id string) ([]byte, error) {
	generated, err := c.generatedClient()
	if err != nil {
		return nil, err
	}
	res, err := generated.HooksOutput(ctx, hookapigen.HooksOutputParams{HookId: id})
	if err != nil {
		return nil, classifyError(c.socketPath, err)
	}
	if err := generatedResultError(res); err != nil {
		return nil, err
	}
	out, ok := res.(*hookapigen.OutputResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected output response %T", res)
	}
	return []byte(out.Output), nil
}

// Shutdown asks the daemon to exit.
func (c *Client) Shutdown(ctx context.Context) error {
	generated, err := c.generatedClient()
	if err != nil {
		return err
	}
	_, err = generated.HooksShutdown(ctx)
	return classifyError(c.socketPath, err)
}

func classifyError(socketPath string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	if socketPath != "" {
		if _, statErr := os.Stat(socketPath); errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("%w: socket %s does not exist", ErrNotRunning, socketPath)
		}
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return fmt.Errorf("%w: %w", ErrNotRunning, err)
	}
	return err
}

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func responseError(status int, body []byte) error {
	var e apiError
	if err := json.Unmarshal(body, &e); err == nil {
		msg := firstNonEmpty(e.Message, e.Error)
		if msg != "" {
			return fmt.Errorf("daemon returned %d: %s", status, msg)
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("daemon returned %d: %s", status, msg)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func generatedResultError(res any) error {
	if e, ok := res.(*hookapigen.ErrorResponse); ok {
		msg := optionalString(e.Message)
		if msg == "" {
			msg = optionalString(e.Error)
		}
		if msg == "" {
			msg = "request failed"
		}
		return fmt.Errorf("daemon returned error: %s", msg)
	}
	return nil
}

func optionalString(value hookapigen.OptString) string {
	if v, ok := value.Get(); ok {
		return v
	}
	return ""
}

func convertGeneratedPtr[T any](value any) (*T, error) {
	var out T
	if err := convertGenerated(value, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func convertGenerated(value, out any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode generated daemon response: %w", err)
	}
	return nil
}

type PingResponse = model.PingResponse
type StatusResponse = model.StatusResponse
type WaitResponse = model.WaitResponse
type HooksResponse = model.HooksResponse
type EventsResponse = model.EventsResponse
type RunsResponse = model.RunsResponse
type ChangesResponse = model.ChangesResponse
type SnapshotsResponse = model.SnapshotsResponse
type QueueResponse = model.QueueResponse
type DiagnosticsResponse = model.DiagnosticsResponse

type EventOptions struct {
	HookID      string
	Limit       int
	LastEventID string
}

type ListOptions struct {
	Limit    int
	LimitSet bool
}

type RunListOptions struct {
	HookID   string
	Limit    int
	LimitSet bool
}

type DiagnosticOptions struct {
	HookID   string
	Limit    int
	LimitSet bool
}

func (o ListOptions) hasLimit() bool {
	return o.LimitSet || o.Limit > 0
}

func (o RunListOptions) hasLimit() bool {
	return o.LimitSet || o.Limit > 0
}

func (o DiagnosticOptions) hasLimit() bool {
	return o.LimitSet || o.Limit > 0
}

type Event = model.Event
type HookStatus = model.HookStatus
type Run = model.Run
type ChangedFile = model.ChangedFile
type ObservedFileChange = model.ObservedFileChange
type WorkspaceSnapshot = model.WorkspaceSnapshot
type QueuedHook = model.QueuedHook
type Diagnostic = model.Diagnostic

type RunOptions struct {
	Force bool
	Phase string
}

type RunResponse = model.RunResponse
