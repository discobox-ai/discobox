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
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	hooksapi "github.com/obot-platform/discobox/hooks"
	hookapi "github.com/obot-platform/discobox/hooks/api"
	"github.com/obot-platform/discobox/hooks/client"
	"github.com/obot-platform/discobox/hooks/daemon"
	"github.com/obot-platform/discobox/hooks/models"
	"github.com/spf13/cobra"
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
	cmd.AddCommand(a.newDBCommand())
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
	var idle, debounce time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the hook daemon in the foreground",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.sessionPaths(cmd.Context())
			if err != nil {
				return err
			}
			cfg := daemon.Config{
				SessionID:   paths.SessionID,
				RepoRoot:    paths.RepoRoot,
				DBPath:      paths.DB,
				SocketPath:  paths.Socket,
				TempDir:     filepath.Join(paths.RuntimeDir, "tmp"),
				Version:     currentBuildVersion(),
				Debounce:    debounce,
				IdleTimeout: idle,
			}
			return daemon.Run(cmd.Context(), cfg)
		},
	}
	cmd.Flags().DurationVar(&idle, "idle-timeout", 0, "daemon idle timeout")
	cmd.Flags().DurationVar(&debounce, "debounce", 0, "file-change debounce duration")
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
	var phase string
	cmd := &cobra.Command{
		Use:   "run [HOOK_ID ...]",
		Short: "Request hook runs",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			phase = strings.TrimSpace(strings.ToLower(phase))
			allPhases := phase == "" && force && isAllArg(args)
			if phase == "" && !allPhases && (len(args) == 0 || isAllArg(args)) {
				phase = "review"
			}
			ids, all, err := targetRunHookIDs(cmd.Context(), c, args, runTargetOptions{SessionHooks: sessionHooks, Phase: phase, Force: force, AllPhases: allPhases})
			if err != nil {
				return err
			}
			phaseByID := map[string]string{}
			if allPhases {
				hooksList, err := c.ListHooks(cmd.Context())
				if err != nil {
					return err
				}
				for _, h := range hooksList {
					phaseByID[h.Hook.ID] = strings.TrimSpace(strings.ToLower(h.Hook.Phase))
				}
			}
			responses := make([]*client.RunResponse, 0, len(ids))
			for _, id := range ids {
				runPhase := phase
				if allPhases {
					runPhase = phaseByID[id]
				}
				resp, err := c.RunHook(cmd.Context(), id, client.RunOptions{Force: force, Phase: runPhase})
				if err != nil {
					return err
				}
				if resp.HookID == "" {
					resp.HookID = id
				}
				responses = append(responses, resp)
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"all": all, "all_phases": allPhases, "session": sessionHooks, "phase": phase, "force": force, "runs": responses})
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
	cmd.Flags().StringVar(&phase, "phase", "", "allow queued hooks in phase after unphased queued hooks")
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

func (a *app) newDBCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "db",
		Aliases: []string{"inspect"},
		Short:   "Inspect extended hook daemon resources",
	}
	cmd.AddCommand(a.newDBRunsCommand())
	cmd.AddCommand(a.newDBChangesCommand())
	cmd.AddCommand(a.newDBSnapshotsCommand())
	cmd.AddCommand(a.newDBQueueCommand())
	return cmd
}

func (a *app) newDBRunsCommand() *cobra.Command {
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

func (a *app) newDBChangesCommand() *cobra.Command {
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

func (a *app) newDBSnapshotsCommand() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "snapshots",
		Short: "List workspace snapshots",
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
	return cmd
}

func (a *app) newDBQueueCommand() *cobra.Command {
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
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tTYPE\tHOOK\tRUN\tMESSAGE")
	for _, event := range events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", event.ID, formatEventTime(event.CreatedAt), event.Type, event.HookID, formatRunID(event.RunID), event.Message)
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
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tPATH\tKIND\tBASE\tDIFF_BYTES")
	for _, change := range changes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", change.ID, formatEventTime(change.CreatedAt), change.Path, change.Kind, change.BaseCommit, len(change.Diff))
	}
	return tw.Flush()
}

