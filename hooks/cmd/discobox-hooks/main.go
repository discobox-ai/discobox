package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	hooksapi "github.com/discobox-ai/discobox/hooks"
	hookapi "github.com/discobox-ai/discobox/hooks/api"
	"github.com/discobox-ai/discobox/hooks/client"
	"github.com/discobox-ai/discobox/hooks/daemon"
	"github.com/discobox-ai/discobox/hooks/models"
	"github.com/discobox-ai/discobox/hooks/processhelper"
	idpkg "github.com/discobox-ai/discobox/id"
	"github.com/discobox-ai/discobox/internal/gitutil"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const defaultSessionID = "default"

// version may be set by release builds with:
//
//	-ldflags "-X main.version=<unix-epoch-or-release-build-number>"
//
// Development builds fall back to the current executable mtime, which is the
// Unix epoch of the last local build.
var version string

type sessionPaths struct {
	SessionID  string
	RepoRoot   string
	StateDir   string
	RuntimeDir string
	Socket     string
	Lock       string
	DB         string
	Runtime    string
}

type cliOptions struct {
	sessionID string
	repoRoot  string
	noStart   bool
	output    string
	stdout    io.Writer
	stderr    io.Writer
}

type app struct {
	opts cliOptions
}

func main() {
	if handled, code := processhelper.HandleEntry(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(run(context.Background(), os.Args[1:], cliOptions{stdout: os.Stdout, stderr: os.Stderr}))
}

func run(ctx context.Context, args []string, opts cliOptions) int {
	cmd := newRootCommand(opts)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
		return 1
	}
	return 0
}

func newRootCommand(opts cliOptions) *cobra.Command {
	if opts.stdout == nil {
		opts.stdout = os.Stdout
	}
	if opts.stderr == nil {
		opts.stderr = os.Stderr
	}
	a := &app{opts: opts}
	cmd := &cobra.Command{
		Use:           "discobox-hooks",
		Short:         "Run and inspect Discobox hooks",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return a.validate()
		},
	}
	cmd.SetOut(opts.stdout)
	cmd.SetErr(opts.stderr)
	cmd.PersistentFlags().StringVar(&a.opts.sessionID, "session-id", a.opts.sessionID, "hook daemon session id (default DISCOBOX_SESSION_ID or repo hash)")
	cmd.PersistentFlags().StringVar(&a.opts.repoRoot, "repo-root", a.opts.repoRoot, "repository root (default: git rev-parse --show-toplevel)")
	cmd.PersistentFlags().BoolVar(&a.opts.noStart, "no-start", a.opts.noStart, "do not start daemon when socket is unavailable")
	cmd.PersistentFlags().StringVarP(&a.opts.output, "output", "o", outputOrDefault(a.opts.output), "Output format: table or json")

	cmd.AddCommand(a.newDaemonCommand())
	cmd.AddCommand(a.newListCommand())
	cmd.AddCommand(a.newEventsCommand())
	cmd.AddCommand(a.newRunCommand())
	cmd.AddCommand(a.newPauseCommand())
	cmd.AddCommand(a.newResumeCommand())
	cmd.AddCommand(a.newOutputCommand())
	cmd.AddCommand(a.newCheckCommand())
	cmd.AddCommand(a.newRunsCommand())
	cmd.AddCommand(a.newChangesCommand())
	cmd.AddCommand(a.newDiagnosticsCommand())
	cmd.AddCommand(a.newLSPCommand())
	cmd.AddCommand(a.newSnapshotsCommand())
	cmd.AddCommand(a.newQueueCommand())
	return cmd
}

func outputOrDefault(output string) string {
	if output == "" {
		return "table"
	}
	return output
}

func (a *app) validate() error {
	switch a.opts.output {
	case "table", "json":
		return nil
	default:
		return fmt.Errorf("unsupported output format %q; expected table or json", a.opts.output)
	}
}

func (a *app) newDaemonCommand() *cobra.Command {
	var idle, debounce, snapshotDebounce, snapshotMinInterval time.Duration
	var maxParallelHooks int
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the hook daemon in the foreground",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.sessionPaths(cmd.Context())
			if err != nil {
				return err
			}
			cfg := daemon.Config{
				SessionID:           paths.SessionID,
				RepoRoot:            paths.RepoRoot,
				DBPath:              paths.DB,
				SocketPath:          paths.Socket,
				RuntimePath:         paths.Runtime,
				TempDir:             filepath.Join(paths.RuntimeDir, "tmp"),
				Version:             currentBuildVersion(),
				Debounce:            debounce,
				SnapshotDebounce:    snapshotDebounce,
				SnapshotMinInterval: snapshotMinInterval,
				IdleTimeout:         idle,
				MaxParallelHooks:    maxParallelHooks,
			}
			return daemon.Run(cmd.Context(), cfg)
		},
	}
	cmd.Flags().DurationVar(&idle, "idle-timeout", 0, "daemon idle timeout (0 uses the 30m default; a negative value disables idle shutdown)")
	cmd.Flags().DurationVar(&debounce, "debounce", 0, "file-change debounce duration")
	cmd.Flags().DurationVar(&snapshotDebounce, "snapshot-debounce", 0, "workspace snapshot quiet duration")
	cmd.Flags().DurationVar(&snapshotMinInterval, "snapshot-min-interval", 0, "minimum time between workspace snapshot captures")
	cmd.Flags().IntVar(&maxParallelHooks, "max-parallel-hooks", 0, "maximum hook processes to run in parallel")
	cmd.Flags().Bool("foreground", true, "run daemon in foreground")
	cmd.AddCommand(a.newDaemonStatusCommand())
	cmd.AddCommand(a.newShutdownCommand())
	return cmd
}

func (a *app) newDaemonStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show hook daemon status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, paths, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			if st.SessionID == "" {
				st.SessionID = paths.SessionID
			}
			if st.RepoRoot == "" {
				st.RepoRoot = paths.RepoRoot
			}
			return a.writeStatus(cmd, st)
		},
	}
}

func (a *app) newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List hooks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			hooks, err := c.ListHooks(cmd.Context())
			if err != nil {
				return err
			}
			return a.writeHooks(cmd, hooks)
		},
	}
}

func (a *app) newEventsCommand() *cobra.Command {
	var limit int
	var follow bool
	var listTypes bool
	cmd := &cobra.Command{
		Use:     "events [HOOK_ID ...]",
		Aliases: []string{"event"},
		Short:   "List or follow daemon audit events",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if listTypes {
				if follow {
					return fmt.Errorf("events --list-types cannot be combined with --follow")
				}
				if len(args) > 0 {
					return fmt.Errorf("events --list-types does not accept hook ids")
				}
				return a.writeEventTypes(cmd, hookapi.KnownEventTypes())
			}
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			if follow {
				return a.followEvents(cmd, c, args, limit)
			}
			events, err := listTargetEvents(cmd.Context(), c, args, limit)
			if err != nil {
				return err
			}
			return a.writeEvents(cmd, events)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum events to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow new events")
	cmd.Flags().BoolVar(&listTypes, "list-types", false, "list known event types")
	return cmd
}

func (a *app) newRunCommand() *cobra.Command {
	var force bool
	var sessionHooks bool
	var phases []string
	cmd := &cobra.Command{
		Use:   "run [--phase PHASE[,PHASE...]] [HOOK_ID ...|all]",
		Short: "Request hook runs",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			phaseSet, allPhases := normalizePhaseSelector(phases)
			explicitIDs, expandAll := splitRunArgs(args)
			if len(args) == 0 && len(phaseSet) == 0 && !allPhases {
				return fmt.Errorf("specify hook IDs, %q, or --phase", "all")
			}
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			ids := explicitIDs
			expand := expandAll || len(explicitIDs) == 0
			if expand {
				hooksList, err := c.ListHooks(cmd.Context())
				if err != nil {
					return err
				}
				matched := filterRunTargets(hooksList, runTargetOptions{SessionHooks: sessionHooks, Phases: phaseSet, AllPhases: allPhases})
				ids = uniqueStrings(append(matched, explicitIDs...))
			}
			responses := make([]*client.RunResponse, 0, len(ids))
			for _, id := range ids {
				resp, err := c.RunHook(cmd.Context(), id, client.RunOptions{Force: force})
				if err != nil {
					return err
				}
				if resp.HookID == "" {
					resp.HookID = id
				}
				responses = append(responses, resp)
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"all": expand, "all_phases": allPhases, "session": sessionHooks, "phases": phaseSet, "force": force, "runs": responses})
			}
			for _, resp := range responses {
				if resp.Skipped {
					fmt.Fprintf(cmd.OutOrStdout(), "skipped %s: %s\n", resp.HookID, resp.Reason)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "run requested for %s\n", resp.HookID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "force hooks to run even if they already succeeded")
	cmd.Flags().BoolVar(&sessionHooks, "session", false, "target session hooks")
	cmd.Flags().StringSliceVar(&phases, "phase", nil, "phase selector for aggregate targeting; repeat or comma-separate, \"all\" selects every phase")
	return cmd
}

func (a *app) newPauseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pause [HOOK_ID ...]",
		Short: "Pause all hook execution or selected hooks",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			ids, all, err := targetHookIDs(cmd.Context(), c, args)
			if err != nil {
				return err
			}
			if all {
				err = c.PauseAll(cmd.Context())
			} else {
				for _, id := range ids {
					if err = c.PauseHook(cmd.Context(), id); err != nil {
						return err
					}
				}
			}
			if err != nil {
				return err
			}
			return a.writeTargetAction(cmd, true, all, ids)
		},
	}
}

