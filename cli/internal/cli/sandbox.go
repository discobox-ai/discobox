package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

type sandboxCreateOptions struct {
	name                 string
	description          string
	image                string
	poolID               string
	harnessConfigID      string
	harnessName          string
	model                string
	modelServiceTier     string
	modelReasoningLevel  string
	prompt               []string
	env                  []string
	secret               []string
	sourceURL            string
	sourceRef            string
	sourceRefType        string
	sourceDirectory      string
	workingDirectory     string
	sourceCodeReferences string
	userName             string
	userUID              int64
	userGroups           []string
	// userUIDSet records whether the flag was given at all. 0 is a
	// meaningful uid/gid (root), so "was it set" cannot be inferred from the
	// value: gating on `> 0` silently drops an explicit `--user-uid 0` and
	// makes it indistinguishable from omitting the flag.
	userUIDSet    bool
	homeDirectory string
	gitUserName   string
	gitUserEmail  string
	wait          bool
	waitTimeout   time.Duration
}

type sandboxUpdateOptions struct {
	name string
}

func (a *App) newSandboxCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "discobox",
		Aliases: []string{"discoboxes"},
		Short:   "Manage discoboxes",
	}
	cmd.AddCommand(a.newSandboxListCommand())
	cmd.AddCommand(a.newSandboxGetCommand())
	cmd.AddCommand(a.newSandboxCreateCommand())
	cmd.AddCommand(a.newSandboxUpdateCommand())
	cmd.AddCommand(a.newSandboxDeleteCommand())
	cmd.AddCommand(a.newSandboxUnarchiveCommand())
	cmd.AddCommand(a.newSandboxPurgeCommand())
	cmd.AddCommand(a.newSandboxStartCommand())
	cmd.AddCommand(a.newSandboxStopCommand())
	cmd.AddCommand(a.newSandboxRestartCommand())
	cmd.AddCommand(a.newSandboxRepairCommand())
	cmd.AddCommand(a.newSandboxUpgradeCommand())
	return cmd
}

func (a *App) newSandboxListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List discoboxes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			bodyRes, err := client.ListSandboxes(cmd.Context(), apiclientgen.ListSandboxesParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListSandboxesBody](bodyRes)
			if err != nil {
				return err
			}
			return a.writeSandboxes(cmd, body.GetSandboxes(), true)
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSandboxGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "get DISCOBOX_ID",
		Short:             "Get a discobox",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			sandboxRes, err := client.GetSandbox(cmd.Context(), apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
}

