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
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tERROR\tUPDATED")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
		sandbox.ID,
		sandbox.Config.Name,
		sandboxDisplayState(*sandbox),
		truncateTableValue(sandboxMessage(*sandbox), 80),
		formatTime(sandbox.UpdatedAt),
	)
	return tw.Flush()
}

func (a *App) writeSandboxes(cmd *cobra.Command, sandboxes []apimodel.Sandbox, showFolder bool) error {
	if a.quiet {
		sandboxes = sortedByCreatedAt(sandboxes, func(sandbox apimodel.Sandbox) time.Time { return sandbox.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), sandboxes, func(sandbox apimodel.Sandbox) string { return sandbox.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"sandboxes": sandboxes})
	}
	sandboxes = sortedByCreatedAt(sandboxes, func(sandbox apimodel.Sandbox) time.Time { return sandbox.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if showFolder {
		fmt.Fprintln(tw, "ID\tNAME\tSTATE\tUPGRADE\tERROR\tUPDATED\tFOLDER")
	} else {
		fmt.Fprintln(tw, "ID\tNAME\tSTATE\tUPGRADE\tERROR\tUPDATED")
	}
	for _, sandbox := range sandboxes {
		if showFolder {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				sandbox.ID,
				sandbox.Config.Name,
				sandboxDisplayState(sandbox),
				sandboxUpgradeState(sandbox),
				truncateTableValue(sandboxMessage(sandbox), 80),
				formatTime(sandbox.UpdatedAt),
				sandboxFolder(sandbox),
			)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			sandbox.ID,
			sandbox.Config.Name,
			sandboxDisplayState(sandbox),
			sandboxUpgradeState(sandbox),
			truncateTableValue(sandboxMessage(sandbox), 80),
			formatTime(sandbox.UpdatedAt),
		)
	}
	return tw.Flush()
}

// sandboxFolder is the client-side project directory a sandbox was started
// from, taken from its origin. It is empty for sandboxes created without an
// origin (for example directly through the API).
func sandboxFolder(sandbox apimodel.Sandbox) string {
	origin, ok := sandbox.Origin.Get()
	if !ok {
		return "-"
	}
	return origin.ProjectPath
}

func sandboxMessage(sandbox apimodel.Sandbox) string {
	if message, ok := sandbox.Runtime.ErrorMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return message
	}
	return ""
}

func sandboxDisplayState(sandbox apimodel.Sandbox) string {
	if state, ok := sandbox.Runtime.DisplayState.Get(); ok {
		return string(state)
	}
	return "-"
}

// sandboxUpgradeState marks sandboxes running an older image than their harness
// config now resolves to. "-" covers both up-to-date and unpinned sandboxes:
// neither has anything to act on, and distinguishing them in a table column
// would explain image pinning to everyone who runs ls.
func sandboxUpgradeState(sandbox apimodel.Sandbox) string {
	upgrade, ok := sandbox.Runtime.Upgrade.Get()
	if !ok || !upgrade.Available {
		return "-"
	}
	return "available"
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
	fmt.Fprintf(tw, "ID\t%s\n", provider.ID)
	fmt.Fprintf(tw, "NAME\t%s\n", provider.Name)
	fmt.Fprintf(tw, "TYPE\t%s\n", provider.Type)
	fmt.Fprintf(tw, "DISABLED\t%t\n", provider.Disabled)
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(provider.UpdatedAt))
	fmt.Fprintf(tw, "CONFIG\t%s\n", formatRedactedRawJSON(provider.GetConfig()))
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
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tDISABLED\tUPDATED")
	for _, provider := range providers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n",
			provider.ID,
			provider.Name,
			provider.Type,
			provider.Disabled,
			formatTime(provider.UpdatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writePool(cmd *cobra.Command, pool *apimodel.Pool) error {
	if pool == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), pool)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", pool.ID)
	fmt.Fprintf(tw, "NAME\t%s\n", pool.Name)
	fmt.Fprintf(tw, "PROVIDER\t%s\n", pool.ProviderInstanceId)
	fmt.Fprintf(tw, "ENVELOPE CPU\t%s\n", formatPoolCPU(pool.CpuVcpus))
	fmt.Fprintf(tw, "ENVELOPE MEMORY\t%s\n", formatPoolBytes(pool.MemoryBytes))
	fmt.Fprintf(tw, "ENVELOPE STORAGE\t%s\n", formatPoolBytes(pool.StorageBytes))
	fmt.Fprintf(tw, "PHASE\t%s\n", pool.Phase)
	fmt.Fprintf(tw, "READY\t%t\n", pool.Ready)
	fmt.Fprintf(tw, "SCHEDULABLE\t%t\n", pool.Schedulable)
	fmt.Fprintf(tw, "CAPACITY\t%s\n", formatPoolCapacity(*pool))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(pool.UpdatedAt))
	if message := poolMessage(*pool); message != "" {
		fmt.Fprintf(tw, "MESSAGE\t%s\n", truncateTableValue(message, 120))
	}
	return tw.Flush()
}

