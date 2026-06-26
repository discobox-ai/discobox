package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeResourceIDs[T any](w io.Writer, values []T, id func(T) string) error {
	for _, value := range values {
		if _, err := fmt.Fprintln(w, id(value)); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) writeSandbox(cmd *cobra.Command, sandbox *apimodel.Sandbox) error {
	if sandbox == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), sandbox)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPHASE\tDESIRED\tGENERATION\tERROR\tUPDATED")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
		shortID(sandbox.ID),
		sandbox.Config.Name,
		sandbox.Runtime.Phase,
		sandbox.Runtime.DesiredState,
		sandbox.Runtime.Generation,
		truncateTableValue(sandboxMessage(*sandbox), 80),
		formatTime(sandbox.UpdatedAt),
	)
	return tw.Flush()
}

func (a *App) writeSandboxes(cmd *cobra.Command, sandboxes []apimodel.Sandbox) error {
	if a.quiet {
		sandboxes = sortedByCreatedAt(sandboxes, func(sandbox apimodel.Sandbox) time.Time { return sandbox.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), sandboxes, func(sandbox apimodel.Sandbox) string { return sandbox.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"sandboxes": sandboxes})
	}
	sandboxes = sortedByCreatedAt(sandboxes, func(sandbox apimodel.Sandbox) time.Time { return sandbox.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPHASE\tDESIRED\tGENERATION\tERROR\tUPDATED")
	for _, sandbox := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			shortID(sandbox.ID),
			sandbox.Config.Name,
			sandbox.Runtime.Phase,
			sandbox.Runtime.DesiredState,
			sandbox.Runtime.Generation,
			truncateTableValue(sandboxMessage(sandbox), 80),
			formatTime(sandbox.UpdatedAt),
		)
	}
	return tw.Flush()
}

func sandboxMessage(sandbox apimodel.Sandbox) string {
	if message, ok := sandbox.Runtime.ErrorMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return message
	}
	return ""
}

func (a *App) writeProviderCatalog(cmd *cobra.Command, providers []apimodel.SandboxProviderCatalogItem) error {
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), providers, func(provider apimodel.SandboxProviderCatalogItem) string { return provider.ID })
	}
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

func (a *App) writeProvider(cmd *cobra.Command, provider *apimodel.SandboxProviderInstance) error {
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
	if status, ok := compactProviderStatusFromProvider(*provider); ok {
		fmt.Fprintf(tw, "STATUS\t%s\n", formatCompactProviderStatus(status))
		if status.LastError != "" {
			fmt.Fprintf(tw, "ERROR\t%s\n", truncateTableValue(status.LastError, 120))
		}
	}
	return tw.Flush()
}

