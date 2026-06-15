package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func (a *App) writeSandbox(cmd *cobra.Command, sandbox *apiclientgen.Sandbox) error {
	if sandbox == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), sandbox)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPHASE\tDESIRED\tGENERATION\tUPDATED")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
		sandbox.ID,
		sandbox.Name,
		sandbox.Phase,
		sandbox.DesiredState,
		sandbox.Generation,
		formatTime(sandbox.UpdatedAt),
	)
	return tw.Flush()
}

func (a *App) writeSandboxes(cmd *cobra.Command, sandboxes []apiclientgen.Sandbox) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"sandboxes": sandboxes})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPHASE\tDESIRED\tGENERATION\tUPDATED")
	for _, sandbox := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			sandbox.ID,
			sandbox.Name,
			sandbox.Phase,
			sandbox.DesiredState,
			sandbox.Generation,
			formatTime(sandbox.UpdatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writeProviderCatalog(cmd *cobra.Command, providers []apiclientgen.SandboxProviderCatalogItem) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providers": providers})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tAVAILABLE\tBUILT-IN\tDESCRIPTION")
	for _, provider := range providers {
		fmt.Fprintf(tw, "%s\t%s\t%t\t%t\t%s\n", provider.ID, provider.Name, provider.Available, provider.BuiltIn, provider.Description.Or(""))
	}
	return tw.Flush()
}

func (a *App) writeProvider(cmd *cobra.Command, provider *apiclientgen.SandboxProviderInstance) error {
	if provider == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), provider)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", provider.ID)
	fmt.Fprintf(tw, "NAME\t%s\n", provider.Name)
	fmt.Fprintf(tw, "TYPE\t%s\n", provider.Type)
	fmt.Fprintf(tw, "DISABLED\t%t\n", provider.Disabled)
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(provider.UpdatedAt))
	fmt.Fprintf(tw, "CONFIG\t%s\n", formatRedactedRawJSON(provider.GetConfig()))
	if status, ok := provider.GetStatus().Get(); ok {
		fmt.Fprintf(tw, "STATUS\t%s\n", formatProviderStatus(status))
	}
	return tw.Flush()
}

func (a *App) writeProviders(cmd *cobra.Command, providers []apiclientgen.SandboxProviderInstance) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providers": providers})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tDISABLED\tUPDATED")
	for _, provider := range providers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", provider.ID, provider.Name, provider.Type, provider.Disabled, formatTime(provider.UpdatedAt))
	}
	return tw.Flush()
}

func (a *App) writeAgentDefinition(cmd *cobra.Command, definition *apiclientgen.AgentConfigDefinition) error {
	if definition == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), definition)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tRUN COMMAND\tDESCRIPTION")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", definition.ID, definition.Name, definition.RunCommand, definition.Description.Or(""))
	return tw.Flush()
}

func (a *App) writeAgentDefinitions(cmd *cobra.Command, definitions []apiclientgen.AgentConfigDefinition) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"agentConfigDefinitions": definitions})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tRUN COMMAND\tDESCRIPTION")
	for _, definition := range definitions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", definition.ID, definition.Name, definition.RunCommand, definition.Description.Or(""))
	}
	return tw.Flush()
}

func (a *App) writeAgent(cmd *cobra.Command, agent *apiclientgen.AgentConfig) error {
	if agent == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), agent)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tRUN COMMAND\tUPDATED")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", agent.ID, agent.Name, agent.RunCommand, formatTime(agent.UpdatedAt))
	return tw.Flush()
}

func (a *App) writeAgents(cmd *cobra.Command, agents []apiclientgen.AgentConfig) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"agentConfigs": agents})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tRUN COMMAND\tUPDATED")
	for _, agent := range agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", agent.ID, agent.Name, agent.RunCommand, formatTime(agent.UpdatedAt))
	}
	return tw.Flush()
}

func parseUUIDArg(value, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	return id, nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Local().Format(time.RFC3339)
}

func formatRedactedRawJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "[invalid JSON config redacted]"
	}
	redactSensitiveJSON(value)
	data, err := json.Marshal(value)
	if err != nil {
		return "[invalid JSON config redacted]"
	}
	return string(data)
}

func formatJSONValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func formatProviderStatus(status apiclientgen.SandboxProviderInstanceStatus) string {
	value := map[string]any{
		"workerCount":        status.WorkerCount,
		"readyWorkers":       status.ReadyWorkers,
		"schedulableWorkers": status.SchedulableWorkers,
		"degradedWorkers":    status.DegradedWorkers,
		"failedWorkers":      status.FailedWorkers,
	}
	if lastError, ok := status.LastError.Get(); ok {
		value["lastError"] = lastError
	}
	if workers, ok := status.Workers.Get(); ok {
		out := make([]map[string]any, 0, len(workers))
		for _, worker := range workers {
			item := map[string]any{
				"id":                  worker.ID,
				"desiredState":        worker.DesiredState,
				"phase":               worker.Phase,
				"ready":               worker.Ready,
				"schedulable":         worker.Schedulable,
				"degraded":            worker.Degraded,
				"lastOperationStatus": worker.LastOperationStatus,
			}
			if identity, ok := worker.Identity.Get(); ok {
				item["identity"] = identity
			}
			if runtimeID, ok := worker.RuntimeId.Get(); ok {
				item["runtimeId"] = runtimeID
			}
			out = append(out, item)
		}
		value["workers"] = out
	}
	return formatJSONValue(value)
}

func redactSensitiveJSON(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveConfigKey(key) {
				typed[key] = "[REDACTED]"
				continue
			}
			redactSensitiveJSON(child)
		}
	case []any:
		for _, child := range typed {
			redactSensitiveJSON(child)
		}
	}
}

func isSensitiveConfigKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""), " ", ""))
	for _, needle := range []string{
		"token",
		"password",
		"secret",
		"apikey",
		"accesskey",
		"privatekey",
		"credential",
	} {
		if strings.Contains(normalized, needle) {
			return true
		}
	}
	return false
}

func optString(value string) apiclientgen.OptString {
	if strings.TrimSpace(value) == "" {
		return apiclientgen.OptString{}
	}
	return apiclientgen.NewOptString(value)
}
