package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type sandboxCreateOptions struct {
	name                     string
	description              string
	image                    string
	providerInstanceID       string
	agentConfigID            string
	agentName                string
	agentModel               string
	agentModelServiceTier    string
	agentModelReasoningLevel string
	prompt                   string
	sourceURL                string
	sourceRef                string
	sourceRefType            string
	sourceDirectory          string
	workingDirectory         string
	sourceCodeReferences     string
	userName                 string
	userUID                  int64
	userGID                  int64
	homeDirectory            string
	cpuVCPUs                 float64
	memoryBytes              int64
	storageBytes             int64
	wait                     bool
	waitTimeout              time.Duration
}

type sandboxUpdateOptions struct {
	name string
}

func (a *App) newSandboxCommand() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:     "sandbox",
		Aliases: []string{"sandboxes"},
		Short:   "Manage sandboxes",
	}
	cmd.PersistentFlags().StringVar(&sandboxID, "sandbox-id", "", "Sandbox ID for nested sandbox resources")
	cmd.AddCommand(a.newSandboxListCommand())
	cmd.AddCommand(a.newSandboxGetCommand())
	cmd.AddCommand(a.newSandboxCreateCommand())
	cmd.AddCommand(a.newSandboxUpdateCommand())
	cmd.AddCommand(a.newSandboxDeleteCommand())
	cmd.AddCommand(a.newSandboxStartCommand())
	cmd.AddCommand(a.newSandboxStopCommand())
	cmd.AddCommand(a.newSandboxRestartCommand())
	cmd.AddCommand(a.newSandboxTerminalsCommand(&sandboxID))
	return cmd
}