func (a *App) writePools(cmd *cobra.Command, pools []apimodel.Pool, defaultPoolID ...string) error {
	if a.quiet {
		pools = sortedByCreatedAt(pools, func(pool apimodel.Pool) time.Time { return pool.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), pools, func(pool apimodel.Pool) string { return pool.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"pools": pools})
	}
	defaultID := ""
	if len(defaultPoolID) > 0 {
		defaultID = defaultPoolID[0]
	}
	pools = sortedByCreatedAt(pools, func(pool apimodel.Pool) time.Time { return pool.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPROVIDER\tDEFAULT\tPHASE\tREADY\tCPU\tMEMORY\tSTORAGE\tUPDATED\tMESSAGE")
	for _, pool := range pools {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\t%s\n",
			pool.ID,
			pool.Name,
			pool.ProviderInstanceId,
			formatDefaultMarker(pool.ID == defaultID),
			pool.Phase,
			pool.Ready,
			formatPoolCPU(pool.CpuVcpus),
			formatPoolBytes(pool.MemoryBytes),
			formatPoolBytes(pool.StorageBytes),
			formatTime(pool.UpdatedAt),
			truncateTableValue(poolMessage(pool), 80),
		)
	}
	return tw.Flush()
}

// formatPoolCPU renders a pool envelope CPU value, where zero means the
// envelope is sized by the host.
func formatPoolCPU(value float64) string {
	if value <= 0 {
		return "host"
	}
	return fmt.Sprintf("%.2f", value)
}

// formatPoolBytes renders a pool envelope byte value, where zero means the
// envelope is sized by the host.
func formatPoolBytes(value int64) string {
	if value <= 0 {
		return "host"
	}
	return formatBytes(value)
}

// formatPoolCapacity renders agent-reported available capacity.
func formatPoolCapacity(pool apimodel.Pool) string {
	return fmt.Sprintf("%.2f vCPU, %s memory, %s storage", pool.AvailableCpuVcpus, formatBytes(pool.AvailableMemoryBytes), formatBytes(pool.AvailableStorageBytes))
}

// poolMessage surfaces the most relevant human-readable detail on the pool.
func poolMessage(pool apimodel.Pool) string {
	if message, ok := pool.ErrorMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return message
	}
	if message, ok := pool.StatusMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return message
	}
	return ""
}

func (a *App) writeSecret(cmd *cobra.Command, secret *apimodel.Secret) error {
	if secret == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), secret)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", secret.ID)
	fmt.Fprintf(tw, "NAME\t%s\n", secret.Name)
	fmt.Fprintf(tw, "TYPE\t%s\n", secret.Type)
	fmt.Fprintf(tw, "HOST\t%s\n", secret.Host.Or(""))
	fmt.Fprintf(tw, "GRANT TTL\t%s\n", formatSeconds(secret.DefaultGrantTTLSeconds))
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(secret.CreatedAt))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(secret.UpdatedAt))
	return tw.Flush()
}

