package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type agentCreateOptions struct {
	name           string
	definitionID   string
	installCommand string
	runCommand     string
	capabilities   string
}

type agentUpdateOptions struct {
	name           string
	installCommand string
	runCommand     string
	capabilities   string
}

func (a *App) newAgentCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "agents", Aliases: []string{"agent"}, Short: "Manage agent configs"}
	cmd.AddCommand(a.newAgentDefinitionsCommand())
	cmd.AddCommand(a.newAgentListCommand())
	cmd.AddCommand(a.newAgentGetCommand())
	cmd.AddCommand(a.newAgentCreateCommand())
	cmd.AddCommand(a.newAgentUpdateCommand())
	cmd.AddCommand(a.newAgentDeleteCommand())
	return cmd
}

func (a *App) newAgentDefinitionsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "definitions", Aliases: []string{"definition", "defs"}, Short: "List agent config definitions", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			definitionID, err := a.resolveAgentDefinitionID(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			definitionRes, err := client.GetAgentConfigDefinition(cmd.Context(), apiclientgen.GetAgentConfigDefinitionParams{DefinitionId: definitionID})
			if err != nil {
				return err
			}
			definition, err := expectResponse[apimodel.AgentConfigDefinition](definitionRes)
			if err != nil {
				return err
			}
			return a.writeAgentDefinition(cmd, definition)
		}
		bodyRes, err := client.ListAgentConfigDefinitions(cmd.Context())
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListAgentConfigDefinitionsBody](bodyRes)
		if err != nil {
			return err
		}
		return a.writeAgentDefinitions(cmd, body.GetAgentConfigDefinitions())
	}}
	cmd.Args = cobra.MaximumNArgs(1)
	return cmd
}

func (a *App) newAgentListCommand() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List agent configs", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		bodyRes, err := client.ListAgentConfigs(cmd.Context(), apiclientgen.ListAgentConfigsParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListAgentConfigsBody](bodyRes)
		if err != nil {
			return err
		}
		return a.writeAgents(cmd, body.GetAgentConfigs())
	}}
}

func (a *App) newAgentGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get AGENT_CONFIG_ID", Short: "Get an agent config", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		projectID, agentID, client, err := a.agentRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		agentRes, err := client.GetAgentConfig(cmd.Context(), apiclientgen.GetAgentConfigParams{ProjectId: projectID, AgentConfigId: agentID})
		if err != nil {
			return err
		}
		agent, err := expectResponse[apimodel.AgentConfig](agentRes)
		if err != nil {
			return err
		}
		return a.writeAgent(cmd, agent)
	}}
}

func (a *App) newAgentCreateCommand() *cobra.Command {
	var opts agentCreateOptions
	cmd := &cobra.Command{Use: "create", Short: "Create an agent config", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		if opts.definitionID != "" {
			opts.definitionID, err = a.resolveAgentDefinitionID(cmd.Context(), client, opts.definitionID)
			if err != nil {
				return err
			}
		}
		body, err := createAgentBody(opts)
		if err != nil {
			return err
		}
		agentRes, err := client.CreateAgentConfig(cmd.Context(), body, apiclientgen.CreateAgentConfigParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		agent, err := expectResponse[apimodel.AgentConfig](agentRes)
		if err != nil {
			return err
		}
		return a.writeAgent(cmd, agent)
	}}
	cmd.Flags().StringVar(&opts.name, "name", "", "Agent config name")
	cmd.Flags().StringVar(&opts.definitionID, "definition", "", "Agent config definition ID to use as defaults")
	cmd.Flags().StringVar(&opts.installCommand, "install-command", "", "Command used to install the agent")
	cmd.Flags().StringVar(&opts.runCommand, "run-command", "", "Command used to run the agent")
	cmd.Flags().StringVar(&opts.capabilities, "capabilities", "", "Agent capabilities JSON or @path")
	return cmd
}

func (a *App) newAgentUpdateCommand() *cobra.Command {
	var opts agentUpdateOptions
	cmd := &cobra.Command{Use: "update AGENT_CONFIG_ID", Short: "Update an agent config", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		projectID, agentID, client, err := a.agentRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		body, err := updateAgentBody(cmd, opts)
		if err != nil {
			return err
		}
		agentRes, err := client.UpdateAgentConfig(cmd.Context(), body, apiclientgen.UpdateAgentConfigParams{ProjectId: projectID, AgentConfigId: agentID})
		if err != nil {
			return err
		}
		agent, err := expectResponse[apimodel.AgentConfig](agentRes)
		if err != nil {
			return err
		}
		return a.writeAgent(cmd, agent)
	}}
	cmd.Flags().StringVar(&opts.name, "name", "", "Agent config name")
	cmd.Flags().StringVar(&opts.installCommand, "install-command", "", "Command used to install the agent")
	cmd.Flags().StringVar(&opts.runCommand, "run-command", "", "Command used to run the agent")
	cmd.Flags().StringVar(&opts.capabilities, "capabilities", "", "Agent capabilities JSON or @path")
	return cmd
}

func (a *App) newAgentDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete AGENT_CONFIG_ID", Short: "Delete an agent config", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		projectID, agentID, client, err := a.agentRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.DeleteAgentConfig(cmd.Context(), apiclientgen.DeleteAgentConfigParams{ProjectId: projectID, AgentConfigId: agentID})
		if err != nil {
			return err
		}
		if err := expectNoContent[apiclientgen.DeleteAgentConfigNoContent](res); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "deleted")
		return nil
	}}
}

func (a *App) agentRequest(ctx context.Context, agentArg string) (projectID string, agentID string, client *apiclientgen.Client, err error) {
	projectID, err = a.projectIDValue()
	if err != nil {
		return projectID, agentID, nil, err
	}
	client, err = a.apiClient()
	if err != nil {
		return projectID, agentID, nil, err
	}
	agentID, err = a.resolveAgentConfigID(ctx, client, projectID, agentArg)
	return projectID, agentID, client, err
}

func createAgentBody(opts agentCreateOptions) (*apimodel.CreateAgentConfigBody, error) {
	body := &apimodel.CreateAgentConfigBody{}
	body.SetName(optString(opts.name))
	body.SetDefinitionId(optString(opts.definitionID))
	body.SetInstallCommand(optString(opts.installCommand))
	body.SetRunCommand(optString(opts.runCommand))
	capabilities, err := rawJSON(opts.capabilities)
	if err != nil {
		return nil, err
	}
	body.SetCapabilities(capabilities)
	return body, nil
}

func updateAgentBody(cmd *cobra.Command, opts agentUpdateOptions) (*apimodel.UpdateAgentConfigBody, error) {
	body := &apimodel.UpdateAgentConfigBody{}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	if cmd.Flags().Changed("install-command") {
		body.SetInstallCommand(apiclientgen.NewOptString(opts.installCommand))
	}
	if cmd.Flags().Changed("run-command") {
		body.SetRunCommand(apiclientgen.NewOptString(opts.runCommand))
	}
	if cmd.Flags().Changed("capabilities") {
		capabilities, err := rawJSON(opts.capabilities)
		if err != nil {
			return nil, err
		}
		body.SetCapabilities(capabilities)
	}
	return body, nil
}
