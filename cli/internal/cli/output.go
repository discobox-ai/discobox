package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiclientgen "github.com/obot-platform/discobox/api/clientgen"
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
		shortID(sandbox.ID),
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
			shortID(sandbox.ID),
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
	fmt.Fprintf(tw, "ID\t%s\n", shortID(provider.ID))
	fmt.Fprintf(tw, "NAME\t%s\n", provider.Name)
	fmt.Fprintf(tw, "TYPE\t%s\n", provider.Type)
	fmt.Fprintf(tw, "DISABLED\t%t\n", provider.Disabled)
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(provider.UpdatedAt))
	fmt.Fprintf(tw, "CONFIG\t%s\n", formatRedactedRawJSON(provider.GetConfig()))
	if workers, ok := provider.GetWorkers().Get(); ok {
		fmt.Fprintf(tw, "STATUS\t%s\n", formatProviderStatus(workers))
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", shortID(provider.ID), provider.Name, provider.Type, provider.Disabled, formatTime(provider.UpdatedAt))
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
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortID(definition.ID), definition.Name, definition.RunCommand, definition.Description.Or(""))
	return tw.Flush()
}

func (a *App) writeAgentDefinitions(cmd *cobra.Command, definitions []apiclientgen.AgentConfigDefinition) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"agentConfigDefinitions": definitions})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tRUN COMMAND\tDESCRIPTION")
	for _, definition := range definitions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortID(definition.ID), definition.Name, definition.RunCommand, definition.Description.Or(""))
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
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortID(agent.ID), agent.Name, agent.RunCommand, formatTime(agent.UpdatedAt))
	return tw.Flush()
}

func (a *App) writeAgents(cmd *cobra.Command, agents []apiclientgen.AgentConfig) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"agentConfigs": agents})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tRUN COMMAND\tUPDATED")
	for _, agent := range agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortID(agent.ID), agent.Name, agent.RunCommand, formatTime(agent.UpdatedAt))
	}
	return tw.Flush()
}

func (a *App) writeJobs(cmd *cobra.Command, jobs []apiclientgen.Job) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"jobs": jobs})
	}
	rows := make([][]string, 0, len(jobs))
	errors := make([]string, 0, len(jobs))
	for _, job := range jobs {
		rows = append(rows, []string{
			shortID(job.ID),
			job.Type,
			string(job.Status),
			fmt.Sprintf("%d/%d", job.Attempts, job.MaxAttempts),
			job.ResourceType + "/" + shortID(job.ResourceId),
			formatTime(job.CreatedAt),
		})
		errors = append(errors, compactTableValue(job.Error.Or("")))
	}
	errorWidth := jobsTableErrorWidth(terminalWidth(cmd.OutOrStdout()), rows)
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tATTEMPTS\tRESOURCE\tCREATED\tERROR")
	for i, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row[0],
			row[1],
			row[2],
			row[3],
			row[4],
			row[5],
			truncateTableValue(errors[i], errorWidth),
		)
	}
	return tw.Flush()
}

func (a *App) writeJob(cmd *cobra.Command, job *apiclientgen.Job) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), job)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", shortID(job.ID))
	fmt.Fprintf(tw, "TYPE\t%s\n", job.Type)
	fmt.Fprintf(tw, "STATUS\t%s\n", job.Status)
	fmt.Fprintf(tw, "ATTEMPTS\t%d/%d\n", job.Attempts, job.MaxAttempts)
	if job.WorkerId.Set && job.WorkerId.Value != "" {
		fmt.Fprintf(tw, "WORKER\t%s\n", shortID(job.WorkerId.Value))
	}
	fmt.Fprintf(tw, "RESOURCE\t%s\n", shortResourceID(job.ResourceType, job.ResourceId))
	fmt.Fprintf(tw, "SCHEDULED\t%s\n", formatTime(job.ScheduledAt))
	if job.StartedAt.Set && !job.StartedAt.Value.IsZero() {
		fmt.Fprintf(tw, "STARTED\t%s\n", formatTime(job.StartedAt.Value))
	}
	if job.CompletedAt.Set && !job.CompletedAt.Value.IsZero() {
		fmt.Fprintf(tw, "COMPLETED\t%s\n", formatTime(job.CompletedAt.Value))
	}
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(job.CreatedAt))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(job.UpdatedAt))
	if job.Error.Set && job.Error.Value != "" {
		fmt.Fprintf(tw, "ERROR\t%s\n", job.Error.Value)
	}
	return tw.Flush()
}

func parseIDArg(value, name string) (string, error) {
	id := strings.TrimSpace(value)
	if id == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return id, nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return formatRelativeTime(time.Now(), value)
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

func compactTableValue(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	return strings.Join(strings.Fields(value), " ")
}

func truncateTableValue(value string, maxTableValueLength int) string {
	value = compactTableValue(value)
	runes := []rune(value)
	if len(runes) <= maxTableValueLength {
		return value
	}
	if maxTableValueLength <= 1 {
		return string(runes[:maxTableValueLength])
	}
	return string(runes[:maxTableValueLength-1]) + "…"
}

func jobsTableErrorWidth(terminalColumns int, rows [][]string) int {
	const (
		defaultErrorWidth = 80
		minErrorWidth     = 20
		separatorWidth    = 2
	)
	if terminalColumns <= 0 {
		return defaultErrorWidth
	}
	widths := []int{
		len("ID"),
		len("TYPE"),
		len("STATUS"),
		len("ATTEMPTS"),
		len("RESOURCE"),
		len("CREATED"),
	}
	for _, row := range rows {
		for i, value := range row {
			if width := runeLen(value); width > widths[i] {
				widths[i] = width
			}
		}
	}
	used := 0
	for _, width := range widths {
		used += width
	}
	// Seven table columns produce six gaps in tabwriter's padded output.
	used += separatorWidth * 6
	available := terminalColumns - used
	if available < minErrorWidth {
		return minErrorWidth
	}
	return available
}

func terminalWidth(w io.Writer) int {
	file, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}

func runeLen(value string) int {
	return len([]rune(value))
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

func formatProviderStatus(workers []apiclientgen.Worker) string {
	readyWorkers := 0
	schedulableWorkers := 0
	degradedWorkers := 0
	failedWorkers := 0
	out := make([]map[string]any, 0, len(workers))
	for _, worker := range workers {
		if worker.Ready {
			readyWorkers++
		}
		if worker.Schedulable {
			schedulableWorkers++
		}
		if worker.Degraded {
			degradedWorkers++
		}
		if string(worker.Phase) == "failed" || string(worker.LastOperationStatus) == "failed" {
			failedWorkers++
		}
		item := map[string]any{
			"id":                  shortID(worker.ID),
			"desiredState":        worker.DesiredState,
			"phase":               worker.Phase,
			"ready":               worker.Ready,
			"schedulable":         worker.Schedulable,
			"degraded":            worker.Degraded,
			"lastOperationStatus": worker.LastOperationStatus,
		}
		if worker.Identity != "" {
			item["identity"] = worker.Identity
		}
		if errMessage, ok := worker.ErrorMessage.Get(); ok {
			item["errorMessage"] = errMessage
		}
		out = append(out, item)
	}
	value := map[string]any{
		"workerCount":        len(workers),
		"readyWorkers":       readyWorkers,
		"schedulableWorkers": schedulableWorkers,
		"degradedWorkers":    degradedWorkers,
		"failedWorkers":      failedWorkers,
		"workers":            out,
	}
	for _, worker := range workers {
		if errMessage, ok := worker.ErrorMessage.Get(); ok {
			value["lastError"] = errMessage
			break
		}
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
