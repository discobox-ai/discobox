package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ExitCodeStartup is used when the hook process could not be started.
	ExitCodeStartup = 126
	// ExitCodeTimeout is used when the hook context deadline is exceeded.
	ExitCodeTimeout = 124
	// ExitCodeCanceled is used when the hook context is canceled without a deadline.
	ExitCodeCanceled = 130

	defaultOutputSummaryBytes = 64 * 1024
)

// HookDefinition is the subset of a discovered hook definition needed to execute
// a script hook. It intentionally stays independent from parser internals.
type HookDefinition struct {
	ID      string
	Name    string
	Type    string
	Engine  string
	Path    string
	Pattern string

	// Command is the executable path from the hook definition. Relative paths are
	// resolved from RepoRoot by Run.
	Command string
	// Args are passed directly to Command. Runner does not invoke a shell.
	Args []string
}

// Request describes one hook execution.
type Request struct {
	Hook HookDefinition

	SessionID string
	RepoRoot  string
	Workspace string
	RunID     string

	ChangedFiles     []string
	ChangedFilesJSON string

	DBPath     string
	SocketPath string

	Timeout time.Duration

	// OutputWriter receives combined stdout/stderr as the process writes it. Runner
	// also captures output in Result.Output for summaries.
	OutputWriter io.Writer

	// Environ is the inherited environment. If nil, os.Environ is used. Values in
	// Env override inherited values, and DISCOBOX_* contract values override both.
	Environ []string
	Env     map[string]string

	// OutputSummaryBytes limits Result.Output. A zero value uses the package default.
	// Negative values keep the full captured output in Result.Output.
	OutputSummaryBytes int
}

// Result is the normalized outcome of one hook process run.
type Result struct {
	Success   bool
	ExitCode  int
	Duration  time.Duration
	Output    string
	Truncated bool

	TimedOut bool
	Canceled bool

	// Err is set for local execution failures such as start, timeout, or
	// cancellation. A hook that starts and exits non-zero reports Success=false and
	// ExitCode with Err left nil.
	Err error
}

// Run executes one script hook from the repository root.
func Run(ctx context.Context, req Request) Result {
	start := time.Now()
	res := Result{}

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	if req.RepoRoot == "" {
		res.ExitCode = ExitCodeStartup
		res.Err = errors.New("repo root is required")
		res.Duration = time.Since(start)
		return res
	}
	if req.Hook.Command == "" {
		res.ExitCode = ExitCodeStartup
		res.Err = errors.New("hook command is required")
		res.Duration = time.Since(start)
		return res
	}

	changedFilesJSON, err := changedFilesJSON(req)
	if err != nil {
		res.ExitCode = ExitCodeStartup
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}
	var output bytes.Buffer
	writer := io.Writer(&output)
	if req.OutputWriter != nil {
		writer = io.MultiWriter(&output, req.OutputWriter)
	}

	cmdPath := req.Hook.Command
	if !filepath.IsAbs(cmdPath) {
		cmdPath = filepath.Join(req.RepoRoot, cmdPath)
	}

	cmd := exec.CommandContext(ctx, cmdPath, req.Hook.Args...)
	cmd.Dir = req.RepoRoot
	cmd.Env = BuildEnv(req, changedFilesJSON)
	cmd.Stdout = writer
	cmd.Stderr = writer
	configureCommandForGroupKill(cmd)

	if err := cmd.Start(); err != nil {
		res.ExitCode = ExitCodeStartup
		res.Err = err
		res.Duration = time.Since(start)
		res.Output, res.Truncated = summarizeOutput(output.String(), req.OutputSummaryBytes)
		return res
	}

	err = cmd.Wait()
	res.Duration = time.Since(start)
	res.Output, res.Truncated = summarizeOutput(output.String(), req.OutputSummaryBytes)

	if ctxErr := ctx.Err(); ctxErr != nil {
		res.Err = ctxErr
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			res.TimedOut = true
			res.ExitCode = ExitCodeTimeout
		} else {
			res.Canceled = true
			res.ExitCode = ExitCodeCanceled
		}
		return res
	}

	if err == nil {
		res.Success = true
		res.ExitCode = 0
		return res
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res
	}

	res.ExitCode = ExitCodeStartup
	res.Err = err
	return res
}

// BuildEnv returns the process environment for a hook run.
func BuildEnv(req Request, changedFilesJSON string) []string {
	base := req.Environ
	if base == nil {
		base = os.Environ()
	}

	merged := make(map[string]string, len(base)+len(req.Env)+16)
	order := make([]string, 0, len(base)+len(req.Env)+16)
	put := func(k, v string) {
		if _, ok := merged[k]; !ok {
			order = append(order, k)
		}
		merged[k] = v
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			put(k, v)
		}
	}
	for k, v := range req.Env {
		put(k, v)
	}

	workspace := req.Workspace
	if workspace == "" {
		workspace = req.RepoRoot
	}
	changedFilesDisplay := strings.Join(req.ChangedFiles, "\n")
	contract := map[string]string{
		"DISCOBOX_SESSION_ID":         req.SessionID,
		"DISCOBOX_REPO_ROOT":          req.RepoRoot,
		"DISCOBOX_WORKSPACE":          workspace,
		"DISCOBOX_HOOK_ID":            req.Hook.ID,
		"DISCOBOX_HOOK_NAME":          req.Hook.Name,
		"DISCOBOX_HOOK_TYPE":          req.Hook.Type,
		"DISCOBOX_HOOK_PATH":          req.Hook.Path,
		"DISCOBOX_HOOK_PATTERN":       req.Hook.Pattern,
		"DISCOBOX_HOOK_RUN_ID":        req.RunID,
		"DISCOBOX_CHANGED_FILES":      changedFilesDisplay,
		"DISCOBOX_CHANGED_FILES_JSON": changedFilesJSON,
		"DISCOBOX_DB_PATH":            req.DBPath,
		"DISCOBOX_SOCKET_PATH":        req.SocketPath,
	}
	contractOrder := []string{
		"DISCOBOX_SESSION_ID",
		"DISCOBOX_REPO_ROOT",
		"DISCOBOX_WORKSPACE",
		"DISCOBOX_HOOK_ID",
		"DISCOBOX_HOOK_NAME",
		"DISCOBOX_HOOK_TYPE",
		"DISCOBOX_HOOK_PATH",
		"DISCOBOX_HOOK_PATTERN",
		"DISCOBOX_HOOK_RUN_ID",
		"DISCOBOX_CHANGED_FILES",
		"DISCOBOX_CHANGED_FILES_JSON",
		"DISCOBOX_DB_PATH",
		"DISCOBOX_SOCKET_PATH",
	}
	for _, k := range contractOrder {
		put(k, contract[k])
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func changedFilesJSON(req Request) (string, error) {
	if req.ChangedFilesJSON != "" {
		return req.ChangedFilesJSON, nil
	}
	payload, err := json.Marshal(req.ChangedFiles)
	if err != nil {
		return "", fmt.Errorf("marshal changed files json: %w", err)
	}
	return string(payload), nil
}

func summarizeOutput(output string, limit int) (string, bool) {
	if limit == 0 {
		limit = defaultOutputSummaryBytes
	}
	if limit < 0 || len(output) <= limit {
		return output, false
	}
	return output[len(output)-limit:], true
}