func (a *app) newResumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "resume [HOOK_ID ...]",
		Short: "Resume all hook execution or selected hooks",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			ids, all, err := targetHookIDs(cmd.Context(), c, args)
			if err != nil {
				return err
			}
			if all {
				err = c.ResumeAll(cmd.Context())
			} else {
				for _, id := range ids {
					if err = c.ResumeHook(cmd.Context(), id); err != nil {
						return err
					}
				}
			}
			if err != nil {
				return err
			}
			return a.writeTargetAction(cmd, false, all, ids)
		},
	}
}

type hookOutput struct {
	HookID string `json:"hook_id"`
	Output string `json:"output"`
}

func (a *app) newOutputCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "output [HOOK_ID ...]",
		Short: "Print captured output for all hooks or selected hooks",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			ids, all, err := targetHookIDs(cmd.Context(), c, args)
			if err != nil {
				return err
			}
			outputs := make([]hookOutput, 0, len(ids))
			for _, id := range ids {
				b, err := c.Output(cmd.Context(), id)
				if err != nil {
					return err
				}
				outputs = append(outputs, hookOutput{HookID: id, Output: string(b)})
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"all": all, "outputs": outputs})
			}
			return writeHookOutputs(cmd.OutOrStdout(), outputs)
		},
	}
}

type checkHookOutput struct {
	HookID      string        `json:"hook_id"`
	Name        string        `json:"name,omitempty"`
	Description string        `json:"description,omitempty"`
	Type        string        `json:"type,omitempty"`
	Pattern     string        `json:"pattern,omitempty"`
	Phase       string        `json:"phase,omitempty"`
	Path        string        `json:"path,omitempty"`
	Status      models.Status `json:"status"`
	Output      string        `json:"output"`
}

var errCheckFailed = errors.New("hooks check failed")

func (a *app) newCheckCommand() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Wait for hook work to finish and report non-successful hooks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			resp, err := a.waitForCheck(cmd.Context(), c, timeout, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			failed := failedHooks(resp.Hooks)
			outputs, err := a.checkOutputs(cmd.Context(), c, failed)
			if err != nil {
				return err
			}
			if a.opts.output == "json" {
				if err := writeJSON(cmd.OutOrStdout(), map[string]any{"settled": resp.Settled, "running": resp.Running, "queued": resp.Queued, "pending_changes": resp.PendingChanges, "pending_snapshot": resp.PendingSnapshot, "pending_lsp": resp.PendingLSP, "failed": outputs}); err != nil {
					return err
				}
			} else {
				if err := writeCheckResult(cmd.OutOrStdout(), resp, outputs); err != nil {
					return err
				}
			}
			if !resp.Settled || len(outputs) > 0 {
				return errCheckFailed
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "maximum time to wait for terminal hook state")
	return cmd
}

const checkProgressInterval = 2 * time.Second
const checkReconnectDelay = 250 * time.Millisecond

func (a *app) waitForCheck(ctx context.Context, c *client.Client, timeout time.Duration, progress io.Writer) (*client.WaitResponse, error) {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	current := c
	for {
		remaining := remainingCheckTimeout(timeout, deadline)
		resp, err := a.waitForCheckOnce(ctx, current, remaining, progress)
		if err == nil {
			return resp, nil
		}
		if !isRetryableCheckWaitError(err) {
			return nil, err
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return nil, fmt.Errorf("hook daemon disconnected while waiting for checks to settle and the check timeout expired: %w", err)
		}
		if progress != nil && a.opts.output != "json" {
			fmt.Fprintf(progress, "hook daemon disconnected while waiting; reconnecting: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(checkReconnectDelay):
		}
		var reconnectErr error
		current, _, reconnectErr = a.client(ctx)
		if reconnectErr != nil {
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				return nil, fmt.Errorf("reconnect hook daemon after wait disconnect: %w", reconnectErr)
			}
			if progress != nil && a.opts.output != "json" {
				fmt.Fprintf(progress, "waiting for hook daemon to reconnect: %v\n", reconnectErr)
			}
			continue
		}
	}
}

func remainingCheckTimeout(original time.Duration, deadline time.Time) time.Duration {
	if original <= 0 || deadline.IsZero() {
		return original
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

func (a *app) waitForCheckOnce(ctx context.Context, c *client.Client, timeout time.Duration, progress io.Writer) (*client.WaitResponse, error) {
	if a.opts.output == "json" {
		return c.Wait(ctx, timeout)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type waitResult struct {
		resp *client.WaitResponse
		err  error
	}
	resultCh := make(chan waitResult, 1)
	go func() {
		resp, err := c.Wait(waitCtx, timeout)
		resultCh <- waitResult{resp: resp, err: err}
	}()
	ticker := time.NewTicker(checkProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case result := <-resultCh:
			return result.resp, result.err
		case <-ticker.C:
			st, err := c.Status(ctx)
			if err == nil {
				writeCheckProgress(progress, st)
			}
		}
	}
}

func isRetryableCheckWaitError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, client.ErrNotRunning) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "malformed http response") ||
		strings.Contains(msg, "protocol error") ||
		strings.Contains(msg, "server closed idle connection")
}

func (a *app) checkOutputs(ctx context.Context, c *client.Client, hooksList []client.HookStatus) ([]checkHookOutput, error) {
	outputs := make([]checkHookOutput, 0, len(hooksList))
	for _, h := range hooksList {
		output := ""
		if h.Hook.Engine == hooksapi.HookEngineLSP {
			diagnostics, err := c.ListDiagnostics(ctx, client.DiagnosticOptions{HookID: h.Hook.ID})
			if err != nil {
				return nil, err
			}
			output = diagnosticsOutput(diagnostics)
		} else if h.LastRunID != "" {
			b, err := c.Output(ctx, h.Hook.ID)
			if err != nil {
				return nil, err
			}
			output = string(b)
		}
		outputs = append(outputs, checkHookOutput{
			HookID:      h.Hook.ID,
			Name:        h.Hook.Name,
			Description: h.Hook.Description,
			Type:        string(h.Hook.Type),
			Pattern:     h.Hook.Pattern,
			Phase:       h.Hook.Phase,
			Path:        h.Hook.RelPath,
			Status:      h.Status,
			Output:      output,
		})
	}
	return outputs, nil
}

