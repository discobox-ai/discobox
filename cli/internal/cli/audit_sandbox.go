package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// The hook and exec trails are kept inside the discobox (ADR 0130 §2). Anything
// in it can rewrite them, they are gone when it is, and they are read only
// while it runs: reading them does not start it.
const sandboxTrailNote = `These records are kept inside the discobox, where anything running in it can
rewrite them; they are its own account, not evidence. They are gone when the
discobox is deleted, and can be read only while it runs: reading them does not
start it.`

func (a *App) newAuditHooksCommand() *cobra.Command {
	var sandboxID, terminalID, provider, event, since string
	var limit int
	var follow bool
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "List the harness hook payloads a discobox recorded",
		Long: `List the lifecycle hook payloads a discobox's coding harness published —
session start, each tool call, notifications, stop — newest first. With
--follow, print the last --limit oldest first and keep printing hooks as they
are recorded.

` + sandboxTrailNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, client, err := a.auditSandboxScope(cmd.Context(), sandboxID)
			if err != nil {
				return err
			}
			params := apiclientgen.ListHarnessHooksParams{ProjectId: projectID, SandboxId: resolvedSandboxID}
			if strings.TrimSpace(terminalID) != "" {
				resolved, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, terminalID)
				if err != nil {
					return err
				}
				params.TerminalId = apiclientgen.NewOptString(resolved)
			}
			if provider != "" {
				params.Provider = apiclientgen.NewOptString(provider)
			}
			if event != "" {
				params.Event = apiclientgen.NewOptString(event)
			}
			sinceAt, err := parseOptionalSince(since)
			if err != nil {
				return err
			}
			source := harnessHookSource(client, params)
			if a.output == "json" && !follow {
				hooks, err := source.read(cmd.Context(), auditReadCursor{Since: sinceAt}, limit)
				if err != nil {
					return err
				}
				return writeTerminalSafeJSON(cmd.OutOrStdout(), &apimodel.HarnessHookLogsResponse{Hooks: hooks})
			}
			table := harnessHookTable(follow)
			header := true
			return readAudit(cmd.Context(), []auditSource[apimodel.HarnessHookLog]{source}, a.auditReadOptions(cmd, sinceAt, limit, follow), func(hooks []apimodel.HarnessHookLog) error {
				if a.output == "json" {
					return writeTerminalSafeJSONLines(cmd, hooks)
				}
				err := table.write(cmd.OutOrStdout(), hooks, header, follow)
				header = false
				return err
			})
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Discobox whose hooks to read (required)")
	cmd.Flags().StringVar(&terminalID, "terminal-id", "", "Only hooks from this terminal (ID or prefix)")
	cmd.Flags().StringVar(&provider, "provider", "", "Only hooks from this harness provider, such as claude-code")
	cmd.Flags().StringVar(&event, "event", "", "Only this hook event, such as PreToolUse")
	cmd.Flags().StringVar(&since, "since", "", "Only hooks from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of hooks to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing hooks as they are recorded")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("terminal-id", a.completeTerminals(&sandboxID))
	return cmd
}

func (a *App) newAuditExecsCommand() *cobra.Command {
	var sandboxID, execID, eventType, since string
	var limit int
	var follow bool
	cmd := &cobra.Command{
		Use:   "execs",
		Short: "List the exec lifecycle events a discobox recorded",
		Long: `List the lifecycle events of every exec in a discobox — created, started,
attached, stopped, failed to start — newest first. With --follow, print the
last --limit oldest first and keep printing events as they are recorded.

` + sandboxTrailNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, client, err := a.auditSandboxScope(cmd.Context(), sandboxID)
			if err != nil {
				return err
			}
			params := apiclientgen.ListExecEventsParams{ProjectId: projectID, SandboxId: resolvedSandboxID}
			if strings.TrimSpace(execID) != "" {
				resolved, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, execID)
				if err != nil {
					return err
				}
				params.ExecId = apiclientgen.NewOptString(resolved)
			}
			if eventType != "" {
				params.Type = apiclientgen.NewOptString(eventType)
			}
			sinceAt, err := parseOptionalSince(since)
			if err != nil {
				return err
			}
			source := execEventSource(client, params)
			if a.output == "json" && !follow {
				events, err := source.read(cmd.Context(), auditReadCursor{Since: sinceAt}, limit)
				if err != nil {
					return err
				}
				return writeTerminalSafeJSON(cmd.OutOrStdout(), &apimodel.SandboxExecEventsResponse{Events: events})
			}
			table := execEventTable(follow)
			header := true
			return readAudit(cmd.Context(), []auditSource[apimodel.SandboxExecEvent]{source}, a.auditReadOptions(cmd, sinceAt, limit, follow), func(events []apimodel.SandboxExecEvent) error {
				if a.output == "json" {
					return writeTerminalSafeJSONLines(cmd, events)
				}
				err := table.write(cmd.OutOrStdout(), events, header, follow)
				header = false
				return err
			})
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Discobox whose exec events to read (required)")
	cmd.Flags().StringVar(&execID, "exec-id", "", "Only this exec's events (ID or prefix)")
	cmd.Flags().StringVar(&eventType, "type", "", "Only this event type, such as exec.start.failed")
	cmd.Flags().StringVar(&since, "since", "", "Only events from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().IntVar(&limit, "limit", defaultAuditLimit, "Maximum number of events to return")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing events as they are recorded")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("exec-id", a.completeExecs(&sandboxID))
	return cmd
}

