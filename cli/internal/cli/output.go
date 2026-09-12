package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
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
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tHARNESS\tGIT\tCHANGES\tERROR\tUPDATED")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		sandbox.ID,
		truncateTableValue(sandbox.DisplayName, sandboxNameColumnWidth),
		sandboxDisplayState(*sandbox),
		sandboxHarness(*sandbox),
		sandboxGitColumn(*sandbox),
		sandboxGitStatus(*sandbox).changes(sandboxSpawnCommit(*sandbox)),
		truncateTableValue(sandboxMessage(*sandbox), 80),
		formatTime(sandbox.UpdatedAt),
	)
	return tw.Flush()
}

// writeSandboxes lists sandboxes newest-created first, the same order and
// row the launcher's list draws: state, harness, where the work sits in git,
// and whether that work has landed anywhere. Creation is the one timestamp a
// user's action put there: nothing yet records real access (the runtime's
// LastActiveAt moves for reconciler-driven reasons), and a list that reorders
// for reasons the user did not cause is a list they cannot read.
//
// serverOf names the server each sandbox is on, by ID, when there is more than
// one (ADR 0116 §4): the table gains a SERVER column and each JSON object a
// "server" field. Nil is one server, which there is no point naming.
func (a *App) writeSandboxes(cmd *cobra.Command, sandboxes []apimodel.Sandbox, showSource bool, serverOf map[string]string) error {
	sandboxes = sortedByRecency(sandboxes, func(sandbox apimodel.Sandbox) time.Time { return sandbox.CreatedAt })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), sandboxes, func(sandbox apimodel.Sandbox) string { return sandbox.ID })
	}
	if a.output == "json" {
		if serverOf == nil {
			return writeJSON(cmd.OutOrStdout(), map[string]any{"sandboxes": sandboxes})
		}
		rows := make([]json.RawMessage, 0, len(sandboxes))
		for _, sandbox := range sandboxes {
			row, err := withServerField(sandbox, serverOf[sandbox.ID])
			if err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return writeJSON(cmd.OutOrStdout(), map[string]any{"sandboxes": rows})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	header := []string{"ID", "NAME"}
	if serverOf != nil {
		header = append(header, "SERVER")
	}
	header = append(header, "STATE", "HARNESS", "GIT", "CHANGES", "DIFF", "UPGRADE", "ERROR", "CREATED")
	if showSource {
		header = append(header, "SOURCE")
	}
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, sandbox := range sandboxes {
		git := sandboxGitStatus(sandbox)
		fmt.Fprintf(tw, "%s\t%s\t", sandbox.ID, truncateTableValue(sandbox.DisplayName, sandboxNameColumnWidth))
		if serverOf != nil {
			fmt.Fprintf(tw, "%s\t", serverOf[sandbox.ID])
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			sandboxDisplayState(sandbox),
			sandboxHarness(sandbox),
			sandboxGitColumn(sandbox),
			git.changes(sandboxSpawnCommit(sandbox)),
			git.diffColumn(),
			sandboxUpgradeState(sandbox),
			truncateTableValue(sandboxMessage(sandbox), 80),
			formatTime(sandbox.CreatedAt),
		)
		if showSource {
			fmt.Fprintf(tw, "\t%s", sandboxSource(sandbox))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

// withServerField is sandbox's JSON with the server it is on added in front of
// its own fields. It marshals through a pointer: the generated types encode
// themselves only from one, and a value falls back to reflection that cannot
// encode an unset optional field.
func withServerField(sandbox apimodel.Sandbox, server string) (json.RawMessage, error) {
	data, err := json.Marshal(&sandbox)
	if err != nil {
		return nil, err
	}
	name, err := json.Marshal(server)
	if err != nil {
		return nil, err
	}
	out := append([]byte(`{"server":`), name...)
	if body := bytes.TrimSpace(data[1:]); len(body) > 1 {
		out = append(out, ',')
		out = append(out, body...)
	} else {
		out = append(out, '}')
	}
	return out, nil
}

// sandboxHarness is the harness the sandbox runs, by the name a user would
// type; "-" for a sandbox that is just a shell.
func sandboxHarness(sandbox apimodel.Sandbox) string {
	cfg, ok := sandbox.HarnessConfig.Get()
	if !ok {
		return "-"
	}
	if cfg.Slug != "" {
		return cfg.Slug
	}
	if cfg.Name != "" {
		return cfg.Name
	}
	return "-"
}

// sandboxGitColumn is where the sandbox's primary source sits in git: the
// reported branch@commit, starred when the working tree is dirty. Until an
// agent reports, it falls back to the position the sandbox was spawned at,
// starred when a snapshot of uncommitted work was carried in — the launcher's
// base column, spelled the same way.
func sandboxGitColumn(sandbox apimodel.Sandbox) string {
	if position := sandboxGitStatus(sandbox).position(); position != "" {
		return position
	}
	source, ok := sandbox.Config.Source.Get()
	if !ok {
		return "-"
	}
	checkout, ok := source.Checkout.Get()
	if !ok {
		return "-"
	}
	branch := strings.TrimSpace(checkout.RefName.Or(""))
	commit := shortCommit(strings.TrimSpace(checkout.Commit.Or("")))
	if branch == "" && commit == "" {
		return "-"
	}
	out := branch + "@" + commit
	if sourceSnapshotRef(source) != "" {
		out += "*"
	}
	return out
}

// sandboxSource is where a sandbox's primary source came from — the local
// directory it was cut from, or the repository URL — which is where its origin
// key files it (ADR 0111). "-" for a sandbox with no source.
func sandboxSource(sandbox apimodel.Sandbox) string {
	source, ok := sandbox.Config.Source.Get()
	if !ok {
		return "-"
	}
	if u, ok := source.URL.Get(); ok {
		return u.String()
	}
	if dir := strings.TrimSpace(source.LocalDirectory.Or("")); dir != "" {
		return dir
	}
	return "-"
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
	fmt.Fprintln(tw, "ID\tNAME\tDESCRIPTION")
	for _, provider := range providers {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", provider.ID, provider.Name, provider.Description.Or(""))
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
	providers = sortedByRecency(providers, func(provider apimodel.SandboxProviderInstance) time.Time {
		return recencyTime(provider.UpdatedAt, provider.CreatedAt)
	})
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), providers, func(provider apimodel.SandboxProviderInstance) string { return provider.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providers": providers})
	}
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

func (a *App) writeProjects(cmd *cobra.Command, projects []apimodel.Project) error {
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), projects, func(project apimodel.Project) string { return project.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"projects": projects})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tDEFAULT\tCREATED")
	for _, project := range projects {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			project.ID,
			project.Name,
			formatDefaultMarker(project.Default),
			formatTime(project.CreatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writeProject(cmd *cobra.Command, project *apimodel.Project) error {
	if project == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), project)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", project.ID)
	fmt.Fprintf(tw, "NAME\t%s\n", project.Name)
	fmt.Fprintf(tw, "DEFAULT\t%t\n", project.Default)
	if poolID := project.DefaultPoolId.Or(""); poolID != "" {
		fmt.Fprintf(tw, "DEFAULT POOL\t%s\n", poolID)
	}
	if harnessID := project.DefaultHarnessConfigId.Or(""); harnessID != "" {
		fmt.Fprintf(tw, "DEFAULT HARNESS\t%s\n", harnessID)
	}
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(project.CreatedAt))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(project.UpdatedAt))
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
	fmt.Fprintf(tw, "STATE\t%s\n", pool.State)
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
	pools = sortedByRecency(pools, func(pool apimodel.Pool) time.Time { return recencyTime(pool.UpdatedAt, pool.CreatedAt) })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), pools, func(pool apimodel.Pool) string { return pool.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"pools": pools})
	}
	defaultID := ""
	if len(defaultPoolID) > 0 {
		defaultID = defaultPoolID[0]
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPROVIDER\tDEFAULT\tSTATE\tREADY\tCPU\tMEMORY\tSTORAGE\tUPDATED\tMESSAGE")
	for _, pool := range pools {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\t%s\n",
			pool.ID,
			pool.Name,
			pool.ProviderInstanceId,
			formatDefaultMarker(pool.ID == defaultID),
			pool.State,
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
//
// It is only ever the error: a status message would be narration of an
// operation in flight, and operations are not a thing the control plane
// records (ADR 0017 §2).
func poolMessage(pool apimodel.Pool) string {
	if message, ok := pool.ErrorMessage.Get(); ok && strings.TrimSpace(message) != "" {
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
	fmt.Fprintf(tw, "MAX GRANT TTL\t%s\n", formatGrantLimit(secret.MaxGrantTTLSeconds))
	// What an OAuth credential is, which is the half of it that can be shown:
	// where it renews, what it may do, and when the access token goes stale.
	if oauth, ok := secret.OAuth.Get(); ok {
		if url := strings.TrimSpace(oauth.TokenUrl.Or("")); url != "" {
			fmt.Fprintf(tw, "RENEWS AT\t%s\n", url)
		}
		if client := strings.TrimSpace(oauth.ClientId.Or("")); client != "" {
			fmt.Fprintf(tw, "CLIENT\t%s\n", client)
		}
		if scopes, ok := oauth.Scopes.Get(); ok && len(scopes) > 0 {
			fmt.Fprintf(tw, "SCOPES\t%s\n", strings.Join(scopes, " "))
		}
		if plan := strings.TrimSpace(oauth.SubscriptionType.Or("")); plan != "" {
			fmt.Fprintf(tw, "PLAN\t%s\n", plan)
		}
		if expires := oauth.AccessTokenExpiresAt.Or(0); expires > 0 {
			fmt.Fprintf(tw, "TOKEN EXPIRES\t%s\n", formatTime(time.UnixMilli(expires).UTC()))
		}
		// Whether it can renew itself at all: an OAuth credential that cannot
		// is one that will expire and stay expired.
		fmt.Fprintf(tw, "REFRESHABLE\t%t\n", oauth.Refreshable.Or(false))
	}
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(secret.CreatedAt))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(secret.UpdatedAt))
	return tw.Flush()
}

func (a *App) writeSecrets(cmd *cobra.Command, secrets []apimodel.Secret) error {
	secrets = sortedByRecency(secrets, func(secret apimodel.Secret) time.Time { return recencyTime(secret.UpdatedAt, secret.CreatedAt) })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), secrets, func(secret apimodel.Secret) string { return secret.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"secrets": secrets})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tHOST\tMAX GRANT TTL\tUPDATED")
	for _, secret := range secrets {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			secret.ID,
			secret.Name,
			secret.Type,
			secret.Host.Or(""),
			formatGrantLimit(secret.MaxGrantTTLSeconds),
			formatTime(secret.UpdatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writeSSHKey(cmd *cobra.Command, key *apimodel.SSHKey) error {
	if key == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), key)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", key.ID)
	fmt.Fprintf(tw, "NAME\t%s\n", key.Name.Or(""))
	fmt.Fprintf(tw, "FINGERPRINT\t%s\n", key.Fingerprint)
	fmt.Fprintf(tw, "COMMENT\t%s\n", key.Comment.Or(""))
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(key.CreatedAt))
	return tw.Flush()
}