func (a *app) newRunsCommand() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "runs [HOOK_ID]",
		Short: "List hook run history",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			hookID := ""
			if len(args) > 0 {
				hookID = args[0]
			}
			runs, err := c.ListRuns(cmd.Context(), client.RunListOptions{HookID: hookID, Limit: limit, LimitSet: true})
			if err != nil {
				return err
			}
			return a.writeRuns(cmd, runs)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum rows to return; 0 means no limit")
	return cmd
}

func (a *app) newChangesCommand() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "changes",
		Short: "List observed file changes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			changes, err := c.ListObservedChanges(cmd.Context(), client.ListOptions{Limit: limit, LimitSet: true})
			if err != nil {
				return err
			}
			return a.writeObservedChanges(cmd, changes)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum rows to return; 0 means no limit")
	return cmd
}

func (a *app) newDiagnosticsCommand() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "diagnostics [HOOK_ID]",
		Short: "List current LSP diagnostics",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			opts := client.DiagnosticOptions{Limit: limit, LimitSet: true}
			if len(args) > 0 {
				opts.HookID = args[0]
			}
			diagnostics, err := c.ListDiagnostics(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return a.writeDiagnostics(cmd, diagnostics)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum rows to return; 0 means no limit")
	return cmd
}

func (a *app) newLSPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lsp",
		Short: "Inspect LSP hooks",
	}
	cmd.AddCommand(a.newLSPStatusCommand())
	cmd.AddCommand(a.newLSPDiagnosticsCommand())
	cmd.AddCommand(a.newLSPEventsCommand())
	return cmd
}

func (a *app) newLSPStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show LSP hook status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			hooksList, err := c.ListHooks(cmd.Context())
			if err != nil {
				return err
			}
			diagnostics, err := c.ListDiagnostics(cmd.Context(), client.DiagnosticOptions{Limit: 0, LimitSet: true})
			if err != nil {
				return err
			}
			return a.writeLSPStatus(cmd, lspStatusRows(hooksList, diagnostics))
		},
	}
}

func (a *app) newLSPDiagnosticsCommand() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "diagnostics [HOOK_ID]",
		Short: "List current LSP diagnostics",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			opts := client.DiagnosticOptions{Limit: limit, LimitSet: true}
			if len(args) > 0 {
				opts.HookID = args[0]
			}
			diagnostics, err := c.ListDiagnostics(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return a.writeDiagnostics(cmd, diagnostics)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum rows to return; 0 means no limit")
	return cmd
}

func (a *app) newLSPEventsCommand() *cobra.Command {
	var limit int
	var follow bool
	var listTypes bool
	cmd := &cobra.Command{
		Use:     "events [HOOK_ID ...]",
		Aliases: []string{"event"},
		Short:   "List or follow LSP hook events",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if listTypes {
				if follow {
					return fmt.Errorf("lsp events --list-types cannot be combined with --follow")
				}
				if len(args) > 0 {
					return fmt.Errorf("lsp events --list-types does not accept hook ids")
				}
				return a.writeEventTypes(cmd, filterEventTypes(hookapi.KnownEventTypes(), isLSPEventType))
			}
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			if follow {
				return a.followLSPEvents(cmd, c, args, limit)
			}
			events, err := listTargetEvents(cmd.Context(), c, args, limit)
			if err != nil {
				return err
			}
			return a.writeEvents(cmd, filterEvents(events, isLSPEvent))
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum events to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow new events")
	cmd.Flags().BoolVar(&listTypes, "list-types", false, "list known LSP event types")
	return cmd
}

func (a *app) newSnapshotsCommand() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "snapshots",
		Short: "List and apply workspace snapshots",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			snapshots, err := c.ListWorkspaceSnapshots(cmd.Context(), client.ListOptions{Limit: limit, LimitSet: true})
			if err != nil {
				return err
			}
			return a.writeSnapshots(cmd, snapshots)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum rows to return; 0 means no limit")
	cmd.AddCommand(a.newSnapshotStatCommand())
	cmd.AddCommand(a.newSnapshotDiffCommand())
	cmd.AddCommand(a.newSnapshotApplyCommand())
	cmd.AddCommand(a.newSnapshotResetCommand())
	return cmd
}

func (a *app) newSnapshotStatCommand() *cobra.Command {
	var parent bool
	cmd := &cobra.Command{
		Use:   "stat [FROM_SNAPSHOT_ID] [TO_SNAPSHOT_ID]",
		Short: "Show a short diffstat between the current workspace and a snapshot, or between two snapshots",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, paths, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			diffRange, err := selectedSnapshotDiffRange(cmd.Context(), c, args, parent)
			if err != nil {
				return err
			}
			diff, err := snapshotRangeDiff(cmd.Context(), paths.RepoRoot, diffRange, gitDiffColorArg(cmd.OutOrStdout(), a.opts.output == "table"), "--stat")
			if err != nil {
				return err
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"from_snapshot_id": diffRange.fromSnapshotID(), "to_snapshot_id": diffRange.To.ID, "from_current": diffRange.FromCurrent, "from_base": diffRange.FromBase, "stat": diff})
			}
			_, err = io.WriteString(cmd.OutOrStdout(), diff)
			return err
		},
	}
	cmd.Flags().BoolVarP(&parent, "parent", "p", false, "compare the selected snapshot against its parent")
	return cmd
}

func (a *app) newSnapshotDiffCommand() *cobra.Command {
	var parent bool
	cmd := &cobra.Command{
		Use:   "diff [FROM_SNAPSHOT_ID] [TO_SNAPSHOT_ID]",
		Short: "Show the full diff between the current workspace and a snapshot, or between two snapshots",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, paths, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			diffRange, err := selectedSnapshotDiffRange(cmd.Context(), c, args, parent)
			if err != nil {
				return err
			}
			diff, err := snapshotRangeDiff(cmd.Context(), paths.RepoRoot, diffRange, gitDiffColorArg(cmd.OutOrStdout(), a.opts.output == "table"), "--binary")
			if err != nil {
				return err
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"from_snapshot_id": diffRange.fromSnapshotID(), "to_snapshot_id": diffRange.To.ID, "from_current": diffRange.FromCurrent, "from_base": diffRange.FromBase, "diff": diff})
			}
			_, err = io.WriteString(cmd.OutOrStdout(), diff)
			return err
		},
	}
	cmd.Flags().BoolVarP(&parent, "parent", "p", false, "compare the selected snapshot against its parent")
	return cmd
}

func (a *app) newSnapshotApplyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "apply [SNAPSHOT_ID]",
		Short: "Apply a snapshot to the workspace as unstaged changes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, paths, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			snapshot, err := selectedSnapshot(cmd.Context(), c, args)
			if err != nil {
				return err
			}
			changed, err := applySnapshotDiff(cmd.Context(), paths.RepoRoot, snapshot)
			if err != nil {
				return err
			}
			return a.writeAction(cmd, map[string]any{"snapshot_id": snapshot.ID, "applied": changed}, snapshotActionMessage("applied", snapshot.ID, changed))
		},
	}
}

func (a *app) newSnapshotResetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reset [SNAPSHOT_ID]",
		Short: "Reset a snapshot's changes from the workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, paths, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			snapshot, err := selectedSnapshot(cmd.Context(), c, args)
			if err != nil {
				return err
			}
			changed, err := resetSnapshotDiff(cmd.Context(), paths.RepoRoot, snapshot)
			if err != nil {
				return err
			}
			return a.writeAction(cmd, map[string]any{"snapshot_id": snapshot.ID, "reset": changed}, snapshotActionMessage("reset", snapshot.ID, changed))
		},
	}
}

func (a *app) newQueueCommand() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "queue",
		Aliases: []string{"queued", "pending"},
		Short:   "List queued hook work",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			pending, err := c.ListQueue(cmd.Context(), client.ListOptions{Limit: limit, LimitSet: true})
			if err != nil {
				return err
			}
			return a.writeQueue(cmd, pending)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum rows to return; 0 means no limit")
	return cmd
}

func (a *app) newShutdownCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Ask the hook daemon to exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.Shutdown(cmd.Context()); err != nil {
				return err
			}
			return a.writeAction(cmd, map[string]any{"shutdown": true}, "shutdown requested")
		},
	}
}