func (a *App) newSandboxCreateCommand() *cobra.Command {
	var opts sandboxCreateOptions
	cmd := &cobra.Command{
		Use:   "create --name NAME",
		Short: "Create a discobox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			if opts.poolID != "" {
				opts.poolID, err = a.resolvePoolID(cmd.Context(), client, projectID, opts.poolID)
				if err != nil {
					return err
				}
			}
			if opts.harnessConfigID != "" {
				opts.harnessConfigID, err = a.resolveHarnessConfigID(cmd.Context(), client, projectID, opts.harnessConfigID)
				if err != nil {
					return err
				}
			}
			opts.userUIDSet = cmd.Flags().Changed("user-uid")
			body, err := createSandboxBody(opts)
			if err != nil {
				return err
			}
			sandboxRes, err := client.CreateSandbox(cmd.Context(), body, apiclientgen.CreateSandboxParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			if opts.wait {
				sandbox, err = a.waitForSandbox(cmd, client, projectID, sandbox.ID, opts.waitTimeout)
				if err != nil {
					return err
				}
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
	addCreateFlags(cmd, &opts)
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	_ = cmd.RegisterFlagCompletionFunc("harness-config", a.completeHarnessConfigs)
	_ = cmd.RegisterFlagCompletionFunc("harness", a.completeHarnessConfigNames)
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func (a *App) newSandboxUpdateCommand() *cobra.Command {
	var opts sandboxUpdateOptions
	cmd := &cobra.Command{
		Use:               "update DISCOBOX_ID",
		Short:             "Update a discobox",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			body, err := updateSandboxBody(cmd, opts)
			if err != nil {
				return err
			}
			sandboxRes, err := client.UpdateSandbox(cmd.Context(), body, apiclientgen.UpdateSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
	addUpdateFlags(cmd, &opts)
	return cmd
}

func (a *App) newSandboxDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete DISCOBOX_ID...",
		Short: "Archive a discobox",
		Long: `Archive discoboxes.

The container and its runtime resources are removed; the discobox's workspace,
config, and secrets are kept, so "discobox unarchive" brings it back with its
work intact. Archived discoboxes are purged automatically once the project's
archive retention runs out (24h by default; see "project update
--archive-retention").

To destroy a discobox and its data now, use "discobox purge".`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			return runActionMany(cmd, args, "discobox", "archived", func(arg string) (string, error) {
				sandboxID, err := a.resolveSandboxID(cmd.Context(), client, projectID, arg)
				if err != nil {
					return "", err
				}
				res, err := client.DeleteSandbox(cmd.Context(), apiclientgen.DeleteSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
				if err != nil {
					return "", err
				}
				if err := expectNoContent[apiclientgen.DeleteSandboxAccepted](res); err != nil {
					return "", err
				}
				return sandboxID, nil
			})
		},
	}
}

func (a *App) newSandboxUnarchiveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unarchive DISCOBOX_ID...",
		Short: "Restore an archived discobox",
		Long: `Restore archived discoboxes.

The container is recreated against the data that was kept and left stopped; it
starts on first use, the same as any other stopped discobox.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			return runActionMany(cmd, args, "discobox", "unarchived", func(arg string) (string, error) {
				sandboxID, err := a.resolveSandboxID(cmd.Context(), client, projectID, arg)
				if err != nil {
					return "", err
				}
				res, err := client.UnarchiveSandbox(cmd.Context(), apiclientgen.UnarchiveSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
				if err != nil {
					return "", err
				}
				if err := expectNoContent[apiclientgen.UnarchiveSandboxAccepted](res); err != nil {
					return "", err
				}
				return sandboxID, nil
			})
		},
	}
}

func (a *App) newSandboxPurgeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "purge DISCOBOX_ID...",
		Short: "Destroy a discobox and its data",
		Long: `Destroy discoboxes and their data.

This is not recoverable: the workspace, config, and secrets are removed from
the pool host along with the container. Unlike the other lifecycle commands it
waits for the pool agent to confirm the removal, so when it returns the data is
actually gone.

To keep the data, use "discobox delete", which archives instead.`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			return runActionMany(cmd, args, "discobox", "purged", func(arg string) (string, error) {
				sandboxID, err := a.resolveSandboxID(cmd.Context(), client, projectID, arg)
				if err != nil {
					return "", err
				}
				res, err := client.PurgeSandbox(cmd.Context(), apiclientgen.PurgeSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
				if err != nil {
					return "", err
				}
				if err := expectNoContent[apiclientgen.PurgeSandboxNoContent](res); err != nil {
					return "", err
				}
				return sandboxID, nil
			})
		},
	}
}

func (a *App) newSandboxStartCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:               "start DISCOBOX_ID",
		Short:             "Start a discobox",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			body := &apimodel.StartSandboxBody{}
			if force {
				body.SetForce(apiclientgen.NewOptBool(true))
			}
			sandboxRes, err := client.StartSandbox(cmd.Context(), body, apiclientgen.StartSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Force start if supported")
	return cmd
}

func (a *App) newSandboxStopCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:               "stop DISCOBOX_ID",
		Short:             "Stop a discobox",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			body := &apimodel.StopSandboxBody{}
			if force {
				body.SetForce(apiclientgen.NewOptBool(true))
			}
			sandboxRes, err := client.StopSandbox(cmd.Context(), body, apiclientgen.StopSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Force stop if supported")
	return cmd
}

func (a *App) newSandboxRestartCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:               "restart DISCOBOX_ID",
		Short:             "Restart a discobox",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			body := &apimodel.RestartSandboxBody{}
			if force {
				body.SetForce(apiclientgen.NewOptBool(true))
			}
			sandboxRes, err := client.RestartSandbox(cmd.Context(), body, apiclientgen.RestartSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Force restart if supported")
	return cmd
}

func (a *App) newSandboxRepairCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "repair DISCOBOX_ID",
		Short: "Repair a discobox by rebuilding it on the current image",
		Long: `Repair a discobox by rebuilding it in place.

The container and its disposable runtime state are torn down, the container is
recreated against the workspace data that was kept, and the discobox is started
- archive, unarchive, and start as one operation. Use this to recover a
discobox stuck in an error state; the workspace and its uncommitted changes
survive.

The rebuild always lands on the image its harness config resolves to now, so a
repair is an upgrade as well: the teardown has already discarded everything a
re-pin costs, and a stale image is itself a way for a discobox to be wedged.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			sandboxRes, err := client.RepairSandbox(cmd.Context(), apiclientgen.RepairSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
}

func (a *App) newSandboxUpgradeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade DISCOBOX_ID",
		Short: "Upgrade a discobox to its harness config's current image",
		Long: "Rebuild the discobox on the image its harness config resolves to now.\n\n" +
			"The discobox keeps its ID, its workspace and sources, its caches, and its\n" +
			"secrets. Anything written elsewhere in the container's filesystem — packages\n" +
			"installed by hand, files outside the workspace — is lost, and the running\n" +
			"harness process is stopped.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			sandboxRes, err := client.UpgradeSandbox(cmd.Context(), &apimodel.UpgradeSandboxBody{}, apiclientgen.UpgradeSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
}

func (a *App) sandboxRequest(ctx context.Context, sandboxArg string) (projectID string, sandboxID string, client *apiclientgen.Client, err error) {
	projectID, err = a.projectIDValue()
	if err != nil {
		return projectID, sandboxID, nil, err
	}
	client, err = a.apiClient()
	if err != nil {
		return projectID, sandboxID, nil, err
	}
	sandboxID, err = a.resolveSandboxID(ctx, client, projectID, sandboxArg)
	return projectID, sandboxID, client, err
}

func addCreateFlags(cmd *cobra.Command, opts *sandboxCreateOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Discobox name")
	cmd.Flags().StringVar(&opts.description, "description", "", "Discobox description")
	cmd.Flags().StringVar(&opts.image, "image", "", "Discobox base image")
	cmd.Flags().StringVar(&opts.poolID, "pool", "", "Pool to schedule the discobox into")
	cmd.Flags().StringVar(&opts.harnessConfigID, "harness-config", "", "Harness config ID")
	cmd.Flags().StringVar(&opts.harnessName, "harness", "", "Harness config name to resolve at create time")
	cmd.Flags().StringVar(&opts.model, "model", "", "Model the harness should use")
	cmd.Flags().StringVar(&opts.modelServiceTier, "model-service-tier", "", "Model service tier the harness should use")
	cmd.Flags().StringVar(&opts.modelReasoningLevel, "model-reasoning-level", "", "Model reasoning level the harness should use")
	cmd.Flags().StringArrayVar(&opts.prompt, "prompt", nil, "Prompt argument the harness should run; repeat to pass multiple argv tokens, preserving the caller's exact tokens")
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables. A KEY whose name contains KEY, TOKEN, PASS, or SECRET is treated as a secret; use KEY!=VALUE to force it to be a plain environment variable")
	cmd.Flags().StringArrayVarP(&opts.secret, "secret", "s", nil, "Secret injected as a sentinel placeholder resolved by the proxy at runtime, as KEY=VALUE (inline value) or KEY=<SECRET_ID> (reference an existing secret); repeat for multiple secrets")
	cmd.Flags().StringVar(&opts.sourceURL, "source-url", "", "Source repository or archive URL")
	cmd.Flags().StringVar(&opts.sourceRef, "source-ref", "", "Source branch, tag, or commit")
	cmd.Flags().StringVar(&opts.sourceRefType, "source-ref-type", "", "Source ref type, such as branch, tag, or commit")
	cmd.Flags().StringVar(&opts.sourceDirectory, "source-directory", "", "Directory where the main source should be placed inside the discobox")
	cmd.Flags().StringVar(&opts.workingDirectory, "working-directory", "", "Working directory inside the discobox")
	cmd.Flags().StringVar(&opts.sourceCodeReferences, "source-code-references", "", "Additional source code references JSON or @path")
	cmd.Flags().StringVar(&opts.userName, "user-name", "", "Username to use inside the discobox")
	cmd.Flags().Int64Var(&opts.userUID, "user-uid", 0, "UID to use inside the discobox")
	cmd.Flags().StringSliceVar(&opts.userGroups, "user-group", nil, "Groups for the discobox user, each a name or a numeric GID. The first is the primary group and the rest are supplementary; omit to use the image's own groups")
	cmd.Flags().StringVar(&opts.homeDirectory, "home-directory", "", "User home directory to use inside the discobox")
	cmd.Flags().StringVar(&opts.gitUserName, "git-user-name", "", "Value for git's user.name inside the discobox. Unlike `discobox run`, this command infers nothing from the local environment")
	cmd.Flags().StringVar(&opts.gitUserEmail, "git-user-email", "", "Value for git's user.email inside the discobox")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait for discobox to reach running or fail")
	cmd.Flags().DurationVar(&opts.waitTimeout, "wait-timeout", 2*time.Minute, "Maximum time to wait")
}

func addUpdateFlags(cmd *cobra.Command, opts *sandboxUpdateOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Discobox name")
}

func createSandboxBody(opts sandboxCreateOptions) (*apimodel.CreateSandboxBody, error) {
	body := &apimodel.CreateSandboxBody{Config: apimodel.SandboxCreateConfig{Name: opts.name}}
	config := &body.Config
	config.SetDescription(optString(opts.description))
	config.SetImage(optString(opts.image))
	body.SetPoolId(optString(opts.poolID))
	config.SetHarnessConfigId(optString(opts.harnessConfigID))
	body.SetHarnessName(optString(opts.harnessName))
	config.SetModel(optString(opts.model))
	config.SetModelServiceTier(optString(opts.modelServiceTier))
	config.SetModelReasoningLevel(optString(opts.modelReasoningLevel))
	if len(opts.prompt) > 0 {
		config.SetPrompt(append([]string(nil), opts.prompt...))
	}
	env, secrets, err := sandboxcreate.EnvAndSecretsFromOptions(opts.env, opts.secret)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		config.SetEnv(apiclientgen.NewOptSandboxCreateConfigEnv(apiclientgen.SandboxCreateConfigEnv(env)))
	}
	if len(secrets) > 0 {
		config.SetSecrets(secrets)
	}
	source, err := gitSourceFromCreateOptions(opts)
	if err != nil {
		return nil, err
	}
	if source != nil {
		config.SetSource(apiclientgen.NewOptGitSource(*source))
	}
	sourceCodeReferences, err := sourceCodeReferences(opts.sourceCodeReferences)
	if err != nil {
		return nil, err
	}
	if sourceCodeReferences != nil {
		config.SetSourceCodeReferences(apiclientgen.NewOptSandboxCreateConfigSourceCodeReferences(sourceCodeReferences))
	}
	if user, ok := sandboxUserFromCreateOptions(opts); ok {
		config.SetUser(apiclientgen.NewOptSandboxUser(user))
	}
	if git, ok := sandboxGitFromCreateOptions(opts); ok {
		config.SetGit(apiclientgen.NewOptSandboxGitIdentity(git))
	}
	return body, nil
}

// sandboxGitFromCreateOptions builds the git authorship from explicit flags
// only. This command is the flag-driven path, so unlike `discobox run` it reads
// nothing from the local git config: what it creates is what was asked for.
func sandboxGitFromCreateOptions(opts sandboxCreateOptions) (apimodel.SandboxGitIdentity, bool) {
	git := apimodel.SandboxGitIdentity{}
	git.SetUserName(optString(opts.gitUserName))
	git.SetUserEmail(optString(opts.gitUserEmail))
	return git, git.UserName.Set || git.UserEmail.Set
}

func sandboxUserFromCreateOptions(opts sandboxCreateOptions) (apimodel.SandboxUser, bool) {
	user := apimodel.SandboxUser{}
	user.SetName(optString(opts.userName))
	user.SetHomeDirectory(optString(opts.homeDirectory))
	if opts.userUIDSet {
		user.SetUID(apiclientgen.NewOptInt64(opts.userUID))
	}
	applySandboxUserGroups(&user, opts.userGroups)
	return user, user.Name.Set || user.HomeDirectory.Set || user.UID.Set || user.Gid.Set ||
		user.GroupName.Set || len(user.AdditionalGroups) > 0
}

// applySandboxUserGroups splits --user-group into the primary group and the
// supplementary ones, the same shape `exec create --gid` takes. Entries may be
// names or numeric GIDs; both resolve inside the sandbox (ADR 0025 §3).
func applySandboxUserGroups(user *apimodel.SandboxUser, values []string) {
	groups := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			groups = append(groups, value)
		}
	}
	if len(groups) == 0 {
		return
	}
	if parsed, ok := parseOptionalUserID(groups[0]); ok {
		user.SetGid(apiclientgen.NewOptInt64(parsed))
	} else {
		user.SetGroupName(apiclientgen.NewOptString(groups[0]))
	}
	if len(groups) > 1 {
		user.SetAdditionalGroups(append([]string(nil), groups[1:]...))
	}
}

