// Package lspclient implements the small stdio LSP client surface needed by
// Discobox LSP hooks.
package lspclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/hooks/processhelper"
)

// Diagnostic is a normalized LSP diagnostic.
type Diagnostic struct {
	URI       string
	Path      string
	Severity  string
	Source    string
	Code      string
	Message   string
	StartLine int
	StartCol  int
	EndLine   int
	EndCol    int
}

// DiagnosticHandler receives publishDiagnostics notifications.
type DiagnosticHandler func(uri string, diagnostics []Diagnostic)

// FileChangeType is the LSP workspace/didChangeWatchedFiles change type.
type FileChangeType int

const (
	FileCreated FileChangeType = 1
	FileChanged FileChangeType = 2
	FileDeleted FileChangeType = 3
)

// FileChange is one workspace file-change notification.
type FileChange struct {
	URI  string
	Type FileChangeType
}

// Client owns one stdio language server process.
type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	repoRoot  string
	rootURI   string
	language  string
	onDiag    DiagnosticHandler
	docMu     sync.Mutex
	versions  map[string]int
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[int]chan response
	nextID    int
	done      chan struct{}
	stderrBuf bytes.Buffer
}

// Options configures a language server process.
type Options struct {
	Command      string
	Args         []string
	RepoRoot     string
	LanguageID   string
	Env          []string
	OnDiagnostic DiagnosticHandler
}

// Start launches the language server and initializes the LSP session.
func Start(ctx context.Context, opts Options) (*Client, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return nil, fmt.Errorf("lsp command is required")
	}
	if strings.TrimSpace(opts.RepoRoot) == "" {
		return nil, fmt.Errorf("repo root is required")
	}
	repoRoot, err := filepath.Abs(opts.RepoRoot)
	if err != nil {
		return nil, err
	}
	cmdPath := opts.Command
	if !filepath.IsAbs(cmdPath) {
		cmdPath = filepath.Join(repoRoot, cmdPath)
	}
	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	cmd, err := processhelper.CommandContext(ctx, processhelper.CommandOptions{
		Command: cmdPath,
		Args:    opts.Args,
		Dir:     repoRoot,
		Env:     env,
		Grace:   2 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	cmd.Dir = repoRoot
	cmd.Env = env
	configureCommandForCleanup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		repoRoot: repoRoot,
		rootURI:  FileURI(repoRoot),
		language: opts.LanguageID,
		onDiag:   opts.OnDiagnostic,
		versions: map[string]int{},
		pending:  map[int]chan response{},
		done:     make(chan struct{}),
	}
	go c.readLoop()
	go c.stderrLoop()
	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// DidOpen sends textDocument/didOpen for a file.
func (c *Client) DidOpen(ctx context.Context, path string) error {
	abs := c.abs(path)
	text, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	uri := FileURI(abs)
	params := map[string]any{"textDocument": map[string]any{
		"uri":        uri,
		"languageId": c.language,
		"version":    c.nextDocumentVersion(uri),
		"text":       string(text),
	}}
	return c.notify(ctx, "textDocument/didOpen", params)
}

// DidChange sends a full-content textDocument/didChange for a file.
func (c *Client) DidChange(ctx context.Context, path string) error {
	abs := c.abs(path)
	text, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	uri := FileURI(abs)
	params := map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": c.nextDocumentVersion(uri)},
		"contentChanges": []map[string]any{{"text": string(text)}},
	}
	return c.notify(ctx, "textDocument/didChange", params)
}

// DidSave sends textDocument/didSave for a file.
func (c *Client) DidSave(ctx context.Context, path string) error {
	abs := c.abs(path)
	text, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": FileURI(abs)},
		"text":         string(text),
	}
	return c.notify(ctx, "textDocument/didSave", params)
}

// DidChangeWatchedFiles sends workspace/didChangeWatchedFiles for repository files.
func (c *Client) DidChangeWatchedFiles(ctx context.Context, changes []FileChange) error {
	if len(changes) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(changes))
	for _, change := range changes {
		if strings.TrimSpace(change.URI) == "" {
			continue
		}
		items = append(items, map[string]any{"uri": change.URI, "type": change.Type})
	}
	if len(items) == 0 {
		return nil
	}
	return c.notify(ctx, "workspace/didChangeWatchedFiles", map[string]any{"changes": items})
}

// DidClose sends textDocument/didClose for a file.
func (c *Client) DidClose(ctx context.Context, path string) error {
	uri := FileURI(c.abs(path))
	c.docMu.Lock()
	delete(c.versions, uri)
	c.docMu.Unlock()
	params := map[string]any{"textDocument": map[string]any{"uri": uri}}
	return c.notify(ctx, "textDocument/didClose", params)
}

// Close shuts down the LSP session and terminates the process.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = c.call(ctx, "shutdown", nil)
	_ = c.notify(ctx, "exit", nil)
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = terminateCommand(c.cmd)
		}
	}
	return c.cmd.Wait()
}