// auditSandboxScope resolves the discobox a sandbox-kept trail is read from.
// Unlike the pool and verdict trails these have no project-wide read: they
// are inside each discobox.
func (a *App) auditSandboxScope(ctx context.Context, sandboxID string) (string, string, *apiclientgen.Client, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return "", "", nil, errors.New("--discobox-id is required: these records are kept inside each discobox")
	}
	return a.sandboxRequest(ctx, sandboxID)
}

// harnessHookSource reads a discobox's hook trail.
func harnessHookSource(client *apiclientgen.Client, params apiclientgen.ListHarnessHooksParams) auditSource[apimodel.HarnessHookLog] {
	return auditSource[apimodel.HarnessHookLog]{
		read: func(ctx context.Context, cursor auditReadCursor, limit int) ([]apimodel.HarnessHookLog, error) {
			params := params
			params.Limit = apiclientgen.NewOptInt(limit)
			if !cursor.Since.IsZero() {
				params.Since = apiclientgen.NewOptDateTime(cursor.Since)
			}
			if cursor.Forward {
				params.Order = apiclientgen.NewOptListHarnessHooksOrder(apiclientgen.ListHarnessHooksOrderAsc)
			}
			res, err := client.ListHarnessHooks(ctx, params)
			if err != nil {
				return nil, err
			}
			body, err := expectResponse[apimodel.HarnessHookLogsResponse](res)
			if err != nil {
				return nil, err
			}
			hooks := body.GetHooks()
			if !cursor.Forward {
				// The agent returns the latest hooks oldest first, as a log
				// reads; every audit read back is newest first.
				slices.Reverse(hooks)
			}
			return hooks, nil
		},
		key: func(h apimodel.HarnessHookLog) string { return h.ID },
		at:  func(h apimodel.HarnessHookLog) time.Time { return h.CreatedAt },
		// Hook IDs are random, so the cursor is the time. The sandbox agent
		// writes each record as it receives it, so only two commits
		// interleaving can reorder them.
		lookback: auditWriterLookback,
	}
}

// execEventSource reads the events of every exec in a discobox.
func execEventSource(client *apiclientgen.Client, params apiclientgen.ListExecEventsParams) auditSource[apimodel.SandboxExecEvent] {
	return auditSource[apimodel.SandboxExecEvent]{
		read: func(ctx context.Context, cursor auditReadCursor, limit int) ([]apimodel.SandboxExecEvent, error) {
			params := params
			params.Limit = apiclientgen.NewOptInt(limit)
			if !cursor.Since.IsZero() {
				params.Since = apiclientgen.NewOptDateTime(cursor.Since)
			}
			if cursor.Forward {
				params.Order = apiclientgen.NewOptListExecEventsOrder(apiclientgen.ListExecEventsOrderAsc)
			}
			res, err := client.ListExecEvents(ctx, params)
			if err != nil {
				return nil, err
			}
			body, err := expectResponse[apimodel.SandboxExecEventsResponse](res)
			if err != nil {
				return nil, err
			}
			return body.GetEvents(), nil
		},
		key: func(e apimodel.SandboxExecEvent) string { return e.ID },
		at:  func(e apimodel.SandboxExecEvent) time.Time { return e.CreatedAt },
		// As with hooks: random IDs, one writer committing as it records.
		lookback: auditWriterLookback,
	}
}

func parseOptionalSince(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return parseSince(value, time.Now())
}

// Every field but the time is whatever the discobox recorded, in a database it
// can rewrite, and a payload that is not valid JSON is printed as its raw bytes.
func harnessHookTable(follow bool) auditTable[apimodel.HarnessHookLog] {
	return auditTable[apimodel.HarnessHookLog]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "TERMINAL", width: 22}, {name: "PROVIDER", width: 14},
			{name: "EVENT", width: 20}, {name: "PAYLOAD"},
		},
		row: func(log apimodel.HarnessHookLog) []string {
			return []string{
				auditTime(log.CreatedAt, follow),
				terminalSafe(log.TerminalId.Or("")),
				terminalSafe(log.Provider),
				terminalSafe(log.Event),
				truncateTableValue(terminalSafe(compactJSON(log.Payload)), 120),
			}
		},
	}
}

func execEventTable(follow bool) auditTable[apimodel.SandboxExecEvent] {
	return auditTable[apimodel.SandboxExecEvent]{
		columns: []auditColumn{
			{name: "TIME", width: 12}, {name: "EXEC", width: 22}, {name: "TYPE", width: 22}, {name: "MESSAGE"},
		},
		row: func(e apimodel.SandboxExecEvent) []string {
			return []string{
				auditTime(e.CreatedAt, follow),
				terminalSafe(e.ExecId.Or("")),
				terminalSafe(e.Type),
				truncateTableValue(terminalSafe(e.Message.Or("")), 120),
			}
		},
	}
}

func compactJSON(raw []byte) string {
	if len(raw) == 0 {
		return "{}"
	}
	if !json.Valid(raw) {
		return string(raw)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