func gitSourceFromCreateOptions(opts sandboxCreateOptions) (*apimodel.GitSource, error) {
	if opts.sourceURL == "" && opts.sourceRef == "" && opts.sourceRefType == "" && opts.sourceDirectory == "" && opts.workingDirectory == "" {
		return nil, nil
	}
	source := &apimodel.GitSource{Kind: apiclientgen.GitSourceKindGit}
	if opts.sourceURL != "" {
		u, err := url.Parse(opts.sourceURL)
		if err != nil {
			return nil, err
		}
		source.SetURL(apiclientgen.NewOptURI(*u))
	}
	if opts.sourceRef != "" || opts.sourceRefType != "" {
		checkout := apimodel.GitSourceCheckout{}
		if opts.sourceRefType == "commit" {
			checkout.SetCommit(optString(opts.sourceRef))
		} else {
			checkout.SetRefName(optString(opts.sourceRef))
		}
		checkout.SetRefType(optString(opts.sourceRefType))
		source.SetCheckout(apiclientgen.NewOptGitSourceCheckout(checkout))
	}
	if opts.sourceDirectory != "" || opts.workingDirectory != "" {
		destination := apimodel.GitSourceDestination{}
		destination.SetDirectory(optString(opts.sourceDirectory))
		destination.SetWorkingDirectory(optString(opts.workingDirectory))
		source.SetDestination(apiclientgen.NewOptGitSourceDestination(destination))
	}
	return source, nil
}