func (a *app) writeSnapshots(cmd *cobra.Command, snapshots []client.WorkspaceSnapshot) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"snapshots": snapshots})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tPARENT\tBASE\tTREE\tPATCH_BYTES\tCHANGED\tOMITTED\tOBSERVED")
	for _, snapshot := range snapshots {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\n", snapshot.ID, formatEventTime(snapshot.CreatedAt), snapshot.ParentID, snapshot.BaseCommit, snapshot.TreeHash, snapshot.PatchBytes, len(snapshot.ChangedFiles), len(snapshot.OmittedFiles), len(snapshot.ObservedChangeIDs))
	}
	return tw.Flush()
}

func (a *app) writeQueue(cmd *cobra.Command, rows []client.QueuedHook) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"queue": rows})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POSITION\tHOOK\tBLOCKED\tBLOCKED_BY\tFILES\tCHANGES\tCREATED\tUPDATED")
	for _, row := range rows {
		fmt.Fprintf(tw, "%d\t%s\t%t\t%s\t%d\t%d\t%s\t%s\n", row.Position, row.HookID, row.Blocked, row.BlockedByHookID, len(row.ChangedFiles), len(row.ChangeIDs), formatEventTime(row.CreatedAt), formatEventTime(row.UpdatedAt))
	}
	return tw.Flush()
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
	return c.FollowEvents(cmd.Context(), client.EventOptions{HookID: hookID, Limit: limit}, func(event client.Event) error {
		if a.opts.output == "json" {
			return writeJSONLine(w, event)
		}
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", event.ID, formatEventTime(event.CreatedAt), event.Type, event.HookID, formatRunID(event.RunID), event.Message)
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
	Phase        string
	Force        bool
	AllPhases    bool
}

func targetRunHookIDs(ctx context.Context, c *client.Client, args []string, opts runTargetOptions) ([]string, bool, error) {
	if len(args) != 0 && !isAllArg(args) {
		return uniqueStrings(args), false, nil
	}
	hooks, err := c.ListHooks(ctx)
	if err != nil {
		return nil, false, err
	}
	return filterRunTargets(hooks, opts), true, nil
}

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
		if opts.AllPhases {
			// No phase filter.
		} else if opts.Phase == "" {
			if hookPhase != "" {
				continue
			}
		} else if hookPhase != "" && hookPhase != opts.Phase {
			continue
		}
		if !opts.Force && !runAllStatusEligible(h) {
			continue
		}
		ids = append(ids, h.Hook.ID)
	}
	return ids
}

func runAllStatusEligible(h client.HookStatus) bool {
	switch h.Status {
	case models.StatusQueued, models.StatusFailure:
		return true
	default:
		return false
	}
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

func formatEventTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Local().Format(time.RFC3339)
}

func formatOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatEventTime(*value)
}

func formatRunID(id string) string { return id }

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
	stateDir := filepath.Join(xdgStateHome(), "discobox", "session", sessionID, "hooks", repoKey)
	runtimeDir := filepath.Join(xdgRuntimeHome(), "discobox", "session", sessionID, "hooks", repoKey)
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
		if err := replaceOlderDaemon(ctx, c); err != nil {
			return err
		}
	} else if !errors.Is(err, client.ErrNotRunning) {
		return err
	}
	if err := startDetachedDaemon(paths); err != nil {
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

func replaceOlderDaemon(ctx context.Context, c *client.Client) error {
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
	return fmt.Errorf("older daemon did not stop at %s", c.SocketPath())
}

func acquireStartupLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func startDetachedDaemon(paths sessionPaths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "--session-id", paths.SessionID, "--repo-root", paths.RepoRoot, "daemon", "--foreground")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "DISCOBOX_SESSION_ID="+paths.SessionID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
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

func xdgStateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Clean(v)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state")
	}
	return filepath.Join(os.TempDir(), "discobox-state")
}

func xdgRuntimeHome() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Clean(v)
	}
	return filepath.Join(xdgStateHome(), "run")
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
