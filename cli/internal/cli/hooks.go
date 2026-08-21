package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func (a *App) newHooksCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Inspect discobox hook logs",
	}
	cmd.AddCommand(a.newHooksLogsCommand())
	return cmd
}

func (a *App) newHooksLogsCommand() *cobra.Command {
	var sandboxID, terminalID string
	var limit int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print discobox hook payload logs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, err := a.resolveHookLogScope(cmd.Context(), sandboxID)
			if err != nil {
				return err
			}
			if strings.TrimSpace(terminalID) != "" {
				terminalID, err = a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, terminalID)
				if err != nil {
					return err
				}
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			params := apiclientgen.ListHarnessHooksParams{
				ProjectId: projectID,
				SandboxId: resolvedSandboxID,
			}
			if terminalID != "" {
				params.TerminalId = apiclientgen.NewOptString(terminalID)
			}
			if limit > 0 {
				params.Limit = apiclientgen.NewOptInt(limit)
			}
			res, err := client.ListHarnessHooks(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.HarnessHookLogsResponse](res)
			if err != nil {
				return err
			}
			logs := body.GetHooks()
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), body)
			}
			return writeHarnessHookLogs(cmd.OutOrStdout(), logs)
		},
	}
	cmd.Flags().StringVar(&sandboxID, "discobox-id", "", "Discobox ID")
	cmd.Flags().StringVar(&terminalID, "terminal-id", "", "Terminal ID or prefix")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of hook records to return")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	_ = cmd.RegisterFlagCompletionFunc("terminal-id", a.completeTerminals(&sandboxID))
	return cmd
}

func (a *App) resolveHookLogScope(ctx context.Context, sandboxID string) (string, string, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return "", "", fmt.Errorf("--discobox-id is required")
	}
	projectID, resolvedSandboxID, _, err := a.sandboxRequest(ctx, sandboxID)
	return projectID, resolvedSandboxID, err
}

func writeHarnessHookLogs(out io.Writer, logs []apimodel.HarnessHookLog) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tTERMINAL\tPROVIDER\tEVENT\tPAYLOAD")
	for _, log := range logs {
		payload := compactJSON(log.Payload)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			formatTime(log.CreatedAt),
			log.TerminalId.Or(""),
			log.Provider,
			log.Event,
			truncateTableValue(payload, 120),
		)
	}
	return tw.Flush()
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