func (a *app) sessionPaths(ctx context.Context) (sessionPaths, error) {
	root := a.opts.repoRoot
	var err error
	if root == "" {
		root, err = gitRoot(ctx)
		if err != nil {
			return sessionPaths{}, fmt.Errorf("resolve git root: %w", err)
		}
	}
	root = filepath.Clean(root)
	return computeSessionPaths(root, resolveSessionID(a.opts.sessionID, root)), nil
}

func (a *app) client(ctx context.Context) (*client.Client, sessionPaths, error) {
	paths, err := a.sessionPaths(ctx)
	if err != nil {
		return nil, sessionPaths{}, err
	}
	c := client.New(paths.Socket)
	if !a.opts.noStart {
		if err := ensureDaemon(ctx, c, paths); err != nil {
			return nil, sessionPaths{}, err
		}
	}
	return c, paths, nil
}

func (a *app) writeStatus(cmd *cobra.Command, st *client.StatusResponse) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), st)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tREPO\tPAUSED\tRUNNING\tQUEUED\tHOOKS")
	fmt.Fprintf(tw, "%s\t%s\t%t\t%t\t%d\t%d\n", st.SessionID, st.RepoRoot, st.Paused, st.Running, st.Queued, len(st.Hooks))
	return tw.Flush()
}

func (a *app) writeHooks(cmd *cobra.Command, hooks []client.HookStatus) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"hooks": hooks})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tENGINE\tSTATUS\tPAUSED\tRUNS\tFAILURES\tPATTERN")
	for _, h := range hooks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%t\t%d\t%d\t%s\n", h.Hook.ID, h.Hook.Name, h.Hook.Type, h.Hook.Engine, h.Status, h.Paused, h.RunCount, h.FailCount, h.Hook.Pattern)
	}
	return tw.Flush()
}

func (a *app) writeEvents(cmd *cobra.Command, events []client.Event) error {
	if a.opts.output == "json" {
		return writeJSONLines(cmd.OutOrStdout(), events)
	}
	events = sortByCreatedAtAsc(events, func(event client.Event) time.Time { return event.CreatedAt }, func(event client.Event) string { return event.ID })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tTYPE\tHOOK\tRUN\tMESSAGE")
	for _, event := range events {
		writeEventTableRow(tw, event, formatEventTime)
	}
	return tw.Flush()
}

func (a *app) writeEventTypes(cmd *cobra.Command, types []hookapi.EventTypeInfo) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"event_types": types})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tDESCRIPTION\tDETAILS")
	for _, eventType := range types {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", eventType.Type, eventType.Description, eventDetailNames(eventType.Details))
	}
	return tw.Flush()
}

func (a *app) writeRuns(cmd *cobra.Command, runs []client.Run) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"runs": runs})
	}
	runs = sortByCreatedAtAsc(runs, func(run client.Run) time.Time { return run.StartedAt }, func(run client.Run) string { return run.ID })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tHOOK\tSTATUS\tEXIT\tSTARTED\tFINISHED\tFILES\tCHANGES\tERROR")
	for _, run := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t%d\t%s\n", run.ID, run.HookID, run.Status, run.ExitCode, formatEventTime(run.StartedAt), formatOptionalTime(run.FinishedAt), len(run.ChangedFiles), len(run.ChangeIDs), run.Error)
	}
	return tw.Flush()
}

func (a *app) writeObservedChanges(cmd *cobra.Command, changes []client.ObservedFileChange) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"changes": changes})
	}
	changes = sortByCreatedAtAsc(changes, func(change client.ObservedFileChange) time.Time { return change.CreatedAt }, func(change client.ObservedFileChange) string { return change.ID })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tPATH\tKIND\tBASE\tDIFF_BYTES")
	for _, change := range changes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", change.ID, formatEventTime(change.CreatedAt), change.Path, change.Kind, change.BaseCommit, len(change.Diff))
	}
	return tw.Flush()
}

func (a *app) writeDiagnostics(cmd *cobra.Command, diagnostics []client.Diagnostic) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"diagnostics": diagnostics})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOOK\tSEVERITY\tLOCATION\tSOURCE\tCODE\tMESSAGE")
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", diagnostic.HookID, diagnostic.Severity, diagnosticLocation(diagnostic), diagnostic.Source, diagnostic.Code, oneLine(diagnostic.Message))
	}
	return tw.Flush()
}

