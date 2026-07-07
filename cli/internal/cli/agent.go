package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type agentCreateOptions struct {
	name           string
	definitionID   string
	installCommand []string
	runCommand     []string
	files          []string
	createOnlyFile []string
}

type agentUpdateOptions struct {
	name           string
	installCommand []string
	runCommand     []string
	files          []string
	createOnlyFile []string
}

func (a *App) newAgentCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "agents", Aliases: []string{"agent"}, Short: "Manage agent configs"}
	cmd.AddCommand(a.newAgentDefinitionsCommand())
	cmd.AddCommand(a.newAgentListCommand())
	cmd.AddCommand(a.newAgentGetCommand())
	cmd.AddCommand(a.newAgentCreateCommand())
	cmd.AddCommand(a.newAgentEnableCommand())
	cmd.AddCommand(a.newAgentDisableCommand())
	cmd.AddCommand(a.newAgentUpdateCommand())
	cmd.AddCommand(a.newAgentSetDefaultCommand())
	cmd.AddCommand(a.newAgentDeleteCommand())
	return cmd
}

func (a *App) newAgentDefinitionsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "definitions", Aliases: []string{"definition", "defs"}, Short: "List agent config definitions", ValidArgsFunction: a.completeAgentDefinitions, RunE: func(cmd *cobra.Command, args []string) error {
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
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newAgentListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "List agent configs", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		agents, err := a.listAgentConfigs(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		var defaultAgentConfigID string
		if !a.quiet && a.output != "json" {
			defaultAgentConfigID, err = a.defaultAgentConfigID(cmd.Context(), client, projectID)
			if err != nil {
				return err
			}
		}
		return a.writeAgents(cmd, agents, defaultAgentConfigID)
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newAgentGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get AGENT_CONFIG_ID", Short: "Get an agent config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeAgentConfigs, RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringArrayVar(&opts.installCommand, "install-command", nil, "Argv element used to install the agent (repeatable, e.g. --install-command npm --install-command install). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.runCommand, "run-command", nil, "Argv element used to run the agent (repeatable, e.g. --run-command claude --run-command --dangerously-skip-permissions). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "File to write into the agent's home directory, as PATH=CONTENT or PATH=@LOCALFILE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	_ = cmd.RegisterFlagCompletionFunc("definition", a.completeAgentDefinitions)
	return cmd
}

func (a *App) newAgentEnableCommand() *cobra.Command {
	var setDefault bool
	cmd := &cobra.Command{Use: "enable DEFINITION_NAME", Aliases: []string{"enabled"}, Short: "Enable an agent config definition", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeAgentDefinitions, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		definition, err := a.resolveAgentDefinition(cmd.Context(), client, args[0])
		if err != nil {
			return err
		}
		agents, err := a.listAgentConfigs(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		existing := agentConfigByName(agents, definition.Name)
		if existing != nil {
			if setDefault {
				if err := a.setDefaultAgentConfig(cmd.Context(), client, projectID, existing.ID); err != nil {
					return err
				}
			}
			return a.writeAgent(cmd, existing)
		}
		body := &apimodel.CreateAgentConfigBody{}
		body.SetDefinitionId(apiclientgen.NewOptString(definition.ID))
		agentRes, err := client.CreateAgentConfig(cmd.Context(), body, apiclientgen.CreateAgentConfigParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		agent, err := expectResponse[apimodel.AgentConfig](agentRes)
		if err != nil {
			return err
		}
		if setDefault || len(agents) == 0 {
			if err := a.setDefaultAgentConfig(cmd.Context(), client, projectID, agent.ID); err != nil {
				return err
			}
		}
		return a.writeAgent(cmd, agent)
	}}
	cmd.Flags().BoolVarP(&setDefault, "default", "d", false, "Set the project default agent config")
	return cmd
}

func (a *App) newAgentDisableCommand() *cobra.Command {
	return &cobra.Command{Use: "disable DEFINITION_NAME", Short: "Disable an agent config definition", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeAgentDefinitions, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		definition, err := a.resolveAgentDefinition(cmd.Context(), client, args[0])
		if err != nil {
			return err
		}
		existing, err := a.agentConfigByName(cmd.Context(), client, projectID, definition.Name)
		if err != nil {
			return err
		}
		if existing == nil {
			return nil
		}
		res, err := client.DeleteAgentConfig(cmd.Context(), apiclientgen.DeleteAgentConfigParams{ProjectId: projectID, AgentConfigId: existing.ID})
		if err != nil {
			return err
		}
		if err := expectNoContent[apiclientgen.DeleteAgentConfigNoContent](res); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", existing.ID)
		return err
	}}
}

func (a *App) newAgentUpdateCommand() *cobra.Command {
	var opts agentUpdateOptions
	cmd := &cobra.Command{Use: "update AGENT_CONFIG_ID", Short: "Update an agent config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeAgentConfigs, RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringArrayVar(&opts.installCommand, "install-command", nil, "Argv element used to install the agent (repeatable, e.g. --install-command npm --install-command install). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.runCommand, "run-command", nil, "Argv element used to run the agent (repeatable, e.g. --run-command claude --run-command --dangerously-skip-permissions). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "File to write into the agent's home directory, as PATH=CONTENT or PATH=@LOCALFILE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	return cmd
}

func (a *App) newAgentSetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "set-default AGENT_CONFIG_ID", Short: "Set the project default agent config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeAgentConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, agentID, client, err := a.agentRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.SetDefaultAgentConfig(cmd.Context(), apiclientgen.SetDefaultAgentConfigParams{ProjectId: projectID, AgentConfigId: agentID})
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
		_, err = cmd.OutOrStdout().Write([]byte("default agent config set to " + shortID(agentID) + "\n"))
		return err
	}}
}