func updateSandboxBody(cmd *cobra.Command, opts sandboxUpdateOptions) (*apimodel.UpdateSandboxBody, error) {
	body := &apimodel.UpdateSandboxBody{}
	if cmd.Flags().Changed("name") {
		body.SetConfig(apiclientgen.NewOptSandboxUpdateConfig(apimodel.SandboxUpdateConfig{
			Name: apiclientgen.NewOptString(opts.name),
		}))
	}
	return body, nil
}

func (a *App) waitForSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID string, sandboxID string, timeout time.Duration) (*apimodel.Sandbox, error) {
	return a.waitForSandboxCtx(cmd.Context(), client, projectID, sandboxID, timeout)
}

func (a *App) waitForSandboxCtx(ctx context.Context, client *apiclientgen.Client, projectID string, sandboxID string, timeout time.Duration) (*apimodel.Sandbox, error) {
	if timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		sandboxRes, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		if err != nil {
			return nil, err
		}
		sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
		if err != nil {
			return nil, err
		}
		// displayState is the single vocabulary the server exposes for this
		// (ADR 0017 §7); reading raw state plus generations here would be
		// re-deriving what it already computed.
		switch sandbox.Runtime.DisplayState.Or("") {
		case "running":
			return sandbox, nil
		case "error":
			return sandbox, fmt.Errorf("discobox failed: %s", sandboxFailureReason(sandbox))
		}
		select {
		case <-ctx.Done():
			return sandbox, ctx.Err()
		case <-ticker.C:
		}
	}
}

// sandboxFailureReason reports why a sandbox failed, preferring the message the
// server recorded on it. The state alone is tautological ("it failed because it
// failed"), so it is only the fallback when no message is set.
func sandboxFailureReason(sandbox *apimodel.Sandbox) string {
	if message, ok := sandbox.Runtime.ErrorMessage.Get(); ok && strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return fmt.Sprintf("state=%s", sandbox.Runtime.State)
}

func sourceCodeReferences(value string) (apiclientgen.SandboxCreateConfigSourceCodeReferences, error) {
	raw, err := rawJSON(value)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var refs apiclientgen.SandboxCreateConfigSourceCodeReferences
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil, fmt.Errorf("source code references must be valid JSON: %w", err)
	}
	return refs, nil
}