func (a *App) writeSSHKeys(cmd *cobra.Command, keys []apimodel.SSHKey) error {
	keys = sortedByRecency(keys, func(key apimodel.SSHKey) time.Time { return recencyTime(key.UpdatedAt, key.CreatedAt) })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), keys, func(key apimodel.SSHKey) string { return key.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"sshKeys": keys})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tFINGERPRINT\tCOMMENT\tCREATED")
	for _, key := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			key.ID,
			key.Name.Or(""),
			key.Fingerprint,
			key.Comment.Or(""),
			formatTime(key.CreatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) writePeer(cmd *cobra.Command, enrolled *apimodel.Peer) error {
	if enrolled == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), enrolled)
	}
	fmt.Fprintln(cmd.OutOrStdout(), enrolled.ID)
	return nil
}

func (a *App) writePeers(cmd *cobra.Command, ids []apimodel.Peer) error {
	ids = sortedByRecency(ids, func(id apimodel.Peer) time.Time { return recencyTime(id.UpdatedAt, id.CreatedAt) })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), ids, func(id apimodel.Peer) string { return id.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"peers": ids})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PEER ID\tNAME\tCREATED")
	for _, id := range ids {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", id.ID, id.Name.Or(""), formatTime(id.CreatedAt))
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
	// A grant with uses came from an agent's request, and the uses are what an
	// operator needs to see to decide whether it should still exist.
	if uses, ok := grant.Uses.Get(); ok {
		for i, use := range uses {
			label := "USES"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(tw, "%s\t%s (%s)\n", label, use.Description, use.UseId.Or(""))
		}
	}
	fmt.Fprintf(tw, "EXPIRES\t%s\n", formatGrantExpiry(grant))
	fmt.Fprintf(tw, "CREATED\t%s\n", formatTime(grant.CreatedAt))
	return tw.Flush()
}

