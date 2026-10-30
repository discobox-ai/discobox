package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

type poolOptions struct {
	name     string
	provider string
	cpuVCPUs float64
	memory   byteSizeFlag
	storage  byteSizeFlag
	// disableAgentEndpointTLS has the pool reach its sandboxes' agents over
	// plain HTTP rather than TLS (ADR 26-10-29-481 §3).
	disableAgentEndpointTLS bool
}

func (a *App) newPoolCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "pool", Aliases: []string{"pools"}, Short: "Manage discobox pools"}
	cmd.AddCommand(a.newPoolListCommand())
	cmd.AddCommand(a.newPoolGetCommand())
	cmd.AddCommand(a.newPoolCreateCommand())
	cmd.AddCommand(a.newPoolUpdateCommand())
	cmd.AddCommand(a.newPoolSetDefaultCommand())
	cmd.AddCommand(a.newPoolUnsetDefaultCommand())
	cmd.AddCommand(a.newPoolDeleteCommand())
	cmd.AddCommand(a.newPoolClearCacheCommand())
	cmd.AddCommand(a.newPoolResourcesCommand())
	cmd.AddCommand(a.newPoolConsoleCommand())
	cmd.AddCommand(a.newPoolLogsCommand())
	cmd.AddCommand(a.newPoolGuestImageCommand())
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
	return &cobra.Command{Use: "get POOL", Short: "Show a pool", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
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

A pool is the sharing boundary discoboxes are scheduled into, and its own
runtime host: discoboxes in the same pool share a cache volume, the pool's
CPU and memory, and a kernel/host. The pool binds to one provider instance at create
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
		if cmd.Flags().Changed("memory") {
			body.SetMemoryBytes(apiclientgen.NewOptInt64(opts.memory.bytes))
		}
		if cmd.Flags().Changed("storage") {
			body.SetStorageBytes(apiclientgen.NewOptInt64(opts.storage.bytes))
		}
		if cmd.Flags().Changed("disable-agent-endpoint-tls") {
			body.SetDisableAgentEndpointTls(apiclientgen.NewOptBool(opts.disableAgentEndpointTLS))
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
	cmd.Flags().StringVar(&opts.provider, "provider", "", "Backing provider instance, by name or ID (required)")
	_ = cmd.MarkFlagRequired("provider")
	_ = cmd.RegisterFlagCompletionFunc("provider", a.completeProviders)
	addPoolAttributeFlags(cmd, &opts)
	return cmd
}

func (a *App) newPoolUpdateCommand() *cobra.Command {
	var opts poolOptions
	var egressOpts egressFlags
	cmd := &cobra.Command{Use: "update POOL", Short: "Update a pool", Long: `Update a pool.

--egress-allow and --egress-deny-by-default set the pool's egress level, which
every discobox in the pool converges onto, running ones included. A build's own
base-image pulls are not filtered by any level (ADR 26-10-01-032 §7).

` + egressHelp, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
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
		if cmd.Flags().Changed("memory") {
			body.SetMemoryBytes(apiclientgen.NewOptInt64(opts.memory.bytes))
		}
		if cmd.Flags().Changed("storage") {
			body.SetStorageBytes(apiclientgen.NewOptInt64(opts.storage.bytes))
		}
		if cmd.Flags().Changed("disable-agent-endpoint-tls") {
			body.SetDisableAgentEndpointTls(apiclientgen.NewOptBool(opts.disableAgentEndpointTLS))
		}
		level, ok, err := egressOpts.update(cmd, func() (apimodel.EgressPolicy, error) {
			res, err := client.GetPool(cmd.Context(), apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: poolID})
			if err != nil {
				return apimodel.EgressPolicy{}, err
			}
			pool, err := expectResponse[apimodel.Pool](res)
			if err != nil {
				return apimodel.EgressPolicy{}, err
			}
			return pool.Egress.Value, nil
		})
		if err != nil {
			return err
		}
		if ok {
			body.SetEgress(apiclientgen.NewOptEgressPolicy(level))
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
	addEgressUpdateFlags(cmd, &egressOpts, "the pool")
	return cmd
}

func (a *App) newPoolSetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "set-default POOL", Short: "Set the project default pool", Long: `Set the project default pool.

New discoboxes created without an explicit --pool are scheduled into the
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
	return &cobra.Command{Use: "unset-default", Short: "Clear the project default pool", Long: `Clear the project default pool.

Leaves the project with no default pool, so new discoboxes must name a pool
explicitly with --pool. This is also how you release the default before
deleting that pool. With no default set, it changes nothing.`, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		project, err := a.getProject(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		if poolID := project.DefaultPoolId.Or(""); poolID != "" {
			res, err := client.UnsetDefaultPool(cmd.Context(), apiclientgen.UnsetDefaultPoolParams{ProjectId: projectID, PoolId: poolID})
			if err != nil {
				return err
			}
			if project, err = expectResponse[apimodel.Project](res); err != nil {
				return err
			}
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
	cmd.Flags().Float64Var(&opts.cpuVCPUs, "cpu-vcpus", 0, "vCPUs for the pool's VM, on providers that size a VM per pool (0 = provider and host default)")
	cmd.Flags().Var(&opts.memory, "memory", "Memory for the pool's VM, such as 8g or 512m, on providers that size a VM per pool (0 = provider and host default)")
	cmd.Flags().Var(&opts.storage, "storage", "Storage for the pool, such as 100g; no provider accepts it today (0 = unset)")
	cmd.Flags().BoolVar(&opts.disableAgentEndpointTLS, "disable-agent-endpoint-tls", false, "Reach the pool's discoboxes' agents over plain HTTP instead of TLS; a discobox takes a change when its box is next made (a VM discobox at its next start, a container when it is replaced)")
}

func (a *App) newPoolDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "rm POOL...", Aliases: []string{"delete", "remove"}, Short: "Delete pools", Args: cobra.MinimumNArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "pool", "delete", "deleted", func(arg string) (string, error) {
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

func (a *App) newPoolClearCacheCommand() *cobra.Command {
	return &cobra.Command{Use: "clear-cache POOL", Short: "Stop a pool's discoboxes and empty its caches", Long: `Stop a pool's discoboxes and empty its caches.

Every running discobox on the pool is stopped, because the caches serve all of
them, and then the pool empties:

  - the cache its discoboxes share (package manager and tool caches)
  - its docker build cache
  - its registry of images discoboxes pushed to build from
  - its proxy's cache of downloaded packages and image layers
  - discobox images no discobox uses, other than the newest of each and any
    that arrived in the last hour

The command returns once they are empty. Nothing is started again afterwards: a
stopped discobox starts the next time it is used. Images a discobox pushed to
the registry are gone until it pushes them again.

A pool whose discoboxes are virtual machines shares no cache between them, so it
stops nothing: it empties its proxy's cache, and each running discobox empties
its own caches in place. A stopped one keeps its caches.`, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completePools, RunE: func(cmd *cobra.Command, args []string) error {
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
		res, err := client.ClearPoolCache(cmd.Context(), apiclientgen.ClearPoolCacheParams{ProjectId: projectID, PoolId: poolID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ClearPoolCacheBody](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), body)
		}
		out := cmd.OutOrStdout()
		for _, sandboxID := range body.StoppedSandboxIds {
			if _, err := fmt.Fprintf(out, "%s stopped\n", sandboxID); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(out, "caches cleared for pool %s\n", poolID)
		return err
	}}
}