type lspStatusRow struct {
	HookID          string              `json:"hook_id"`
	Name            string              `json:"name"`
	LanguageID      string              `json:"language_id,omitempty"`
	Pattern         string              `json:"pattern,omitempty"`
	Status          models.Status       `json:"status"`
	Paused          bool                `json:"paused"`
	DiagnosticCount int                 `json:"diagnostic_count"`
	LastError       string              `json:"last_error,omitempty"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Hook            hooksapi.Hook       `json:"hook"`
	Diagnostics     []client.Diagnostic `json:"diagnostics,omitempty"`
}

func (a *app) writeLSPStatus(cmd *cobra.Command, rows []lspStatusRow) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"lsp_hooks": rows})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tLANGUAGE\tSTATUS\tPAUSED\tDIAGNOSTICS\tERROR\tUPDATED\tPATTERN")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%d\t%s\t%s\t%s\n", row.HookID, row.Name, row.LanguageID, row.Status, row.Paused, row.DiagnosticCount, oneLine(row.LastError), formatEventTime(row.UpdatedAt), row.Pattern)
	}
	return tw.Flush()
}

func (a *app) writeSnapshots(cmd *cobra.Command, snapshots []client.WorkspaceSnapshot) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"snapshots": snapshots})
	}
	snapshots = sortByCreatedAtAsc(snapshots, func(snapshot client.WorkspaceSnapshot) time.Time { return snapshot.CreatedAt }, func(snapshot client.WorkspaceSnapshot) string { return snapshot.ID })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tBASE\tPATCH_BYTES\tCHANGED\tOMITTED\tOBSERVED")
	for _, snapshot := range snapshots {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\n", snapshot.ID, formatEventTime(snapshot.CreatedAt), formatShortCommit(snapshot.BaseCommit), snapshot.PatchBytes, len(snapshot.ChangedFiles), len(snapshot.OmittedFiles), len(snapshot.ObservedChangeIDs))
	}
	return tw.Flush()
}

func (a *app) writeQueue(cmd *cobra.Command, rows []client.QueuedHook) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"queue": rows})
	}
	rows = sortByCreatedAtAsc(rows, func(row client.QueuedHook) time.Time { return row.CreatedAt }, func(row client.QueuedHook) string { return row.HookID })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POSITION\tHOOK\tBLOCKED\tBLOCKED_BY\tFILES\tCHANGES\tCREATED\tUPDATED")
	for _, row := range rows {
		fmt.Fprintf(tw, "%d\t%s\t%t\t%s\t%d\t%d\t%s\t%s\n", row.Position, row.HookID, row.Blocked, row.BlockedByHookID, len(row.ChangedFiles), len(row.ChangeIDs), formatEventTime(row.CreatedAt), formatEventTime(row.UpdatedAt))
	}
	return tw.Flush()
}

func writeCheckResult(w io.Writer, resp *client.WaitResponse, outputs []checkHookOutput) error {
	if len(outputs) == 0 && resp.Settled {
		_, err := fmt.Fprintln(w, "all hooks successful")
		return err
	}
	if !resp.Settled {
		fmt.Fprintf(w, "hook daemon unsettled: running=%t queued=%d pending_changes=%t pending_snapshot=%t pending_lsp=%t\n", resp.Running, resp.Queued, resp.PendingChanges, resp.PendingSnapshot, resp.PendingLSP)
	}
	if len(outputs) == 0 {
		return nil
	}
	for i, out := range outputs {
		if i > 0 || !resp.Settled {
			fmt.Fprintln(w)
		}
		writeCheckFailure(w, out)
	}
	return nil
}

func writeCheckProgress(w io.Writer, resp *client.StatusResponse) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "waiting for hooks: running=%s queued=%s paused=%t\n", checkRunningSummary(resp), checkQueuedSummary(resp), resp.Paused)
}

func checkRunningSummary(resp *client.StatusResponse) string {
	ids := hookIDsWithStatus(resp.Hooks, models.StatusRunning)
	if ids != "-" {
		return ids
	}
	if resp.Running {
		return "yes"
	}
	return "-"
}

func checkQueuedSummary(resp *client.StatusResponse) string {
	ids := hookIDsWithStatus(resp.Hooks, models.StatusQueued)
	if ids != "-" {
		return ids
	}
	if resp.Queued > 0 {
		return fmt.Sprintf("%d", resp.Queued)
	}
	return "-"
}

func hookIDsWithStatus(hooksList []client.HookStatus, status models.Status) string {
	ids := make([]string, 0)
	for _, h := range hooksList {
		if h.Status == status {
			ids = append(ids, h.Hook.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ",")
}

func writeCheckFailure(w io.Writer, out checkHookOutput) {
	fmt.Fprintf(w, "=== FAILED HOOK %s ===\n", out.HookID)
	fmt.Fprintln(w, "--- metadata ---")
	if out.Name != "" {
		fmt.Fprintf(w, "name: %s\n", out.Name)
	}
	if out.Description != "" {
		fmt.Fprintf(w, "description: %s\n", out.Description)
	}
	if out.Type != "" {
		fmt.Fprintf(w, "type: %s\n", out.Type)
	}
	if out.Pattern != "" {
		fmt.Fprintf(w, "pattern: %s\n", out.Pattern)
	}
	if out.Phase != "" {
		fmt.Fprintf(w, "phase: %s\n", out.Phase)
	}
	if out.Path != "" {
		fmt.Fprintf(w, "hook_file: %s\n", out.Path)
	}
	fmt.Fprintln(w, "--- output ---")
	if out.Output == "" {
		fmt.Fprintln(w, "(empty)")
	} else {
		_, _ = io.WriteString(w, out.Output)
		if !strings.HasSuffix(out.Output, "\n") {
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w, "--- end output ---")
	fmt.Fprintf(w, "=== END FAILED HOOK %s ===\n", out.HookID)
}

func diagnosticsOutput(diagnostics []client.Diagnostic) string {
	if len(diagnostics) == 0 {
		return ""
	}
	var b strings.Builder
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(&b, "%s: %s: %s", diagnosticLocation(diagnostic), diagnostic.Severity, diagnostic.Message)
		if diagnostic.Source != "" || diagnostic.Code != "" {
			fmt.Fprintf(&b, " [%s%s]", diagnostic.Source, diagnostic.Code)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func diagnosticLocation(diagnostic client.Diagnostic) string {
	line := diagnostic.StartLine
	if line <= 0 {
		line = 1
	}
	col := diagnostic.StartCol
	if col <= 0 {
		col = 1
	}
	return fmt.Sprintf("%s:%d:%d", diagnostic.Path, line, col)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func eventDetailNames(details []hookapi.EventDetailInfo) string {
	if len(details) == 0 {
		return ""
	}
	names := make([]string, 0, len(details))
	for _, detail := range details {
		if detail.Required {
			names = append(names, detail.Name+"*")
		} else {
			names = append(names, detail.Name)
		}
	}
	return strings.Join(names, ",")
}

func (a *app) followEvents(cmd *cobra.Command, c *client.Client, args []string, limit int) error {
	hookID, err := followEventHookID(args)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	var tw *tabwriter.Writer
	if a.opts.output == "table" {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTIME\tTYPE\tHOOK\tRUN\tMESSAGE")
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return c.FollowEvents(cmd.Context(), a.followOptions(cmd, hookID, limit), func(event client.Event) error {
		if a.opts.output == "json" {
			return writeJSONLine(w, event)
		}
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		writeEventTableRow(tw, event, formatLiveEventTime)
		return tw.Flush()
	})
}

// followOptions builds the follow request and reports reconnects on stderr, so
// a daemon restart shows up as a notice instead of ending the follow.
func (a *app) followOptions(cmd *cobra.Command, hookID string, limit int) client.EventOptions {
	return client.EventOptions{
		HookID: hookID,
		Limit:  limit,
		OnDisconnect: func(err error, attempt int) {
			fmt.Fprintf(cmd.ErrOrStderr(), "hook daemon connection lost (%s); reconnecting (attempt %d)\n", oneLine(err.Error()), attempt)
		},
	}
}

func (a *app) followLSPEvents(cmd *cobra.Command, c *client.Client, args []string, limit int) error {
	hookID, err := followEventHookID(args)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	var tw *tabwriter.Writer
	if a.opts.output == "table" {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTIME\tTYPE\tHOOK\tRUN\tMESSAGE")
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return c.FollowEvents(cmd.Context(), a.followOptions(cmd, hookID, limit), func(event client.Event) error {
		if !isLSPEvent(event) {
			return nil
		}
		if a.opts.output == "json" {
			return writeJSONLine(w, event)
		}
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		writeEventTableRow(tw, event, formatLiveEventTime)
		return tw.Flush()
	})
}

func (a *app) writeAction(cmd *cobra.Command, value any, message string) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), value)
	}
	fmt.Fprintln(cmd.OutOrStdout(), message)
	return nil
}

func selectedSnapshot(ctx context.Context, c *client.Client, args []string) (client.WorkspaceSnapshot, error) {
	id := "latest"
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		id = strings.TrimSpace(args[0])
	}
	snapshots, err := c.ListWorkspaceSnapshots(ctx, client.ListOptions{Limit: 0, LimitSet: true})
	if err != nil {
		return client.WorkspaceSnapshot{}, err
	}
	if len(snapshots) == 0 {
		return client.WorkspaceSnapshot{}, fmt.Errorf("no workspace snapshots found")
	}
	if strings.EqualFold(id, "latest") {
		id = latestSnapshot(snapshots).ID
	}
	byID := make(map[string]client.WorkspaceSnapshot, len(snapshots))
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		byID[snapshot.ID] = snapshot
		ids = append(ids, snapshot.ID)
	}
	matches := make([]client.WorkspaceSnapshot, 0, 1)
	for _, matchID := range idpkg.ResolveShort(id, ids) {
		matches = append(matches, byID[matchID])
	}
	if len(matches) == 0 {
		return client.WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot %q not found", id)
	}
	if len(matches) > 1 {
		return client.WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot short ID %q is ambiguous", id)
	}
	fullSnapshot, err := c.GetWorkspaceSnapshot(ctx, matches[0].ID)
	if err != nil {
		return client.WorkspaceSnapshot{}, err
	}
	if strings.TrimSpace(fullSnapshot.TreeHash) == "" {
		return client.WorkspaceSnapshot{}, fmt.Errorf("snapshot %s has no tree hash", matches[0].ID)
	}
	return *fullSnapshot, nil
}

func latestSnapshot(snapshots []client.WorkspaceSnapshot) client.WorkspaceSnapshot {
	latest := snapshots[0]
	for _, snapshot := range snapshots[1:] {
		if snapshot.CreatedAt.After(latest.CreatedAt) || (snapshot.CreatedAt.Equal(latest.CreatedAt) && snapshot.ID > latest.ID) {
			latest = snapshot
		}
	}
	return latest
}

type snapshotDiffRange struct {
	From        *client.WorkspaceSnapshot `json:"from_snapshot,omitempty"`
	To          client.WorkspaceSnapshot  `json:"to_snapshot"`
	FromCurrent bool                      `json:"from_current"`
	FromBase    bool                      `json:"from_base"`
}

func (r snapshotDiffRange) fromSnapshotID() string {
	if r.From == nil {
		return ""
	}
	return r.From.ID
}

func selectedSnapshotDiffRange(ctx context.Context, c *client.Client, args []string, parent bool) (snapshotDiffRange, error) {
	if parent {
		if len(args) > 1 {
			return snapshotDiffRange{}, fmt.Errorf("snapshots --parent accepts at most one snapshot id")
		}
		target, err := selectedSnapshot(ctx, c, args)
		if err != nil {
			return snapshotDiffRange{}, err
		}
		parentID := strings.TrimSpace(target.ParentID)
		if parentID == "" {
			return snapshotDiffRange{}, fmt.Errorf("snapshot %s has no parent", target.ID)
		}
		parentSnapshot, err := selectedSnapshot(ctx, c, []string{parentID})
		if err != nil {
			return snapshotDiffRange{}, fmt.Errorf("load parent snapshot %s: %w", parentID, err)
		}
		return snapshotDiffRange{From: &parentSnapshot, To: target}, nil
	}
	if len(args) == 2 {
		from, err := selectedSnapshot(ctx, c, []string{args[0]})
		if err != nil {
			return snapshotDiffRange{}, err
		}
		to, err := selectedSnapshot(ctx, c, []string{args[1]})
		if err != nil {
			return snapshotDiffRange{}, err
		}
		return snapshotDiffRange{From: &from, To: to}, nil
	}
	to, err := selectedSnapshot(ctx, c, args)
	if err != nil {
		return snapshotDiffRange{}, err
	}
	return snapshotDiffRange{To: to, FromBase: true}, nil
}

func snapshotDiff(ctx context.Context, repoRoot string, snapshot client.WorkspaceSnapshot, diffArgs ...string) (string, error) {
	return snapshotRangeDiff(ctx, repoRoot, snapshotDiffRange{To: snapshot, FromBase: true}, diffArgs...)
}

func snapshotRangeDiff(ctx context.Context, repoRoot string, diffRange snapshotDiffRange, diffArgs ...string) (string, error) {
	fromTree, cleanup, err := snapshotRangeFromTree(ctx, repoRoot, diffRange)
	if err != nil {
		return "", err
	}
	defer cleanup()
	toTree, err := ensureSnapshotTree(ctx, repoRoot, diffRange.To)
	if err != nil {
		return "", err
	}
	args := []string{"diff"}
	for _, arg := range diffArgs {
		if arg != "" {
			args = append(args, arg)
		}
	}
	args = append(args, fromTree, toTree)
	return gitutil.Output(ctx, repoRoot, nil, nil, args...)
}

func snapshotRangeFromTree(ctx context.Context, repoRoot string, diffRange snapshotDiffRange) (string, func(), error) {
	if diffRange.FromCurrent {
		workspaceTree, cleanup, err := gitutil.CurrentWorkspaceTree(ctx, repoRoot)
		if err != nil {
			return "", cleanup, err
		}
		return workspaceTree.Tree, cleanup, nil
	}
	if diffRange.FromBase {
		base := strings.TrimSpace(diffRange.To.BaseCommit)
		if base == "" {
			return "", func() {}, fmt.Errorf("snapshot %s has no base commit", diffRange.To.ID)
		}
		baseTree, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", base+"^{tree}")
		return strings.TrimSpace(baseTree), func() {}, err
	}
	if diffRange.From == nil {
		return "", func() {}, fmt.Errorf("from snapshot is required")
	}
	tree, err := ensureSnapshotTree(ctx, repoRoot, *diffRange.From)
	return tree, func() {}, err
}

func applySnapshotDiff(ctx context.Context, repoRoot string, snapshot client.WorkspaceSnapshot) (bool, error) {
	workspaceTree, cleanup, err := gitutil.CurrentWorkspaceTree(ctx, repoRoot)
	if err != nil {
		return false, err
	}
	defer cleanup()
	snapshotTree, err := ensureSnapshotTree(ctx, repoRoot, snapshot)
	if err != nil {
		return false, err
	}
	patch, err := gitutil.Output(ctx, repoRoot, nil, nil, "diff", "--binary", workspaceTree.Tree, snapshotTree)
	if err != nil {
		return false, err
	}
	return applyPatch(ctx, repoRoot, patch)
}

func resetSnapshotDiff(ctx context.Context, repoRoot string, snapshot client.WorkspaceSnapshot) (bool, error) {
	base := strings.TrimSpace(snapshot.BaseCommit)
	if base == "" {
		return false, fmt.Errorf("snapshot %s has no base commit", snapshot.ID)
	}
	snapshotTree, err := ensureSnapshotTree(ctx, repoRoot, snapshot)
	if err != nil {
		return false, err
	}
	baseTree, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", base+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("resolve snapshot base tree: %w", err)
	}
	patch, err := gitutil.Output(ctx, repoRoot, nil, nil, "diff", "--binary", snapshotTree, strings.TrimSpace(baseTree))
	if err != nil {
		return false, err
	}
	return applyPatch(ctx, repoRoot, patch)
}

func ensureSnapshotTree(ctx context.Context, repoRoot string, snapshot client.WorkspaceSnapshot) (string, error) {
	tree := strings.TrimSpace(snapshot.TreeHash)
	if tree == "" {
		return "", fmt.Errorf("snapshot %s has no tree hash", snapshot.ID)
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "cat-file", "-e", tree+"^{tree}"); err == nil {
		return tree, nil
	}
	if strings.TrimSpace(snapshot.Patch) == "" {
		return "", fmt.Errorf("snapshot %s tree %s is unavailable and no patch payload was returned", snapshot.ID, tree)
	}
	reconstructed, cleanup, err := reconstructSnapshotTree(ctx, repoRoot, snapshot)
	if err != nil {
		return "", err
	}
	cleanup()
	return reconstructed, nil
}

func reconstructSnapshotTree(ctx context.Context, repoRoot string, snapshot client.WorkspaceSnapshot) (string, func(), error) {
	base := strings.TrimSpace(snapshot.BaseCommit)
	if base == "" {
		return "", func() {}, fmt.Errorf("snapshot %s has no base commit", snapshot.ID)
	}
	tempDir, err := os.MkdirTemp("", "discobox-hooks-snapshot-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temporary snapshot index directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	indexFile, err := os.CreateTemp(tempDir, "index-*")
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("create temporary snapshot index: %w", err)
	}
	indexPath := indexFile.Name()
	_ = indexFile.Close()
	env := map[string]string{"GIT_INDEX_FILE": indexPath}
	if _, err := gitutil.Output(ctx, repoRoot, nil, env, "read-tree", base); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("read snapshot base into temporary index: %w", err)
	}
	if _, err := gitutil.Output(ctx, repoRoot, []byte(snapshot.Patch), env, "apply", "--cached", "--binary"); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("reconstruct snapshot tree from patch: %w", err)
	}
	tree, err := gitutil.Output(ctx, repoRoot, nil, env, "write-tree")
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("write reconstructed snapshot tree: %w", err)
	}
	return strings.TrimSpace(tree), cleanup, nil
}

func applyPatch(ctx context.Context, repoRoot, patch string) (bool, error) {
	if strings.TrimSpace(patch) == "" {
		return false, nil
	}
	if _, err := gitutil.Output(ctx, repoRoot, []byte(patch), nil, "apply", "--binary"); err != nil {
		return false, fmt.Errorf("apply snapshot patch: %w", err)
	}
	return true, nil
}

func snapshotActionMessage(action, id string, changed bool) string {
	if !changed {
		return fmt.Sprintf("snapshot %s already matches workspace", id)
	}
	return fmt.Sprintf("snapshot %s %s", id, action)
}

func (a *app) writeTargetAction(cmd *cobra.Command, paused, all bool, ids []string) error {
	verb := "resumed"
	if paused {
		verb = "paused"
	}
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"all": all, "hook_ids": ids, "paused": paused})
	}
	if all {
		fmt.Fprintf(cmd.OutOrStdout(), "%s all hooks\n", verb)
		return nil
	}
	for _, id := range ids {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", verb, id)
	}
	return nil
}

func targetHookIDs(ctx context.Context, c *client.Client, args []string) ([]string, bool, error) {
	if len(args) == 0 || isAllArg(args) {
		hooks, err := c.ListHooks(ctx)
		if err != nil {
			return nil, false, err
		}
		ids := make([]string, 0, len(hooks))
		for _, h := range hooks {
			if h.Hook.ID != "" {
				ids = append(ids, h.Hook.ID)
			}
		}
		return ids, true, nil
	}
	return uniqueStrings(args), false, nil
}

type runTargetOptions struct {
	SessionHooks bool
	Phases       []string
	AllPhases    bool
}

// normalizePhaseSelector lowercases, trims, and dedupes the --phase values and
// reports whether the selector includes every phase.
func normalizePhaseSelector(values []string) ([]string, bool) {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	all := false
	for _, value := range values {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			continue
		}
		if value == "all" {
			all = true
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, all
}

// splitRunArgs separates explicit hook IDs from the "all" ID selector.
func splitRunArgs(args []string) ([]string, bool) {
	ids := make([]string, 0, len(args))
	all := false
	for _, arg := range args {
		if strings.EqualFold(strings.TrimSpace(arg), "all") {
			all = true
			continue
		}
		ids = append(ids, arg)
	}
	return uniqueStrings(ids), all
}

// filterRunTargets selects hook IDs in the phase scope: unphased hooks when no
// phases are selected, hooks in the selected phases otherwise, and every hook
// when AllPhases is set. Run eligibility is decided by the daemon per hook.
func filterRunTargets(hooks []client.HookStatus, opts runTargetOptions) []string {
	ids := make([]string, 0, len(hooks))
	for _, h := range hooks {
		if h.Hook.ID == "" {
			continue
		}
		if opts.SessionHooks && h.Hook.Type != hooksapi.HookTypeSession {
			continue
		}
		hookPhase := strings.TrimSpace(strings.ToLower(h.Hook.Phase))
		if !opts.AllPhases {
			if len(opts.Phases) == 0 {
				if hookPhase != "" {
					continue
				}
			} else if !slices.Contains(opts.Phases, hookPhase) {
				continue
			}
		}
		ids = append(ids, h.Hook.ID)
	}
	return ids
}

func failedHooks(hooks []client.HookStatus) []client.HookStatus {
	out := make([]client.HookStatus, 0)
	for _, h := range hooks {
		if h.Hook.ID == "" {
			continue
		}
		if h.Status == models.StatusFailure {
			out = append(out, h)
		}
	}
	return out
}

func lspStatusRows(hooksList []client.HookStatus, diagnostics []client.Diagnostic) []lspStatusRow {
	byHook := make(map[string][]client.Diagnostic)
	for _, diagnostic := range diagnostics {
		if diagnostic.HookID == "" {
			continue
		}
		byHook[diagnostic.HookID] = append(byHook[diagnostic.HookID], diagnostic)
	}
	rows := make([]lspStatusRow, 0)
	for _, h := range hooksList {
		if h.Hook.Engine != hooksapi.HookEngineLSP {
			continue
		}
		hookDiagnostics := byHook[h.Hook.ID]
		rows = append(rows, lspStatusRow{
			HookID:          h.Hook.ID,
			Name:            h.Hook.Name,
			LanguageID:      h.Hook.LanguageID,
			Pattern:         h.Hook.Pattern,
			Status:          h.Status,
			Paused:          h.Paused,
			DiagnosticCount: len(hookDiagnostics),
			LastError:       h.LastError,
			UpdatedAt:       h.UpdatedAt,
			Hook:            h.Hook,
			Diagnostics:     hookDiagnostics,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].HookID == rows[j].HookID {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].HookID < rows[j].HookID
	})
	return rows
}

func filterEvents(events []client.Event, keep func(client.Event) bool) []client.Event {
	out := events[:0]
	for _, event := range events {
		if keep(event) {
			out = append(out, event)
		}
	}
	return out
}

func filterEventTypes(types []hookapi.EventTypeInfo, keep func(hookapi.EventTypeInfo) bool) []hookapi.EventTypeInfo {
	out := types[:0]
	for _, eventType := range types {
		if keep(eventType) {
			out = append(out, eventType)
		}
	}
	return out
}

func isLSPEvent(event client.Event) bool {
	return strings.HasPrefix(event.Type, "lsp.")
}

func isLSPEventType(eventType hookapi.EventTypeInfo) bool {
	return strings.HasPrefix(eventType.Type, "lsp.")
}

func listTargetEvents(ctx context.Context, c *client.Client, args []string, limit int) ([]client.Event, error) {
	if len(args) == 0 || isAllArg(args) {
		return c.ListEvents(ctx, client.EventOptions{Limit: limit})
	}
	ids := uniqueStrings(args)
	out := make([]client.Event, 0)
	for _, id := range ids {
		events, err := c.ListEvents(ctx, client.EventOptions{HookID: id, Limit: limit})
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func followEventHookID(args []string) (string, error) {
	if len(args) == 0 || isAllArg(args) {
		return "", nil
	}
	ids := uniqueStrings(args)
	if len(ids) > 1 {
		return "", fmt.Errorf("events --follow supports at most one hook id")
	}
	return ids[0], nil
}

func isAllArg(args []string) bool {
	return len(args) == 1 && strings.EqualFold(args[0], "all")
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func writeHookOutputs(w io.Writer, outputs []hookOutput) error {
	if len(outputs) == 1 {
		_, err := io.WriteString(w, outputs[0].Output)
		if err != nil {
			return err
		}
		if outputs[0].Output == "" || !strings.HasSuffix(outputs[0].Output, "\n") {
			_, err = fmt.Fprintln(w)
		}
		return err
	}
	for i, out := range outputs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "==> %s <==\n", out.HookID)
		_, err := io.WriteString(w, out.Output)
		if err != nil {
			return err
		}
		if out.Output == "" || !strings.HasSuffix(out.Output, "\n") {
			fmt.Fprintln(w)
		}
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeJSONLines[T any](w io.Writer, values []T) error {
	for _, value := range values {
		if err := writeJSONLine(w, value); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONLine(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func writeEventTableRow(w io.Writer, event client.Event, formatTime func(time.Time) string) {
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", event.ID, formatTime(event.CreatedAt), event.Type, event.HookID, formatRunID(event.RunID), eventMessage(event))
}

// eventMessage renders the MESSAGE column. Failure events keep their cause in
// the "error" detail, so append it here to make a table row self-explanatory
// instead of forcing a second lookup in JSON output.
func eventMessage(event client.Event) string {
	message := oneLine(event.Message)
	detail, _ := event.Details["error"].(string)
	detail = oneLine(detail)
	switch {
	case detail == "" || strings.Contains(message, detail):
		return message
	case message == "":
		return detail
	default:
		return message + ": " + detail
	}
}

func formatEventTime(value time.Time) string {
	return formatRelativeTime(time.Now(), value)
}

func formatLiveEventTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func formatRelativeTime(now, value time.Time) string {
	if value.IsZero() {
		return ""
	}
	d := now.Sub(value)
	suffix := "ago"
	if d < 0 {
		d = -d
		suffix = "from now"
	}
	unit := "second"
	amount := int64(d.Round(time.Second) / time.Second)
	switch {
	case amount < 60:
		if amount < 1 {
			amount = 1
		}
	case amount < 60*60:
		unit = "minute"
		amount = int64(d.Round(time.Minute) / time.Minute)
	case amount < 24*60*60:
		unit = "hour"
		amount = int64(d.Round(time.Hour) / time.Hour)
	case amount < 30*24*60*60:
		unit = "day"
		amount = int64(d.Round(24*time.Hour) / (24 * time.Hour))
	case amount < 365*24*60*60:
		unit = "month"
		amount = int64(d.Round(30*24*time.Hour) / (30 * 24 * time.Hour))
	default:
		unit = "year"
		amount = int64(d.Round(365*24*time.Hour) / (365 * 24 * time.Hour))
	}
	if amount < 1 {
		amount = 1
	}
	plural := ""
	if amount != 1 {
		plural = "s"
	}
	return fmt.Sprintf("%d %s%s %s", amount, unit, plural, suffix)
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatEventTime(*value)
}

func sortByCreatedAtAsc[T any](values []T, createdAt func(T) time.Time, tieBreak func(T) string) []T {
	out := append([]T(nil), values...)
	sort.SliceStable(out, func(i, j int) bool {
		left := createdAt(out[i])
		right := createdAt(out[j])
		if left.Equal(right) {
			return tieBreak(out[i]) < tieBreak(out[j])
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		return left.Before(right)
	})
	return out
}

func formatRunID(id string) string { return id }

func formatShortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

func gitDiffColorArg(w io.Writer, enabled bool) string {
	if enabled && colorEnabled(w) {
		return "--color=always"
	}
	return "--color=never"
}

func colorEnabled(w io.Writer) bool {
	if noColorEnv() {
		return false
	}
	if forceColorEnv() {
		return true
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	file, ok := w.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func noColorEnv() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return true
	}
	return os.Getenv("CLICOLOR") == "0"
}

func forceColorEnv() bool {
	if value, ok := os.LookupEnv("CLICOLOR_FORCE"); ok && value != "" && value != "0" {
		return true
	}
	if value, ok := os.LookupEnv("FORCE_COLOR"); ok && value != "" && value != "0" {
		return true
	}
	return false
}

func resolveSessionID(explicit, repoRoot string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("DISCOBOX_SESSION_ID"); v != "" {
		return v
	}
	if repoRoot != "" {
		return repoStateKey(repoRoot)
	}
	return defaultSessionID
}

func gitRoot(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return filepath.Clean(strings.TrimSpace(string(out))), nil
}

func computeSessionPaths(repoRoot, sessionID string) sessionPaths {
	root := filepath.Clean(repoRoot)
	repoKey := repoStateKey(root)
	sessionID = safePathComponent(sessionID)
	stateDir := filepath.Join(stateHome(), "discobox", "session", sessionID, "hooks", repoKey)
	runtimeDir := filepath.Join(runtimeHome(), "discobox", "session", sessionID, "hooks", repoKey)
	return sessionPaths{
		SessionID:  sessionID,
		RepoRoot:   root,
		StateDir:   stateDir,
		RuntimeDir: runtimeDir,
		Socket:     filepath.Join(runtimeDir, "daemon.sock"),
		Lock:       filepath.Join(runtimeDir, "startup.lock"),
		DB:         filepath.Join(stateDir, "hooks.db"),
		Runtime:    filepath.Join(runtimeDir, "runtime.json"),
	}
}

func ensureDaemon(ctx context.Context, c *client.Client, paths sessionPaths) error {
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	info, err := c.PingInfo(pingCtx)
	cancel()
	if err == nil && !clientNewerThanDaemon(currentBuildVersion(), info.Version) {
		return nil
	}
	if err != nil && !errors.Is(err, client.ErrNotRunning) {
		return err
	}
	if err := os.MkdirAll(paths.RuntimeDir, 0755); err != nil {
		return err
	}
	unlock, err := acquireStartupLock(paths.Lock)
	if err != nil {
		return err
	}
	defer unlock()
	pingCtx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
	info, err = c.PingInfo(pingCtx)
	cancel()
	if err == nil && !clientNewerThanDaemon(currentBuildVersion(), info.Version) {
		return nil
	}
	if err == nil && clientNewerThanDaemon(currentBuildVersion(), info.Version) {
		if err := replaceOlderDaemon(ctx, c, paths); err != nil {
			return err
		}
	} else if !errors.Is(err, client.ErrNotRunning) {
		return err
	}
	if err := terminateStaleRuntimeDaemon(ctx, paths); err != nil {
		return err
	}
	if err := startDetachedDaemon(ctx, paths); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pingCtx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
		info, err = c.PingInfo(pingCtx)
		cancel()
		if err == nil && !clientNewerThanDaemon(currentBuildVersion(), info.Version) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready at %s: %w", paths.Socket, err)
}

func clientNewerThanDaemon(clientVersion, daemonVersion int64) bool {
	return clientVersion > daemonVersion
}

func replaceOlderDaemon(ctx context.Context, c *client.Client, paths sessionPaths) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := c.Shutdown(shutdownCtx)
	cancel()
	if err != nil && !errors.Is(err, client.ErrNotRunning) {
		return fmt.Errorf("shutdown older daemon: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		err = c.Ping(pingCtx)
		cancel()
		if errors.Is(err, client.ErrNotRunning) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := terminateStaleRuntimeDaemon(ctx, paths); err != nil {
		return err
	}
	return fmt.Errorf("older daemon did not stop at %s", c.SocketPath())
}

type hookDaemonRuntimeFile struct {
	SessionID string `json:"session_id"`
	RepoRoot  string `json:"repo_root"`
	PID       int    `json:"pid"`
}

func terminateStaleRuntimeDaemon(ctx context.Context, paths sessionPaths) error {
	data, readErr := os.ReadFile(paths.Runtime)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return nil
		}
		return readErr
	}
	var runtime hookDaemonRuntimeFile
	if json.Unmarshal(data, &runtime) != nil {
		_ = os.Remove(paths.Runtime)
	}
	if runtime.SessionID != "" && runtime.SessionID != paths.SessionID {
		return nil
	}
	if runtime.RepoRoot != "" && filepath.Clean(runtime.RepoRoot) != filepath.Clean(paths.RepoRoot) {
		return nil
	}
	if runtime.PID <= 0 || runtime.PID == os.Getpid() {
		return nil
	}
	if !processExists(runtime.PID) {
		_ = os.Remove(paths.Runtime)
		return nil
	}
	if err := terminateDaemonProcessGroup(runtime.PID); err != nil && !isProcessNotFound(err) {
		return fmt.Errorf("terminate stale daemon pid %d: %w", runtime.PID, err)
	}
	if waitForProcessExit(ctx, runtime.PID, 2*time.Second) {
		_ = os.Remove(paths.Runtime)
		return nil
	}
	if err := killDaemonProcessGroup(runtime.PID); err != nil && !isProcessNotFound(err) {
		return fmt.Errorf("kill stale daemon pid %d: %w", runtime.PID, err)
	}
	if waitForProcessExit(ctx, runtime.PID, 2*time.Second) {
		_ = os.Remove(paths.Runtime)
		return nil
	}
	return fmt.Errorf("stale daemon pid %d did not exit", runtime.PID)
}

func processIsZombie(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat"))
	if err != nil {
		return false
	}
	_, rest, ok := strings.Cut(string(data), ") ")
	if !ok || rest == "" {
		return false
	}
	return rest[0] == 'Z'
}

func waitForProcessExit(ctx context.Context, pid int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !processExists(pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return !processExists(pid)
		case <-ticker.C:
		}
	}
}

func currentBuildVersion() int64 {
	if parsed, ok := parseVersion(version); ok {
		return parsed
	}
	exe, err := os.Executable()
	if err == nil {
		if info, err := os.Stat(exe); err == nil {
			return info.ModTime().Unix()
		}
	}
	return time.Now().Unix()
}

func parseVersion(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var parsed int64
	if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func repoStateKey(repoRoot string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(repoRoot)))
	return hex.EncodeToString(sum[:])[:16]
}

// stateHome is where a session's hook state lives: XDG_STATE_HOME where it is
// set, and otherwise the platform's own place for state a program derives
// rather than a user edits (see statehome_unix.go, statehome_windows.go).
//
// The variable is honored on every platform, Windows included. Nothing there
// sets it by accident, and the runner is run from shells that may.
func stateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Clean(v)
	}
	if home := defaultStateHome(); home != "" {
		return home
	}
	return filepath.Join(os.TempDir(), "discobox-state")
}

// runtimeHome is where the socket and the lock live: what XDG_RUNTIME_DIR names
// where there is one, and a directory under the state home where there is not —
// which is every Windows machine, since it has no such variable.
func runtimeHome() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Clean(v)
	}
	return filepath.Join(stateHome(), "run")
}

func safePathComponent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultSessionID
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return defaultSessionID
	}
	return b.String()
}