func (a *App) writeSecretGrants(cmd *cobra.Command, grants []apimodel.SecretGrant) error {
	grants = sortedByRecency(grants, func(grant apimodel.SecretGrant) time.Time { return recencyTime(grant.UpdatedAt, grant.CreatedAt) })
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
	// What an agent asked for and why is the whole basis for approving it, so it
	// belongs in the detail view rather than only in the JSON.
	if name, ok := request.Name.Get(); ok && name != "" {
		fmt.Fprintf(tw, "CREDENTIAL\t%s\n", name)
	}
	if envName, ok := request.EnvName.Get(); ok && envName != "" {
		fmt.Fprintf(tw, "ENV VAR\t%s\n", envName)
	}
	if justification, ok := request.Justification.Get(); ok && justification != "" {
		fmt.Fprintf(tw, "WHY\t%s\n", justification)
	}
	if uses, ok := request.Uses.Get(); ok {
		for i, use := range uses {
			label := "USES"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(tw, "%s\t%s\n", label, use.Description)
		}
	}
	if asked := lifetime.FromRequest(request.GrantTTLSeconds.Or(0)); asked > 0 {
		fmt.Fprintf(tw, "WANTED FOR\t%s\n", lifetime.Label(asked))
	}
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
	requests = sortedByRecency(requests, func(request apimodel.SecretRequest) time.Time {
		return recencyTime(request.UpdatedAt, request.CreatedAt)
	})
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), requests, func(request apimodel.SecretRequest) string { return request.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"secretRequests": requests})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tHOST\tSTATUS\tSECRET\tDISCOBOX\tREQUESTED BY\tUPDATED")
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
	harnesses = sortedByRecency(harnesses, func(harness apimodel.HarnessConfig) time.Time {
		return recencyTime(harness.UpdatedAt, harness.CreatedAt)
	})
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), harnesses, func(harness apimodel.HarnessConfig) string { return harness.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"harnessConfigs": harnesses})
	}
	defaultID := ""
	if len(defaultHarnessConfigID) > 0 {
		defaultID = defaultHarnessConfigID[0]
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSLUG\tNAME\tCONFIGURED\tDEFAULT\tRUN COMMAND\tUPDATED")
	for _, harness := range harnesses {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", harness.ID, harness.Slug, harness.Name, formatConfigured(&harness), formatDefaultMarker(harness.ID == defaultID), strings.Join(harness.RunCommand, " "), formatTime(harness.UpdatedAt))
	}
	return tw.Flush()
}

