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
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

type sandboxCreateOptions struct {
	name               string
	description        string
	providerInstanceID string
	sourceURL          string
	sourceRef          string
	workingDirectory   string
	cpuVCPUs           float64
	memoryBytes        int64
	storageBytes       int64
	runtimeState       string
	wait               bool
	waitTimeout        time.Duration
}

type sandboxUpdateOptions sandboxCreateOptions

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
			projectID, err := a.projectUUID()
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
			projectID, err := a.projectUUID()
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

func (a *App) sandboxRequest(sandboxArg string) (projectID, sandboxID uuid.UUID, client *apiclientgen.Client, err error) {
	projectID, err = a.projectUUID()
	if err != nil {
		return projectID, sandboxID, nil, err
	}
	sandboxID, err = parseUUIDArg(sandboxArg, "sandbox ID")
	if err != nil {
		return projectID, sandboxID, nil, err
	}
	client, err = a.apiClient()
	return projectID, sandboxID, client, err
}

func addCreateFlags(cmd *cobra.Command, opts *sandboxCreateOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Sandbox name")
	cmd.Flags().StringVar(&opts.description, "description", "", "Sandbox description")
	cmd.Flags().StringVar(&opts.providerInstanceID, "provider-instance", "", "Sandbox provider instance ID")
	cmd.Flags().StringVar(&opts.sourceURL, "source-url", "", "Source repository or archive URL")
	cmd.Flags().StringVar(&opts.sourceRef, "source-ref", "", "Source branch, tag, or commit")
	cmd.Flags().StringVar(&opts.workingDirectory, "working-directory", "", "Working directory inside the sandbox")
	cmd.Flags().Float64Var(&opts.cpuVCPUs, "cpu-vcpus", 0, "Requested CPU capacity in vCPUs")
	cmd.Flags().Int64Var(&opts.memoryBytes, "memory-bytes", 0, "Requested memory capacity in bytes")
	cmd.Flags().Int64Var(&opts.storageBytes, "storage-bytes", 0, "Requested storage capacity in bytes")
	cmd.Flags().StringVar(&opts.runtimeState, "runtime-state", "", "Initial runtime state as JSON or @path")
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait for sandbox to reach running or fail")
	cmd.Flags().DurationVar(&opts.waitTimeout, "wait-timeout", 2*time.Minute, "Maximum time to wait")
}

func addUpdateFlags(cmd *cobra.Command, opts *sandboxUpdateOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Sandbox name")
	cmd.Flags().StringVar(&opts.description, "description", "", "Sandbox description")
	cmd.Flags().StringVar(&opts.providerInstanceID, "provider-instance", "", "Sandbox provider instance ID")
	cmd.Flags().StringVar(&opts.sourceURL, "source-url", "", "Source repository or archive URL")
	cmd.Flags().StringVar(&opts.sourceRef, "source-ref", "", "Source branch, tag, or commit")
	cmd.Flags().StringVar(&opts.workingDirectory, "working-directory", "", "Working directory inside the sandbox")
	cmd.Flags().Float64Var(&opts.cpuVCPUs, "cpu-vcpus", 0, "Requested CPU capacity in vCPUs")
	cmd.Flags().Int64Var(&opts.memoryBytes, "memory-bytes", 0, "Requested memory capacity in bytes")
	cmd.Flags().Int64Var(&opts.storageBytes, "storage-bytes", 0, "Requested storage capacity in bytes")
	cmd.Flags().StringVar(&opts.runtimeState, "runtime-state", "", "Runtime state as JSON or @path")
}

func createSandboxBody(opts sandboxCreateOptions) (*apiclientgen.CreateSandboxBody, error) {
	body := &apiclientgen.CreateSandboxBody{Name: opts.name}
	body.SetDescription(optString(opts.description))
	body.SetProviderInstanceId(optString(opts.providerInstanceID))
	body.SetSourceRef(optString(opts.sourceRef))
	body.SetWorkingDirectory(optString(opts.workingDirectory))
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
	return body, nil
}

func updateSandboxBody(cmd *cobra.Command, opts sandboxUpdateOptions) (*apiclientgen.UpdateSandboxBody, error) {
	body := &apiclientgen.UpdateSandboxBody{}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	if cmd.Flags().Changed("description") {
		body.SetDescription(apiclientgen.NewOptString(opts.description))
	}
	if cmd.Flags().Changed("provider-instance") {
		body.SetProviderInstanceId(apiclientgen.NewOptString(opts.providerInstanceID))
	}
	if cmd.Flags().Changed("source-ref") {
		body.SetSourceRef(apiclientgen.NewOptString(opts.sourceRef))
	}
	if cmd.Flags().Changed("working-directory") {
		body.SetWorkingDirectory(apiclientgen.NewOptString(opts.workingDirectory))
	}
	if cmd.Flags().Changed("cpu-vcpus") {
		body.SetCpuVcpus(apiclientgen.NewOptFloat64(opts.cpuVCPUs))
	}
	if cmd.Flags().Changed("memory-bytes") {
		body.SetMemoryBytes(apiclientgen.NewOptInt64(opts.memoryBytes))
	}
	if cmd.Flags().Changed("storage-bytes") {
		body.SetStorageBytes(apiclientgen.NewOptInt64(opts.storageBytes))
	}
	if cmd.Flags().Changed("source-url") {
		u, err := url.Parse(opts.sourceURL)
		if err != nil {
			return nil, err
		}
		body.SetSourceUrl(apiclientgen.NewOptURI(*u))
	}
	if cmd.Flags().Changed("runtime-state") {
		state, err := runtimeState(opts.runtimeState)
		if err != nil {
			return nil, err
		}
		body.SetRuntimeState(state)
	}
	return body, nil
}

func (a *App) waitForSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID, sandboxID uuid.UUID, timeout time.Duration) (*apiclientgen.Sandbox, error) {
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