func (a *App) newSandboxListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sandboxes",
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
			return a.writeSandboxes(cmd, body.GetSandboxes())
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSandboxGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get SANDBOX_ID",
		Short: "Get a sandbox",
		Args:  cobra.ExactArgs(1),
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
		Short: "Create a sandbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			if opts.providerInstanceID != "" {
				opts.providerInstanceID, err = a.resolveProviderID(cmd.Context(), client, projectID, opts.providerInstanceID)
				if err != nil {
					return err
				}
			}
			if opts.agentConfigID != "" {
				opts.agentConfigID, err = a.resolveAgentConfigID(cmd.Context(), client, projectID, opts.agentConfigID)
				if err != nil {
					return err
				}
			}
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
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func (a *App) newSandboxUpdateCommand() *cobra.Command {
	var opts sandboxUpdateOptions
	cmd := &cobra.Command{
		Use:   "update SANDBOX_ID",
		Short: "Update a sandbox",
		Args:  cobra.ExactArgs(1),
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
		Use:   "delete SANDBOX_ID...",
		Short: "Delete a sandbox",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			return runDeleteMany(cmd, args, "sandbox", func(arg string) (string, error) {
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

func (a *App) newSandboxStartCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "start SANDBOX_ID",
		Short: "Start a sandbox",
		Args:  cobra.ExactArgs(1),
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
		Use:   "stop SANDBOX_ID",
		Short: "Stop a sandbox",
		Args:  cobra.ExactArgs(1),
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
		Use:   "restart SANDBOX_ID",
		Short: "Restart a sandbox",
		Args:  cobra.ExactArgs(1),
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
	cmd.Flags().StringVar(&opts.name, "name", "", "Sandbox name")
	cmd.Flags().StringVar(&opts.description, "description", "", "Sandbox description")
	cmd.Flags().StringVar(&opts.image, "image", "", "Sandbox base image")
	cmd.Flags().StringVar(&opts.providerInstanceID, "provider-instance", "", "Sandbox provider instance ID")
	cmd.Flags().StringVar(&opts.agentConfigID, "agent-config", "", "Agent config ID")
	cmd.Flags().StringVar(&opts.agentName, "agent", "", "Agent config name to resolve at create time")
	cmd.Flags().StringVar(&opts.agentModel, "agent-model", "", "Model the agent should use")
	cmd.Flags().StringVar(&opts.agentModelServiceTier, "agent-model-service-tier", "", "Model service tier the agent should use")
	cmd.Flags().StringVar(&opts.agentModelReasoningLevel, "agent-model-reasoning-level", "", "Model reasoning level the agent should use")
	cmd.Flags().StringVar(&opts.prompt, "prompt", "", "Prompt the agent should run")
	cmd.Flags().StringVar(&opts.sourceURL, "source-url", "", "Source repository or archive URL")
	cmd.Flags().StringVar(&opts.sourceRef, "source-ref", "", "Source branch, tag, or commit")
	cmd.Flags().StringVar(&opts.sourceRefType, "source-ref-type", "", "Source ref type, such as branch, tag, or commit")
	cmd.Flags().StringVar(&opts.sourceDirectory, "source-directory", "", "Directory where the main source should be placed inside the sandbox")
	cmd.Flags().StringVar(&opts.workingDirectory, "working-directory", "", "Working directory inside the sandbox")
	cmd.Flags().StringVar(&opts.sourceCodeReferences, "source-code-references", "", "Additional source code references JSON or @path")
	cmd.Flags().StringVar(&opts.userName, "user-name", "", "Username to use inside the sandbox")
	cmd.Flags().Int64Var(&opts.userUID, "user-uid", 0, "UID to use inside the sandbox")
	cmd.Flags().Int64Var(&opts.userGID, "user-gid", 0, "GID to use inside the sandbox")
	cmd.Flags().StringVar(&opts.homeDirectory, "home-directory", "", "User home directory to use inside the sandbox")
	cmd.Flags().Float64Var(&opts.cpuVCPUs, "cpu-vcpus", 0, "Requested CPU capacity in vCPUs")
	cmd.Flags().Int64Var(&opts.memoryBytes, "memory-bytes", 0, "Requested memory capacity in bytes")
	cmd.Flags().Int64Var(&opts.storageBytes, "storage-bytes", 0, "Requested storage capacity in bytes")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait for sandbox to reach running or fail")
	cmd.Flags().DurationVar(&opts.waitTimeout, "wait-timeout", 2*time.Minute, "Maximum time to wait")
}

func addUpdateFlags(cmd *cobra.Command, opts *sandboxUpdateOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Sandbox name")
}

func createSandboxBody(opts sandboxCreateOptions) (*apimodel.CreateSandboxBody, error) {
	body := &apimodel.CreateSandboxBody{Name: opts.name}
	body.SetDescription(optString(opts.description))
	body.SetImage(optString(opts.image))
	body.SetProviderInstanceId(optString(opts.providerInstanceID))
	body.SetAgentConfigId(optString(opts.agentConfigID))
	body.SetAgentName(optString(opts.agentName))
	body.SetAgentModel(optString(opts.agentModel))
	body.SetAgentModelServiceTier(optString(opts.agentModelServiceTier))
	body.SetAgentModelReasoningLevel(optString(opts.agentModelReasoningLevel))
	body.SetPrompt(optString(opts.prompt))
	source, err := gitSourceFromCreateOptions(opts)
	if err != nil {
		return nil, err
	}
	if source != nil {
		body.SetSource(apiclientgen.NewOptGitSource(*source))
	}
	if opts.cpuVCPUs > 0 {
		body.SetCpuVcpus(apiclientgen.NewOptFloat64(opts.cpuVCPUs))
	}
	if opts.memoryBytes > 0 {
		body.SetMemoryBytes(apiclientgen.NewOptInt64(opts.memoryBytes))
	}
	if opts.storageBytes > 0 {
		body.SetStorageBytes(apiclientgen.NewOptInt64(opts.storageBytes))
	}
	sourceCodeReferences, err := sourceCodeReferences(opts.sourceCodeReferences)
	if err != nil {
		return nil, err
	}
	if sourceCodeReferences != nil {
		body.SetSourceCodeReferences(apiclientgen.NewOptCreateSandboxBodySourceCodeReferences(sourceCodeReferences))
	}
	if user, ok := sandboxUserFromCreateOptions(opts); ok {
		body.SetUser(apiclientgen.NewOptSandboxUser(user))
	}
	return body, nil
}

func sandboxUserFromCreateOptions(opts sandboxCreateOptions) (apimodel.SandboxUser, bool) {
	user := apimodel.SandboxUser{}
	user.SetName(optString(opts.userName))
	user.SetHomeDirectory(optString(opts.homeDirectory))
	if opts.userUID > 0 {
		user.SetUID(apiclientgen.NewOptInt64(opts.userUID))
	}
	if opts.userGID > 0 {
		user.SetGid(apiclientgen.NewOptInt64(opts.userGID))
	}
	return user, user.Name.Set || user.HomeDirectory.Set || user.UID.Set || user.Gid.Set
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
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	return body, nil
}

func (a *App) waitForSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID string, sandboxID string, timeout time.Duration) (*apimodel.Sandbox, error) {
	ctx := cmd.Context()
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
		if sandbox.Phase == "running" && sandbox.LastOperationStatus == "success" {
			return sandbox, nil
		}
		if sandbox.Phase == "failed" || sandbox.LastOperationStatus == "failed" {
			return sandbox, fmt.Errorf("sandbox failed: phase=%s lastOperationStatus=%s", sandbox.Phase, sandbox.LastOperationStatus)
		}
		select {
		case <-ctx.Done():
			return sandbox, ctx.Err()
		case <-ticker.C:
		}
	}
}

func sourceCodeReferences(value string) (apiclientgen.CreateSandboxBodySourceCodeReferences, error) {
	raw, err := rawJSON(value)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	var refs apiclientgen.CreateSandboxBodySourceCodeReferences
	if err := json.Unmarshal(raw, &refs); err != nil {
		return nil, fmt.Errorf("source code references must be valid JSON: %w", err)
	}
	return refs, nil
}