func (a *App) writeSecrets(cmd *cobra.Command, secrets []apimodel.Secret) error {
	if a.quiet {
		secrets = sortedByCreatedAt(secrets, func(secret apimodel.Secret) time.Time { return secret.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), secrets, func(secret apimodel.Secret) string { return secret.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"secrets": secrets})
	}
	secrets = sortedByCreatedAt(secrets, func(secret apimodel.Secret) time.Time { return secret.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tHOST\tGRANT TTL\tUPDATED")
	for _, secret := range secrets {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			secret.ID,
			secret.Name,
			secret.Type,
			secret.Host.Or(""),
			formatSeconds(secret.DefaultGrantTTLSeconds),
			formatTime(secret.UpdatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writeSecretGrant(cmd *cobra.Command, grant *apimodel.SecretGrant) error {
	if grant == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), grant)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", grant.ID)
	fmt.Fprintf(tw, "SECRET\t%s\n", grant.SecretId)
	fmt.Fprintf(tw, "SCOPE\t%s\n", grant.Scope)
	fmt.Fprintf(tw, "SCOPE KEY\t%s\n", grant.ScopeKey)
	fmt.Fprintf(tw, "HOST\t%s\n", grant.Host.Or("(any)"))
	fmt.Fprintf(tw, "EXPIRES\t%s\n", formatGrantExpiry(grant))
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(grant.CreatedAt))
	return tw.Flush()
}

func (a *App) writeSecretGrants(cmd *cobra.Command, grants []apimodel.SecretGrant) error {
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), grants, func(grant apimodel.SecretGrant) string { return grant.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"secretGrants": grants})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSECRET\tSCOPE\tSCOPE KEY\tHOST\tEXPIRES")
	for _, grant := range grants {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			grant.ID,
			grant.SecretId,
			grant.Scope,
			grant.ScopeKey,
			grant.Host.Or("(any)"),
			formatGrantExpiry(&grant),
		)
	}
	return tw.Flush()
}

func formatGrantExpiry(grant *apimodel.SecretGrant) string {
	if expiresAt, ok := grant.ExpiresAt.Get(); ok && !expiresAt.IsZero() {
		return formatTime(expiresAt)
	}
	return "never"
}

func (a *App) writeSecretRequest(cmd *cobra.Command, request *apimodel.SecretRequest) error {
	if request == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), request)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", request.ID)
	fmt.Fprintf(tw, "REQUESTED BY\t%s\n", request.RequestedBy)
	fmt.Fprintf(tw, "TYPE\t%s\n", request.Type)
	fmt.Fprintf(tw, "HOST\t%s\n", request.Host.Or(""))
	fmt.Fprintf(tw, "STATUS\t%s\n", request.Status)
	if secretID, ok := request.SecretId.Get(); ok && secretID != "" {
		fmt.Fprintf(tw, "SECRET\t%s\n", secretID)
	}
	if grantID, ok := request.GrantId.Get(); ok && grantID != "" {
		fmt.Fprintf(tw, "GRANT\t%s\n", grantID)
	}
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(request.CreatedAt))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(request.UpdatedAt))
	return tw.Flush()
}