func (a *App) writeProviders(cmd *cobra.Command, providers []apimodel.SandboxProviderInstance) error {
	if a.quiet {
		providers = sortedByCreatedAt(providers, func(provider apimodel.SandboxProviderInstance) time.Time { return provider.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), providers, func(provider apimodel.SandboxProviderInstance) string { return provider.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providers": providers})
	}
	providers = sortedByCreatedAt(providers, func(provider apimodel.SandboxProviderInstance) time.Time { return provider.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tDISABLED\tSTATUS\tERROR\tUPDATED")
	for _, provider := range providers {
		status, _ := compactProviderStatusFromProvider(provider)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			shortID(provider.ID),
			provider.Name,
			provider.Type,
			provider.Disabled,
			formatCompactProviderStatus(status),
			truncateTableValue(status.LastError, 80),
			formatTime(provider.UpdatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writeWorkers(cmd *cobra.Command, workers []apimodel.Worker) error {
	if a.quiet {
		workers = sortedByCreatedAt(workers, func(worker apimodel.Worker) time.Time { return worker.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), workers, func(worker apimodel.Worker) string { return worker.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"workers": workers})
	}
	workers = sortedByCreatedAt(workers, func(worker apimodel.Worker) time.Time { return worker.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROVIDER\tPHASE\tREADY\tSCHEDULABLE\tDEGRADED\tCPU\tMEMORY\tSTORAGE\tUPDATED\tMESSAGE")
	for _, worker := range workers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%t\t%t\t%.2f\t%s\t%s\t%s\t%s\n",
			shortID(worker.ID),
			shortID(worker.ProviderInstanceId),
			worker.Phase,
			worker.Ready,
			worker.Schedulable,
			worker.Degraded,
			worker.AvailableCpuVcpus,
			formatBytes(worker.AvailableMemoryBytes),
			formatBytes(worker.AvailableStorageBytes),
			formatTime(worker.UpdatedAt),
			truncateTableValue(workerMessage(worker), 80),
		)
	}
	return tw.Flush()
}

func workerMessage(worker apimodel.Worker) string {
	if message, ok := worker.ErrorMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return message
	}
	if message, ok := worker.StatusMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return message
	}
	return ""
}

func (a *App) writeAgentDefinition(cmd *cobra.Command, definition *apimodel.AgentConfigDefinition) error {
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

func (a *App) writeAgentDefinitions(cmd *cobra.Command, definitions []apimodel.AgentConfigDefinition) error {
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), definitions, func(definition apimodel.AgentConfigDefinition) string { return definition.ID })
	}
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

func (a *App) writeAgent(cmd *cobra.Command, agent *apimodel.AgentConfig) error {
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

func (a *App) writeAgents(cmd *cobra.Command, agents []apimodel.AgentConfig, defaultAgentConfigID ...string) error {
	if a.quiet {
		agents = sortedByCreatedAt(agents, func(agent apimodel.AgentConfig) time.Time { return agent.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), agents, func(agent apimodel.AgentConfig) string { return agent.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"agentConfigs": agents})
	}
	defaultID := ""
	if len(defaultAgentConfigID) > 0 {
		defaultID = defaultAgentConfigID[0]
	}
	agents = sortedByCreatedAt(agents, func(agent apimodel.AgentConfig) time.Time { return agent.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tDEFAULT\tRUN COMMAND\tUPDATED")
	for _, agent := range agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", shortID(agent.ID), agent.Name, formatDefaultMarker(agent.ID == defaultID), agent.RunCommand, formatTime(agent.UpdatedAt))
	}
	return tw.Flush()
}

func formatDefaultMarker(isDefault bool) string {
	if isDefault {
		return "yes"
	}
	return ""
}

func (a *App) writeJobs(cmd *cobra.Command, jobs []apimodel.Job) error {
	if a.quiet {
		jobs = sortedByCreatedAt(jobs, func(job apimodel.Job) time.Time { return job.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), jobs, func(job apimodel.Job) string { return job.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"jobs": jobs})
	}
	jobs = sortedByCreatedAt(jobs, func(job apimodel.Job) time.Time { return job.CreatedAt })
	now := time.Now()
	rows := make([][]string, 0, len(jobs))
	errors := make([]string, 0, len(jobs))
	for _, job := range jobs {
		message := truncateTableValue(job.Message.Or(""), 40)
		rows = append(rows, []string{
			shortID(job.ID),
			job.Type,
			string(job.Status),
			fmt.Sprintf("%d/%d", job.Attempts, job.MaxAttempts),
			job.ResourceType + "/" + shortID(job.ResourceId),
			formatTime(job.CreatedAt),
			formatFutureTime(now, job.ScheduledAt),
			message,
		})
		errors = append(errors, compactTableValue(job.Error.Or("")))
	}
	errorWidth := jobsTableErrorWidth(terminalWidth(cmd.OutOrStdout()), rows)
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tATTEMPTS\tRESOURCE\tCREATED\tNEXT\tMESSAGE\tERROR")
	for i, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row[0],
			row[1],
			row[2],
			row[3],
			row[4],
			row[5],
			row[6],
			row[7],
			truncateTableValue(errors[i], errorWidth),
		)
	}
	return tw.Flush()
}

func (a *App) writeStatus(cmd *cobra.Command, status statusSnapshot) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), status)
	}
	w := cmd.OutOrStdout()
	if err := writeStatusSandboxes(w, status.Sandboxes); err != nil {
		return err
	}
	if err := writeStatusWorkers(w, status.Workers); err != nil {
		return err
	}
	if err := writeStatusProviders(w, status.Providers); err != nil {
		return err
	}
	return writeStatusJobs(w, status.Jobs)
}

