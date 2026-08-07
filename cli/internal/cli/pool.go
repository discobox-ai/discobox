package cli

import (
	"context"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type poolOptions struct {
	name         string
	provider     string
	cpuVCPUs     float64
	memoryBytes  int64
	storageBytes int64
}

func (a *App) newPoolCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "pool", Aliases: []string{"pools"}, Short: "Manage sandbox pools"}
	cmd.AddCommand(a.newPoolListCommand())
	cmd.AddCommand(a.newPoolGetCommand())
	cmd.AddCommand(a.newPoolCreateCommand())
	cmd.AddCommand(a.newPoolUpdateCommand())
	cmd.AddCommand(a.newPoolSetDefaultCommand())
	cmd.AddCommand(a.newPoolUnsetDefaultCommand())
	cmd.AddCommand(a.newPoolDeleteCommand())
	return cmd
}

func (a *App) newPoolListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List pools", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		bodyRes, err := client.ListPools(cmd.Context(), apiclientgen.ListPoolsParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListPoolsBody](bodyRes)
		if err != nil {
			return err
		}
		defaultPoolID, err := a.defaultPoolID(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		return a.writePools(cmd, body.GetPools(), defaultPoolID)
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newPoolGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get POOL_ID", Short: "Get a pool", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		poolID, err := a.resolvePoolID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		poolRes, err := client.GetPool(cmd.Context(), apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: poolID})
		if err != nil {
			return err
		}
		pool, err := expectResponse[apimodel.Pool](poolRes)
		if err != nil {
			return err
		}
		return a.writePool(cmd, pool)
	}}
}

func (a *App) newPoolCreateCommand() *cobra.Command {
	var opts poolOptions
	cmd := &cobra.Command{Use: "create NAME", Short: "Create a pool", Long: `Create a pool.

A pool is the sharing boundary sandboxes are scheduled into, and its own
runtime host: sandboxes in the same pool share a cache volume, a resource
envelope, and a kernel/host. The pool binds to one provider instance at create
time and cannot be moved.`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		providerID, err := a.resolveProviderID(cmd.Context(), client, projectID, opts.provider)
		if err != nil {
			return err
		}
		body := &apimodel.CreatePoolBody{Name: args[0], ProviderInstanceId: providerID}
		if cmd.Flags().Changed("cpu-vcpus") {
			body.SetCpuVcpus(apiclientgen.NewOptFloat64(opts.cpuVCPUs))
		}
		if cmd.Flags().Changed("memory-bytes") {
			body.SetMemoryBytes(apiclientgen.NewOptInt64(opts.memoryBytes))
		}
		if cmd.Flags().Changed("storage-bytes") {
			body.SetStorageBytes(apiclientgen.NewOptInt64(opts.storageBytes))
		}
		poolRes, err := client.CreatePool(cmd.Context(), body, apiclientgen.CreatePoolParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		pool, err := expectResponse[apimodel.Pool](poolRes)
		if err != nil {
			return err
		}
		return a.writePool(cmd, pool)
	}}
	cmd.Flags().StringVar(&opts.provider, "provider", "", "Backing provider instance ID (required)")
	_ = cmd.MarkFlagRequired("provider")
	_ = cmd.RegisterFlagCompletionFunc("provider", a.completeProviders)
	addPoolAttributeFlags(cmd, &opts)
	return cmd
}

func (a *App) newPoolUpdateCommand() *cobra.Command {
	var opts poolOptions
	cmd := &cobra.Command{Use: "update POOL_ID", Short: "Update a pool", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		poolID, err := a.resolvePoolID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		body := &apimodel.UpdatePoolBody{}
		if cmd.Flags().Changed("name") {
			body.SetName(apiclientgen.NewOptString(opts.name))
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
		poolRes, err := client.UpdatePool(cmd.Context(), body, apiclientgen.UpdatePoolParams{ProjectId: projectID, PoolId: poolID})
		if err != nil {
			return err
		}
		pool, err := expectResponse[apimodel.Pool](poolRes)
		if err != nil {
			return err
		}
		return a.writePool(cmd, pool)
	}}
	cmd.Flags().StringVar(&opts.name, "name", "", "Pool display name")
	addPoolAttributeFlags(cmd, &opts)
	return cmd
}

func (a *App) newPoolSetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "set-default POOL_ID", Short: "Set the project default pool", Long: `Set the project default pool.

New sandboxes created without an explicit --pool are scheduled into the
project's default pool.`, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		poolID, err := a.resolvePoolID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.SetDefaultPool(cmd.Context(), apiclientgen.SetDefaultPoolParams{ProjectId: projectID, PoolId: poolID})
		if err != nil {
			return err
		}
		project, err := expectResponse[apimodel.Project](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), project)
		}
		_, err = cmd.OutOrStdout().Write([]byte("default pool set to " + poolID + "\n"))
		return err
	}}
}

func (a *App) newPoolUnsetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "unset-default POOL_ID", Short: "Clear the project default pool", Long: `Clear the project default pool.

Leaves the project with no default pool, so new sandboxes must name a pool
explicitly with --pool. POOL_ID must be the current default; this is also how
you release the default before deleting that pool.`, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		poolID, err := a.resolvePoolID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.UnsetDefaultPool(cmd.Context(), apiclientgen.UnsetDefaultPoolParams{ProjectId: projectID, PoolId: poolID})
		if err != nil {
			return err
		}
		project, err := expectResponse[apimodel.Project](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), project)
		}
		_, err = cmd.OutOrStdout().Write([]byte("default pool cleared\n"))
		return err
	}}
}

// defaultPoolID returns the project's configured default pool ID, or "" when
// none is set.
func (a *App) defaultPoolID(ctx context.Context, client *apiclientgen.Client, projectID string) (string, error) {
	project, err := a.getProject(ctx, client, projectID)
	if err != nil {
		return "", err
	}
	return project.DefaultPoolId.Or(""), nil
}

func addPoolAttributeFlags(cmd *cobra.Command, opts *poolOptions) {
	cmd.Flags().Float64Var(&opts.cpuVCPUs, "cpu-vcpus", 0, "Total CPU capacity of the pool envelope in vCPUs (0 = host-sized)")
	cmd.Flags().Int64Var(&opts.memoryBytes, "memory-bytes", 0, "Total memory capacity of the pool envelope in bytes (0 = host-sized)")
	cmd.Flags().Int64Var(&opts.storageBytes, "storage-bytes", 0, "Total storage capacity of the pool envelope in bytes (0 = host-sized)")
}

func (a *App) newPoolDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete POOL_ID...", Short: "Delete pools", Args: cobra.MinimumNArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "pool", "deleted", func(arg string) (string, error) {
			poolID, err := a.resolvePoolID(cmd.Context(), client, projectID, arg)
			if err != nil {
				return "", err
			}
			res, err := client.DeletePool(cmd.Context(), apiclientgen.DeletePoolParams{ProjectId: projectID, PoolId: poolID})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeletePoolNoContent](res); err != nil {
				return "", err
			}
			return poolID, nil
		})
	}}
}