func (a *App) writeSecretRequests(cmd *cobra.Command, requests []apimodel.SecretRequest) error {
	if a.quiet {
		requests = sortedByCreatedAt(requests, func(request apimodel.SecretRequest) time.Time { return request.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), requests, func(request apimodel.SecretRequest) string { return request.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"secretRequests": requests})
	}
	requests = sortedByCreatedAt(requests, func(request apimodel.SecretRequest) time.Time { return request.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tHOST\tSTATUS\tSECRET\tSANDBOX\tREQUESTED BY\tUPDATED")
	for _, request := range requests {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			request.ID,
			request.Type,
			request.Host.Or(""),
			request.Status,
			request.SecretId.Or(""),
			request.SandboxId.Or(""),
			request.RequestedBy,
			formatTime(request.UpdatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writeHarness(cmd *cobra.Command, harness *apimodel.HarnessConfig) error {
	if harness == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), harness)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSLUG\tNAME\tCONFIGURED\tRUN COMMAND\tSECRETS\tUPDATED")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", harness.ID, harness.Slug, harness.Name, formatConfigured(harness), strings.Join(harness.RunCommand, " "), formatHarnessSecrets(harness.Secrets.Or(nil)), formatTime(harness.UpdatedAt))
	return tw.Flush()
}

func (a *App) writeHarnesses(cmd *cobra.Command, harnesses []apimodel.HarnessConfig, defaultHarnessConfigID ...string) error {
	if a.quiet {
		harnesses = sortedByCreatedAt(harnesses, func(harness apimodel.HarnessConfig) time.Time { return harness.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), harnesses, func(harness apimodel.HarnessConfig) string { return harness.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"harnessConfigs": harnesses})
	}
	defaultID := ""
	if len(defaultHarnessConfigID) > 0 {
		defaultID = defaultHarnessConfigID[0]
	}
	harnesses = sortedByCreatedAt(harnesses, func(harness apimodel.HarnessConfig) time.Time { return harness.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSLUG\tNAME\tCONFIGURED\tDEFAULT\tRUN COMMAND\tUPDATED")
	for _, harness := range harnesses {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", harness.ID, harness.Slug, harness.Name, formatConfigured(&harness), formatDefaultMarker(harness.ID == defaultID), strings.Join(harness.RunCommand, " "), formatTime(harness.UpdatedAt))
	}
	return tw.Flush()
}

// formatConfigured renders whether a harness has completed its configure flow.
// Only a configured harness can be run, so this is the column that explains why
// `disco run -H <slug>` is refused. A failed attempt shows its reason.
func formatConfigured(harness *apimodel.HarnessConfig) string {
	if harness.Configured {
		return "yes"
	}
	if reason := strings.TrimSpace(harness.ConfigureError.Or("")); reason != "" {
		return "no (failed)"
	}
	return "no"
}

func (a *App) writeHarnessSecretBindings(cmd *cobra.Command, declarations []apimodel.HarnessConfigSecret, bindings []apimodel.HarnessConfigSecretBinding) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"secrets": declarations, "secretBindings": bindings})
	}
	boundByEnv := make(map[string]string, len(bindings))
	for _, b := range bindings {
		boundByEnv[b.EnvName] = b.SecretId
	}
	// Show every declared env var, then any binding for an env the definition
	// didn't declare (e.g. a custom secret the user added).
	seen := map[string]struct{}{}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ENV\tREQUIRED\tONE-OF GROUP\tBOUND SECRET")
	for _, decl := range declarations {
		seen[decl.Name] = struct{}{}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", decl.Name, formatRequired(decl.Required.Or(false)), formatOneOfGroup(decl.OneOfGroup.Or("")), formatBoundSecret(boundByEnv[decl.Name]))
	}
	for _, b := range bindings {
		if _, ok := seen[b.EnvName]; ok {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.EnvName, "-", "-", formatBoundSecret(b.SecretId))
	}
	return tw.Flush()
}

func formatRequired(required bool) string {
	if required {
		return "yes"
	}
	return "no"
}

func formatOneOfGroup(group string) string {
	if group == "" {
		return "-"
	}
	return group
}

func formatBoundSecret(secretID string) string {
	if secretID == "" {
		return "—"
	}
	return secretID
}

func formatHarnessSecrets(secrets []apimodel.HarnessConfigSecret) string {
	if len(secrets) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret.Required.Or(false) {
			parts = append(parts, secret.Name+" (required)")
		} else {
			parts = append(parts, secret.Name+" (optional)")
		}
	}
	return strings.Join(parts, ", ")
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
			job.ID,
			job.Type,
			string(job.Status),
			fmt.Sprintf("%d/%d", job.Attempts, job.MaxAttempts),
			job.ResourceType + "/" + job.ResourceId,
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
	fmt.Fprintf(tw, "ID\t%s\n", job.ID)
	fmt.Fprintf(tw, "TYPE\t%s\n", job.Type)
	fmt.Fprintf(tw, "STATUS\t%s\n", job.Status)
	fmt.Fprintf(tw, "ATTEMPTS\t%d/%d\n", job.Attempts, job.MaxAttempts)
	if job.WorkerId.Set && job.WorkerId.Value != "" {
		fmt.Fprintf(tw, "WORKER\t%s\n", job.WorkerId.Value)
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

func formatSeconds(value int64) string {
	if value <= 0 {
		return ""
	}
	return (time.Duration(value) * time.Second).String()
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