func writeStatusSandboxes(w io.Writer, sandboxes []apimodel.Sandbox) error {
	if _, err := fmt.Fprintln(w, "Sandboxes"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPHASE\tDESIRED\tUPDATED\tMESSAGE")
	if len(sandboxes) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\t-")
	}
	for _, sandbox := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(sandbox.ID),
			sandbox.Config.Name,
			sandbox.Runtime.Phase,
			sandbox.Runtime.DesiredState,
			formatTime(sandbox.UpdatedAt),
			truncateTableValue(sandboxMessage(sandbox), 64),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func writeStatusWorkers(w io.Writer, workers []apimodel.Worker) error {
	if _, err := fmt.Fprintln(w, "Workers"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPROVIDER\tPHASE\tREADY\tSCHED\tUPDATED\tMESSAGE")
	if len(workers) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\t-\t-")
	}
	for _, worker := range workers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%t\t%s\t%s\n",
			shortID(worker.ID),
			shortID(worker.ProviderInstanceId),
			worker.Phase,
			worker.Ready,
			worker.Schedulable,
			formatTime(worker.UpdatedAt),
			truncateTableValue(workerMessage(worker), 64),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func writeStatusProviders(w io.Writer, providers []apimodel.SandboxProviderInstance) error {
	if _, err := fmt.Fprintln(w, "Providers"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tDISABLED\tWORKERS\tUPDATED\tERROR")
	if len(providers) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\t-\t-")
	}
	for _, provider := range providers {
		status, _ := compactProviderStatusFromProvider(provider)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			shortID(provider.ID),
			provider.Name,
			provider.Type,
			provider.Disabled,
			formatCompactProviderStatus(status),
			formatTime(provider.UpdatedAt),
			truncateTableValue(status.LastError, 64),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func writeStatusJobs(w io.Writer, jobs []apimodel.Job) error {
	if _, err := fmt.Fprintln(w, "Jobs"); err != nil {
		return err
	}
	now := time.Now()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tATTEMPTS\tRESOURCE\tNEXT\tMESSAGE\tERROR")
	if len(jobs) == 0 {
		fmt.Fprintln(tw, "-\t-\t-\t-\t-\t-\t-\t-")
	}
	for _, job := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\t%s\n",
			shortID(job.ID),
			job.Type,
			job.Status,
			job.Attempts,
			job.MaxAttempts,
			shortResourceID(job.ResourceType, job.ResourceId),
			formatFutureTime(now, job.ScheduledAt),
			truncateTableValue(job.Message.Or(""), 40),
			truncateTableValue(job.Error.Or(""), 64),
		)
	}
	return tw.Flush()
}

func sortedByCreatedAt[T any](values []T, createdAt func(T) time.Time) []T {
	out := append([]T(nil), values...)
	sort.SliceStable(out, func(i, j int) bool {
		return createdAt(out[i]).Before(createdAt(out[j]))
	})
	return out
}

func newestByCreatedAt[T any](values []T, createdAt func(T) time.Time, limit int) []T {
	out := append([]T(nil), values...)
	sort.SliceStable(out, func(i, j int) bool {
		return createdAt(out[i]).After(createdAt(out[j]))
	})
	if limit >= 0 && len(out) > limit {
		return out[:limit]
	}
	return out
}

func (a *App) writeJob(cmd *cobra.Command, job *apimodel.Job) error {
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
	if job.Message.Set && job.Message.Value != "" {
		fmt.Fprintf(tw, "MESSAGE\t%s\n", job.Message.Value)
	}
	if metadata := rawTableValue(job.Metadata); metadata != "" {
		fmt.Fprintf(tw, "METADATA\t%s\n", metadata)
	}
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

func formatFutureTime(now, value time.Time) string {
	if value.IsZero() || !value.After(now) {
		return ""
	}
	return formatRelativeTime(now, value)
}

func formatBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%dB", value)
	}
	div := int64(unit)
	exp := 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(value)/float64(div), "KMGTPE"[exp])
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

func rawTableValue(value []byte) string {
	value = []byte(strings.TrimSpace(string(value)))
	if len(value) == 0 || string(value) == "null" {
		return ""
	}
	return string(value)
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
		len("NEXT"),
		len("MESSAGE"),
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
	// Nine table columns produce eight gaps in tabwriter's padded output.
	used += separatorWidth * 8
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

type compactProviderStatus struct {
	WorkerCount        int
	ReadyWorkers       int
	SchedulableWorkers int
	DegradedWorkers    int
	FailedWorkers      int
	LastError          string
}

func compactProviderStatusFromProvider(provider apimodel.SandboxProviderInstance) (compactProviderStatus, bool) {
	if status, ok := provider.GetStatus().Get(); ok {
		return compactProviderStatusFromInstanceStatus(status), true
	}
	if workers, ok := provider.GetWorkers().Get(); ok {
		return compactProviderStatusFromWorkers(workers), true
	}
	return compactProviderStatus{}, false
}

func compactProviderStatusFromInstanceStatus(status apimodel.SandboxProviderInstanceStatus) compactProviderStatus {
	out := compactProviderStatus{
		WorkerCount:        int(status.WorkerCount),
		ReadyWorkers:       int(status.ReadyWorkers),
		SchedulableWorkers: int(status.SchedulableWorkers),
		DegradedWorkers:    int(status.DegradedWorkers),
		FailedWorkers:      int(status.FailedWorkers),
	}
	if lastError, ok := status.LastError.Get(); ok {
		out.LastError = strings.TrimSpace(lastError)
	}
	if out.LastError == "" {
		if workers, ok := status.Workers.Get(); ok {
			for _, worker := range workers {
				if message := providerWorkerStatusMessage(worker); message != "" {
					out.LastError = message
					break
				}
			}
		}
	}
	return out
}

func compactProviderStatusFromWorkers(workers []apimodel.Worker) compactProviderStatus {
	var out compactProviderStatus
	for _, worker := range workers {
		if providerWorkerDeleted(worker) {
			if out.LastError == "" {
				out.LastError = workerMessage(worker)
			}
			continue
		}
		out.WorkerCount++
		if worker.Ready {
			out.ReadyWorkers++
		}
		if worker.Schedulable {
			out.SchedulableWorkers++
		}
		if worker.Degraded {
			out.DegradedWorkers++
		}
		if string(worker.Phase) == "failed" || string(worker.LastOperationStatus) == "failed" || worker.ErrorMessage.Set {
			out.FailedWorkers++
		}
		if out.LastError == "" {
			out.LastError = workerMessage(worker)
		}
	}
	return out
}

func providerWorkerDeleted(worker apimodel.Worker) bool {
	return string(worker.DesiredState) == "deleted" || string(worker.Phase) == "deleted"
}

func providerWorkerStatusMessage(worker apimodel.ProviderWorkerStatus) string {
	if message, ok := worker.ErrorMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	if message, ok := worker.StatusMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return ""
}

func formatCompactProviderStatus(status compactProviderStatus) string {
	if status.WorkerCount == 0 {
		return "0 workers"
	}
	parts := []string{fmt.Sprintf("%d/%d ready", status.ReadyWorkers, status.WorkerCount)}
	if status.SchedulableWorkers != status.ReadyWorkers {
		parts = append(parts, fmt.Sprintf("%d sched", status.SchedulableWorkers))
	}
	if status.DegradedWorkers > 0 {
		parts = append(parts, fmt.Sprintf("%d degraded", status.DegradedWorkers))
	}
	if status.FailedWorkers > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", status.FailedWorkers))
	}
	return strings.Join(parts, ", ")
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
