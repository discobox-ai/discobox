package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// The project's gateways (ADR 26-10-16-470 §6): routes every discobox of the
// project is sent through by its pool's proxy.
func (a *App) newGatewayCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "gateway",
		Aliases: []string{"gateways"},
		Short:   "Manage the gateways a project's discoboxes are routed through",
		Long: `Manage the project's gateways.

A gateway sends the requests every discobox of the project makes to one API —
an origin and a path prefix — to another origin and prefix instead, with
headers removed, set, or added on the way. The pool's proxy applies it after
everything else has judged the request as the discobox sent it, so a
discobox cannot see or bypass it, and a credential it holds for the API is
sent on to the gateway.

A gateway the server's configuration file names is managed by config: it is
listed and shown here, and changed only in that file.`,
	}
	cmd.AddCommand(a.newGatewayListCommand())
	cmd.AddCommand(a.newGatewayGetCommand())
	cmd.AddCommand(a.newGatewaySetCommand())
	cmd.AddCommand(a.newGatewayDeleteCommand())
	return cmd
}

func (a *App) newGatewayListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List gateways", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		res, err := client.ListProjectGateways(cmd.Context(), apiclientgen.ListProjectGatewaysParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListProjectGatewaysBody](res)
		if err != nil {
			return err
		}
		return a.writeGateways(cmd, body.GetGateways())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newGatewayGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get NAME", Short: "Show a gateway", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		res, err := client.GetProjectGateway(cmd.Context(), apiclientgen.GetProjectGatewayParams{ProjectId: projectID, GatewayName: args[0]})
		if err != nil {
			return err
		}
		gw, err := expectResponse[apimodel.ProjectGateway](res)
		if err != nil {
			return err
		}
		return a.writeGateway(cmd, gw)
	}}
}

func (a *App) newGatewaySetCommand() *cobra.Command {
	var match, target string
	var setHeaders, addHeaders, removeHeaders []string
	cmd := &cobra.Command{
		Use:   "set NAME --match URL --target URL",
		Short: "Create a gateway, or replace the one of that name",
		Long: `Create a gateway, or replace the one of that name as a whole: what the flags
say is the gateway, and anything they leave out is gone from it.

--match is an http(s) URL of an origin and a path prefix; a request beneath
it, on whole path segments, is routed. --target is where it goes: the matched
prefix is replaced by the target's path, and the rest of the path and the
query are kept. The longest match of the project's gateways wins.

Headers are removed, then set (replacing what the discobox sent), then added.
A header's value is written in the flag. A header whose value comes from the
Discobox server's environment, such as the gateway's key, is set only in the
server's configuration file, which names its gateways itself.`,
		Example: `  discobox admin gateway set anthropic \
    --match https://api.anthropic.com/v1 \
    --target https://gateway.example.com/anthropic/v1 \
    --set-header x-team=platform`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := gatewayHeaderFlags(setHeaders)
			if err != nil {
				return err
			}
			add, err := gatewayHeaderFlags(addHeaders)
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			body := &apimodel.PutProjectGatewayBody{Match: match, Target: target}
			if len(removeHeaders) > 0 || len(set) > 0 || len(add) > 0 {
				body.RequestHeaders = apiclientgen.NewOptPutGatewayHeaderModifier(apimodel.PutGatewayHeaderModifier{Remove: removeHeaders, Set: set, Add: add})
			}
			res, err := client.PutProjectGateway(cmd.Context(), body, apiclientgen.PutProjectGatewayParams{ProjectId: projectID, GatewayName: args[0]})
			if err != nil {
				return err
			}
			gw, err := expectResponse[apimodel.ProjectGateway](res)
			if err != nil {
				return err
			}
			return a.writeGateway(cmd, gw)
		},
	}
	cmd.Flags().StringVar(&match, "match", "", "The requests to route: an http(s) URL of an origin and a path prefix")
	cmd.Flags().StringVar(&target, "target", "", "Where they go: an http(s) URL of an origin and the path that replaces the match's")
	cmd.Flags().StringArrayVar(&setHeaders, "set-header", nil, "Set a header, replacing the discobox's value: NAME=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&addHeaders, "add-header", nil, "Add a header beside the discobox's value: NAME=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&removeHeaders, "remove-header", nil, "Remove a header the discobox sent: NAME (repeatable)")
	_ = cmd.MarkFlagRequired("match")
	_ = cmd.MarkFlagRequired("target")
	return cmd
}

// gatewayHeaderFlags reads NAME=VALUE flags into headers.
func gatewayHeaderFlags(values []string) ([]apimodel.PutGatewayHeader, error) {
	var out []apimodel.PutGatewayHeader
	for _, flag := range values {
		name, value, err := splitHeaderFlag(flag)
		if err != nil {
			return nil, err
		}
		out = append(out, apimodel.PutGatewayHeader{Name: name, Value: value})
	}
	return out, nil
}

func splitHeaderFlag(flag string) (string, string, error) {
	name, value, ok := strings.Cut(flag, "=")
	if !ok || strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("header %q must be NAME=VALUE", flag)
	}
	return strings.TrimSpace(name), value, nil
}

func (a *App) newGatewayDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "rm NAME...", Aliases: []string{"delete"}, Short: "Remove gateways", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "gateway", "remove", "removed", func(name string) (string, error) {
			res, err := client.DeleteProjectGateway(cmd.Context(), apiclientgen.DeleteProjectGatewayParams{ProjectId: projectID, GatewayName: name})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeleteProjectGatewayNoContent](res); err != nil {
				return "", err
			}
			return name, nil
		})
	}}
}

func (a *App) writeGateways(cmd *cobra.Command, gateways []apimodel.ProjectGateway) error {
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), gateways, func(gw apimodel.ProjectGateway) string { return gw.Name })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"gateways": gateways})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMATCH\tTARGET\tMANAGED BY")
	for _, gw := range gateways {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", terminalSafe(gw.Name), terminalSafe(gw.Match), terminalSafe(gw.Target), gw.ManagedBy)
	}
	return tw.Flush()
}

func (a *App) writeGateway(cmd *cobra.Command, gw *apimodel.ProjectGateway) error {
	if gw == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), gw)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "NAME\t%s\n", terminalSafe(gw.Name))
	fmt.Fprintf(tw, "MATCH\t%s\n", terminalSafe(gw.Match))
	fmt.Fprintf(tw, "TARGET\t%s\n", terminalSafe(gw.Target))
	fmt.Fprintf(tw, "MANAGED BY\t%s\n", gw.ManagedBy)
	headers := gw.RequestHeaders
	for _, name := range headers.Remove {
		fmt.Fprintf(tw, "REMOVE\t%s\n", terminalSafe(name))
	}
	for _, header := range headers.Set {
		fmt.Fprintf(tw, "SET\t%s\n", gatewayHeaderLine(header))
	}
	for _, header := range headers.Add {
		fmt.Fprintf(tw, "ADD\t%s\n", gatewayHeaderLine(header))
	}
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatTime(gw.UpdatedAt))
	return tw.Flush()
}

// gatewayHeaderLine shows a header's written value, or the variable it is
// read from: the value of that is the server's, never the CLI's to show.
func gatewayHeaderLine(header apimodel.GatewayHeader) string {
	if source, ok := header.ValueFrom.Get(); ok {
		return terminalSafe(header.Name) + ": env:" + terminalSafe(source.Env)
	}
	return terminalSafe(header.Name) + ": " + terminalSafe(header.Value.Or(""))
}