// formatConfigured renders whether a harness has completed its configure flow.
// Only a configured harness can be run, so this is the column that explains why
// `discobox run -H <slug>` is refused. A failed attempt shows its reason.
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
	jobs = sortedByRecency(jobs, func(job apimodel.Job) time.Time { return recencyTime(job.UpdatedAt, job.CreatedAt) })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), jobs, func(job apimodel.Job) string { return job.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"jobs": jobs})
	}
	now := time.Now()
	rows := make([][]string, 0, len(jobs))
	errors := make([]string, 0, len(jobs))
	for _, job := range jobs {
		message := truncateTableValue(job.Message.Or(""), 40)
		rows = append(rows, []string{
			job.ID,
			job.Type,
			string(job.Status),
			strconv.Itoa(job.Attempts),
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

// sortedByRecency orders a listing most-recently-touched first, the default for
// every CLI listing. recency reports the time a value was last touched; use
// recencyTime to fall back to the creation time for resources that do not track
// an update time.
func sortedByRecency[T any](values []T, recency func(T) time.Time) []T {
	out := append([]T(nil), values...)
	sort.SliceStable(out, func(i, j int) bool {
		return recency(out[i]).After(recency(out[j]))
	})
	return out
}

// recencyTime is the time a resource was last touched: its update time, or its
// creation time when the resource has no update time (either unset or not
// tracked at all).
func recencyTime(updatedAt, createdAt time.Time) time.Time {
	if updatedAt.IsZero() {
		return createdAt
	}
	return updatedAt
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
	fmt.Fprintf(tw, "ATTEMPTS\t%d\n", job.Attempts)
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

// formatGrantLimit says a secret's ceiling on grant lifetimes. Zero is not
// absence here — it is the meaningful answer "no limit", so it is spelled out
// rather than left blank the way an unset duration is.
func formatGrantLimit(seconds int64) string {
	if seconds <= 0 {
		return "forever"
	}
	return formatSeconds(seconds)
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

// sandboxNameColumnWidth is how much of a discobox's name every rendering that
// names a discobox to be read prints: the listing, the single-discobox view,
// and the per-discobox header the resources view writes above a process table.
// It is shared with the matching in sandboxListedAs, so a name typed back off
// any of them resolves at exactly the width it was shown at, and widening the
// column cannot quietly stop the longer names it now prints from resolving.
//
// The one rendering left out is the resources table itself (`resources.go`),
// which prints the same field narrower because its other columns are numbers. A
// name cut at that width is not one this resolves — that table leads with the
// discobox ID, which is what it expects to be named from.
const sandboxNameColumnWidth = 40

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