// Stderr returns captured language-server stderr.
func (c *Client) Stderr() string { return c.stderrBuf.String() }

func (c *Client) nextDocumentVersion(uri string) int {
	c.docMu.Lock()
	defer c.docMu.Unlock()
	if c.versions == nil {
		c.versions = map[string]int{}
	}
	c.versions[uri]++
	return c.versions[uri]
}

func (c *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   c.rootURI,
		"workspaceFolders": []map[string]string{{
			"uri":  c.rootURI,
			"name": filepath.Base(c.repoRoot),
		}},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true, "dynamicRegistration": false},
				"publishDiagnostics": map[string]any{"relatedInformation": true, "versionSupport": false},
			},
			"workspace": map[string]any{"workspaceFolders": true},
		},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return err
	}
	return c.notify(ctx, "initialized", map[string]any{})
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextRequestID()
	ch := make(chan response, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	if err := c.write(ctx, request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.deletePending(id)
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.deletePending(id)
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("lsp %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-c.done:
		c.deletePending(id)
		return nil, fmt.Errorf("language server exited%s", c.formattedStderr())
	}
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.write(ctx, notification{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) write(ctx context.Context, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	header := []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data)))
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeAll(ctx, c.stdin, header); err != nil {
		return err
	}
	return writeAll(ctx, c.stdin, data)
}

func writeAll(ctx context.Context, w io.Writer, data []byte) error {
	done := make(chan error, 1)
	go func() {
		_, err := w.Write(data)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (c *Client) readLoop() {
	defer close(c.done)
	reader := bufio.NewReader(c.stdout)
	for {
		body, err := readMessage(reader)
		if err != nil {
			return
		}
		var envelope struct {
			ID     *int            `json:"id,omitempty"`
			Method string          `json:"method,omitempty"`
			Params json.RawMessage `json:"params,omitempty"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *rpcError       `json:"error,omitempty"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			continue
		}
		if envelope.ID != nil {
			c.pendingMu.Lock()
			ch := c.pending[*envelope.ID]
			delete(c.pending, *envelope.ID)
			c.pendingMu.Unlock()
			if ch != nil {
				ch <- response{Result: envelope.Result, Error: envelope.Error}
			}
			continue
		}
		if envelope.Method == "textDocument/publishDiagnostics" {
			c.handleDiagnostics(envelope.Params)
		}
	}
}

func (c *Client) stderrLoop() {
	_, _ = io.Copy(&c.stderrBuf, c.stderr)
}

func (c *Client) handleDiagnostics(raw json.RawMessage) {
	if c.onDiag == nil {
		return
	}
	var params struct {
		URI         string `json:"uri"`
		Diagnostics []struct {
			Range struct {
				Start struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"start"`
				End struct {
					Line      int `json:"line"`
					Character int `json:"character"`
				} `json:"end"`
			} `json:"range"`
			Severity int             `json:"severity"`
			Code     json.RawMessage `json:"code"`
			Source   string          `json:"source"`
			Message  string          `json:"message"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	path := PathFromURI(c.repoRoot, params.URI)
	out := make([]Diagnostic, 0, len(params.Diagnostics))
	for _, diagnostic := range params.Diagnostics {
		out = append(out, Diagnostic{
			URI:       params.URI,
			Path:      path,
			Severity:  severityName(diagnostic.Severity),
			Source:    diagnostic.Source,
			Code:      decodeCode(diagnostic.Code),
			Message:   diagnostic.Message,
			StartLine: diagnostic.Range.Start.Line + 1,
			StartCol:  diagnostic.Range.Start.Character + 1,
			EndLine:   diagnostic.Range.End.Line + 1,
			EndCol:    diagnostic.Range.End.Character + 1,
		})
	}
	c.onDiag(params.URI, out)
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			contentLength, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if contentLength < 0 {
		return nil, errors.New("missing content length")
	}
	body := make([]byte, contentLength)
	_, err := io.ReadFull(r, body)
	return body, err
}

func (c *Client) nextRequestID() int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	c.nextID++
	return c.nextID
}

func (c *Client) deletePending(id int) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

func (c *Client) abs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(c.repoRoot, filepath.FromSlash(path))
}

func (c *Client) formattedStderr() string {
	stderr := strings.TrimSpace(c.stderrBuf.String())
	if stderr == "" {
		return ""
	}
	return ": " + stderr
}

func decodeCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.Itoa(n)
	}
	return strings.TrimSpace(string(raw))
}

func severityName(severity int) string {
	switch severity {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "information"
	case 4:
		return "hint"
	default:
		return "error"
	}
}

// FileURI returns a file:// URI for path.
func FileURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	return u.String()
}

// PathFromURI converts a file:// URI to a repository-relative slash path.
func PathFromURI(repoRoot, uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return uri
	}
	path := filepath.FromSlash(parsed.Path)
	rel, err := filepath.Rel(repoRoot, path)
	if err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	Result json.RawMessage
	Error  *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