func (a *App) newAgentDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete AGENT_CONFIG_ID...", Short: "Delete agent configs", Args: cobra.MinimumNArgs(1), ValidArgsFunction: a.completeAgentConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		return runDeleteMany(cmd, args, "agent config", func(arg string) (string, error) {
			agentID, err := a.resolveAgentConfigID(cmd.Context(), client, projectID, arg)
			if err != nil {
				return "", err
			}
			res, err := client.DeleteAgentConfig(cmd.Context(), apiclientgen.DeleteAgentConfigParams{ProjectId: projectID, AgentConfigId: agentID})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeleteAgentConfigNoContent](res); err != nil {
				return "", err
			}
			return agentID, nil
		})
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

func (a *App) resolveAgentDefinition(ctx context.Context, client *apiclientgen.Client, value string) (*apimodel.AgentConfigDefinition, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("agent definition name is required")
	}
	res, err := client.ListAgentConfigDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListAgentConfigDefinitionsBody](res)
	if err != nil {
		return nil, err
	}
	var nameMatches []apimodel.AgentConfigDefinition
	var ids []string
	definitions := body.GetAgentConfigDefinitions()
	for _, definition := range definitions {
		ids = append(ids, definition.ID)
		if definition.Name == value {
			matched := definition
			return &matched, nil
		}
		if strings.EqualFold(definition.Name, value) {
			nameMatches = append(nameMatches, definition)
		}
	}
	if len(nameMatches) == 1 {
		return &nameMatches[0], nil
	}
	if len(nameMatches) > 1 {
		return nil, fmt.Errorf("agent definition name %q is ambiguous", value)
	}
	definitionID, err := resolveShortID(value, "agent definition ID", ids)
	if err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		if definition.ID == definitionID {
			matched := definition
			return &matched, nil
		}
	}
	return nil, fmt.Errorf("agent definition %q not found", value)
}

func (a *App) agentConfigByName(ctx context.Context, client *apiclientgen.Client, projectID, name string) (*apimodel.AgentConfig, error) {
	agents, err := a.listAgentConfigs(ctx, client, projectID)
	if err != nil {
		return nil, err
	}
	return agentConfigByName(agents, name), nil
}

func (a *App) listAgentConfigs(ctx context.Context, client *apiclientgen.Client, projectID string) ([]apimodel.AgentConfig, error) {
	res, err := client.ListAgentConfigs(ctx, apiclientgen.ListAgentConfigsParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListAgentConfigsBody](res)
	if err != nil {
		return nil, err
	}
	return body.GetAgentConfigs(), nil
}

