package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

type sandboxCreateOptions struct {
	name                     string
	description              string
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
	userUID                  int64
	userGID                  int64
	cpuVCPUs                 float64
	memoryBytes              int64
	storageBytes             int64
	runtimeState             string
	wait                     bool
	waitTimeout              time.Duration
}

type sandboxUpdateOptions struct {
	name string
}

func (a *App) newSandboxCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sandbox",
		Aliases: []string{"sandboxes"},
		Short:   "Manage sandboxes",
	}
	cmd.AddCommand(a.newSandboxListCommand())
	cmd.AddCommand(a.newSandboxGetCommand())
	cmd.AddCommand(a.newSandboxCreateCommand())
	cmd.AddCommand(a.newSandboxUpdateCommand())
	cmd.AddCommand(a.newSandboxDeleteCommand())
	cmd.AddCommand(a.newSandboxStartCommand())
	cmd.AddCommand(a.newSandboxStopCommand())
	cmd.AddCommand(a.newSandboxRestartCommand())
	return cmd
}

func (a *App) newSandboxListCommand() *cobra.Command {
	return &cobra.Command{
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
			body, err := client.ListSandboxes(cmd.Context(), apiclientgen.ListSandboxesParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			return a.writeSandboxes(cmd, body.GetSandboxes())
		},
	}
}

func (a *App) newSandboxGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get SANDBOX_ID",
		Short: "Get a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(args[0])
			if err != nil {
				return err
			}
			sandbox, err := client.GetSandbox(cmd.Context(), apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
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
			body, err := createSandboxBody(opts)
			if err != nil {
				return err
			}
			sandbox, err := client.CreateSandbox(cmd.Context(), body, apiclientgen.CreateSandboxParams{ProjectId: projectID})
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
			projectID, sandboxID, client, err := a.sandboxRequest(args[0])
			if err != nil {
				return err
			}
			body, err := updateSandboxBody(cmd, opts)
			if err != nil {
				return err
			}
			sandbox, err := client.UpdateSandbox(cmd.Context(), body, apiclientgen.UpdateSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
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
		Use:   "delete SANDBOX_ID",
		Short: "Delete a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(args[0])
			if err != nil {
				return err
			}
			if err := client.DeleteSandbox(cmd.Context(), apiclientgen.DeleteSandboxParams{ProjectId: projectID, SandboxId: sandboxID}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "deleted")
			return nil
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
			projectID, sandboxID, client, err := a.sandboxRequest(args[0])
			if err != nil {
				return err
			}
			body := &apiclientgen.StartSandboxBody{}
			if force {
				body.SetForce(apiclientgen.NewOptBool(true))
			}
			sandbox, err := client.StartSandbox(cmd.Context(), body, apiclientgen.StartSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
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
			projectID, sandboxID, client, err := a.sandboxRequest(args[0])
			if err != nil {
				return err
			}
			body := &apiclientgen.StopSandboxBody{}
			if force {
				body.SetForce(apiclientgen.NewOptBool(true))
			}
			sandbox, err := client.StopSandbox(cmd.Context(), body, apiclientgen.StopSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
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
			projectID, sandboxID, client, err := a.sandboxRequest(args[0])
			if err != nil {
				return err
			}
			body := &apiclientgen.RestartSandboxBody{}
			if force {
				body.SetForce(apiclientgen.NewOptBool(true))
			}
			sandbox, err := client.RestartSandbox(cmd.Context(), body, apiclientgen.RestartSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Force restart if supported")
	return cmd
}

func (a *App) sandboxRequest(sandboxArg string) (projectID string, sandboxID string, client *apiclientgen.Client, err error) {
	projectID, err = a.projectIDValue()
	if err != nil {
		return projectID, sandboxID, nil, err
	}
	id, err := parseUUIDArg(sandboxArg, "sandbox ID")
	if err != nil {
		return projectID, sandboxID, nil, err
	}
	sandboxID = id.String()
	client, err = a.apiClient()
	return projectID, sandboxID, client, err
}

func addCreateFlags(cmd *cobra.Command, opts *sandboxCreateOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Sandbox name")
	cmd.Flags().StringVar(&opts.description, "description", "", "Sandbox description")
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
	cmd.Flags().Int64Var(&opts.userUID, "user-uid", 0, "UID to use inside the sandbox")
	cmd.Flags().Int64Var(&opts.userGID, "user-gid", 0, "GID to use inside the sandbox")
	cmd.Flags().Float64Var(&opts.cpuVCPUs, "cpu-vcpus", 0, "Requested CPU capacity in vCPUs")
	cmd.Flags().Int64Var(&opts.memoryBytes, "memory-bytes", 0, "Requested memory capacity in bytes")
	cmd.Flags().Int64Var(&opts.storageBytes, "storage-bytes", 0, "Requested storage capacity in bytes")
	cmd.Flags().StringVar(&opts.runtimeState, "runtime-state", "", "Initial runtime state as JSON or @path")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait for sandbox to reach running or fail")
	cmd.Flags().DurationVar(&opts.waitTimeout, "wait-timeout", 2*time.Minute, "Maximum time to wait")
}

func addUpdateFlags(cmd *cobra.Command, opts *sandboxUpdateOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Sandbox name")
}

func createSandboxBody(opts sandboxCreateOptions) (*apiclientgen.CreateSandboxBody, error) {
	body := &apiclientgen.CreateSandboxBody{Name: opts.name}
	body.SetDescription(optString(opts.description))
	body.SetProviderInstanceId(optString(opts.providerInstanceID))
	body.SetAgentConfigId(optString(opts.agentConfigID))
	body.SetAgentName(optString(opts.agentName))
	body.SetAgentModel(optString(opts.agentModel))
	body.SetAgentModelServiceTier(optString(opts.agentModelServiceTier))
	body.SetAgentModelReasoningLevel(optString(opts.agentModelReasoningLevel))
	body.SetPrompt(optString(opts.prompt))
	body.SetSourceRef(optString(opts.sourceRef))
	body.SetSourceRefType(optString(opts.sourceRefType))
	body.SetSourceDirectory(optString(opts.sourceDirectory))
	body.SetWorkingDirectory(optString(opts.workingDirectory))
	if opts.userUID > 0 {
		body.SetUserUid(apiclientgen.NewOptInt64(opts.userUID))
	}
	if opts.userGID > 0 {
		body.SetUserGid(apiclientgen.NewOptInt64(opts.userGID))
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
	if opts.sourceURL != "" {
		u, err := url.Parse(opts.sourceURL)
		if err != nil {
			return nil, err
		}
		body.SetSourceUrl(apiclientgen.NewOptURI(*u))
	}
	state, err := runtimeState(opts.runtimeState)
	if err != nil {
		return nil, err
	}
	body.SetRuntimeState(state)
	sourceCodeReferences, err := sourceCodeReferences(opts.sourceCodeReferences)
	if err != nil {
		return nil, err
	}
	if sourceCodeReferences != nil {
		body.SetSourceCodeReferences(apiclientgen.NewOptCreateSandboxBodySourceCodeReferences(sourceCodeReferences))
	}
	return body, nil
}

func updateSandboxBody(cmd *cobra.Command, opts sandboxUpdateOptions) (*apiclientgen.UpdateSandboxBody, error) {
	body := &apiclientgen.UpdateSandboxBody{}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	return body, nil
}

func (a *App) waitForSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID string, sandboxID string, timeout time.Duration) (*apiclientgen.Sandbox, error) {
	ctx := cmd.Context()
	if timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		sandbox, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
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

func runtimeState(value string) (jx.Raw, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if strings.HasPrefix(value, "@") {
		data, err := os.ReadFile(strings.TrimPrefix(value, "@"))
		if err != nil {
			return nil, err
		}
		value = string(data)
	}
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("runtime state must be valid JSON: %w", err)
	}
	return jx.Raw(raw), nil
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