func (a *App) defaultAgentConfigID(ctx context.Context, client *apiclientgen.Client, projectID string) (string, error) {
	res, err := client.GetProject(ctx, apiclientgen.GetProjectParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	project, err := expectResponse[apimodel.Project](res)
	if err != nil {
		return "", err
	}
	return project.DefaultAgentConfigId.Or(""), nil
}

func agentConfigByName(agents []apimodel.AgentConfig, name string) *apimodel.AgentConfig {
	for _, agent := range agents {
		if agent.Name == name {
			matched := agent
			return &matched
		}
	}
	return nil
}

func (a *App) setDefaultAgentConfig(ctx context.Context, client *apiclientgen.Client, projectID, agentID string) error {
	res, err := client.SetDefaultAgentConfig(ctx, apiclientgen.SetDefaultAgentConfigParams{ProjectId: projectID, AgentConfigId: agentID})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.Project](res)
	return err
}

func createAgentBody(opts agentCreateOptions) (*apimodel.CreateAgentConfigBody, error) {
	body := &apimodel.CreateAgentConfigBody{}
	body.SetName(optString(opts.name))
	body.SetDefinitionId(optString(opts.definitionID))
	if len(opts.installCommand) > 0 {
		body.SetInstallCommand(apiclientgen.NewOptNilStringArray(opts.installCommand))
	}
	if len(opts.runCommand) > 0 {
		body.SetRunCommand(apiclientgen.NewOptNilStringArray(opts.runCommand))
	}
	if len(opts.files) > 0 {
		files, err := parseAgentFileFlags(opts.files, opts.createOnlyFile)
		if err != nil {
			return nil, err
		}
		body.SetFiles(apiclientgen.NewOptNilAgentConfigFileArray(files))
	}
	return body, nil
}

func updateAgentBody(cmd *cobra.Command, opts agentUpdateOptions) (*apimodel.UpdateAgentConfigBody, error) {
	body := &apimodel.UpdateAgentConfigBody{}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	if cmd.Flags().Changed("install-command") {
		body.SetInstallCommand(apiclientgen.NewOptNilStringArray(opts.installCommand))
	}
	if cmd.Flags().Changed("run-command") {
		body.SetRunCommand(apiclientgen.NewOptNilStringArray(opts.runCommand))
	}
	if cmd.Flags().Changed("file") {
		files, err := parseAgentFileFlags(opts.files, opts.createOnlyFile)
		if err != nil {
			return nil, err
		}
		body.SetFiles(apiclientgen.NewOptNilAgentConfigFileArray(files))
	}
	return body, nil
}

func parseAgentFileFlags(values []string, createOnlyFiles []string) ([]apimodel.AgentConfigFile, error) {
	createOnly := map[string]struct{}{}
	for _, path := range createOnlyFiles {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		createOnly[path] = struct{}{}
	}
	files := make([]apimodel.AgentConfigFile, 0, len(values))
	for _, value := range values {
		path, content, ok := strings.Cut(value, "=")
		path = strings.TrimSpace(path)
		if !ok || path == "" {
			return nil, fmt.Errorf("--file must be PATH=CONTENT or PATH=@LOCALFILE, got %q", value)
		}
		if localPath, isLocalFile := strings.CutPrefix(content, "@"); isLocalFile {
			data, err := os.ReadFile(localPath)
			if err != nil {
				return nil, fmt.Errorf("read --file content %q: %w", localPath, err)
			}
			content = string(data)
		}
		files = append(files, apimodel.AgentConfigFile{Path: path, Content: content})
	}
	for filePath := range createOnly {
		found := false
		for _, file := range files {
			if file.Path == filePath {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("--create-only-file path %q has no matching --file entry", filePath)
		}
	}
	for i := range files {
		_, isCreateOnly := createOnly[files[i].Path]
		if isCreateOnly {
			files[i].CreateOnly = apiclientgen.NewOptBool(true)
		}
	}
	return files, nil
}
