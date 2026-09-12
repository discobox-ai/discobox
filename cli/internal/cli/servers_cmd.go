package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/endpoint"
)

// newServersCommand implements `discobox servers`: the servers this client
// lists discoboxes from beside its primary (ADR 0114 §3).
func (a *App) newServersCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "servers",
		Aliases: []string{"server"},
		Short:   "List and manage the servers discoboxes are listed from",
		Long: `List and manage the servers this client lists discoboxes from.

There is always a primary server: the one --server names, or the local one
when nothing does. Discoboxes are created there unless you choose otherwise, and
it is the only one started for you.

Registered servers are listed beside it. "discobox ls" and "discobox tui" list
every server's discoboxes, a command given a discobox ID finds it on whichever
server has it, and the launcher's run options can create one on any of them.
--server takes a registered server's name as well as an address.

A server's address is one of:

  discobox://<peer-id>        a peer, reached over iroh
  discobox://<host>:<port>    an https server, by IP or name
  discobox://<host>           a peer when a _discobox.<host> TXT record names
                              one, and an https server when none does
  discobox+http://<host>:<port>   a plain-http server, said outright

A discobox's address is its server's with the discobox after it,
discobox://<server>/<discobox>. Attaching to one registers the server it names.

With no subcommand, this lists the servers.`,
		Example: `  discobox servers
  discobox servers add discobox://box.example.com
  discobox servers add discobox://10.0.0.5:8443 --name lab
  discobox servers rename lab workstation
  discobox servers rm workstation`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.listServers(cmd)
		},
	}
	cmd.AddCommand(a.newServersListCommand())
	cmd.AddCommand(a.newServersAddCommand())
	cmd.AddCommand(a.newServersRenameCommand())
	cmd.AddCommand(a.newServersRemoveCommand())
	return cmd
}

func (a *App) newServersListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List the primary server and the registered ones",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.listServers(cmd)
		},
	}
}

// serverRow is one server as `discobox servers` prints it.
type serverRow struct {
	Name       string `json:"name"`
	Address    string `json:"address"`
	ID         string `json:"id,omitempty"`
	Primary    bool   `json:"primary"`
	Registered bool   `json:"registered"`
}

func (a *App) listServers(cmd *cobra.Command) error {
	set, err := a.servers()
	if err != nil {
		return err
	}
	// A primary nobody registered goes by the name and the peer ID it gives
	// when it answers; a registered server by what was recorded when it was
	// registered. Listing servers is not a reason to start one, so the primary
	// is asked only if it is already running.
	if primary := set[0]; !primary.registered {
		if baseURL, httpClient, err := a.httpClientWithAutoStart(false); err == nil {
			if client, err := apiclientgen.NewClient(baseURL, apiclientgen.WithClient(httpClient)); err == nil {
				if name := offeredName(cmd.Context(), client); name != "" {
					primary.name = name
				}
			}
		}
		primary.id = a.peerID(cmd.Context())
	}
	rows := make([]serverRow, 0, len(set))
	for _, s := range set {
		rows = append(rows, serverRow{Name: s.name, Address: s.address, ID: s.id, Primary: s.primary, Registered: s.registered})
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"servers": rows})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tADDRESS\tPRIMARY\tID")
	for _, row := range rows {
		primary := ""
		if row.Primary {
			primary = "yes"
		}
		id := row.ID
		if id == "" {
			id = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", row.Name, row.Address, primary, id)
	}
	return tw.Flush()
}

// serverAddTimeout bounds reaching a server being registered. It is longer than
// a listing's bound because this is the whole of what the command does, and a
// first dial through a relay can take several round trips.
const serverAddTimeout = 30 * time.Second

func (a *App) newServersAddCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "add ADDRESS",
		Short: "Register a server, under the name it offers unless --name gives one",
		Long: `Register a server, so "discobox ls" and "discobox tui" list its discoboxes.

The server is reached before anything is written down, so what gets registered
is a server that answered rather than a typo. It is registered under the name it
offers — its name setting, or its hostname — unless --name says otherwise, and
with its peer ID, which "discobox servers" lists and which recognizes the same
server registered again under another address.`,
		Example: `  discobox servers add discobox://box.example.com
  discobox servers add discobox://10.0.0.5:8443 --name lab`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.addServer(cmd, strings.TrimSpace(args[0]), strings.TrimSpace(name))
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Name to register the server under; defaults to the name the server offers")
	return cmd
}

func (a *App) addServer(cmd *cobra.Command, address, name string) error {
	if _, err := endpoint.Parse(address); err != nil {
		return err
	}
	reg, err := loadServerRegistry()
	if err != nil {
		return err
	}
	if i, ok := reg.byServer(address, ""); ok {
		return fmt.Errorf("%s is already registered, as %s", address, reg.Servers[i].Name)
	}
	if name != "" {
		if err := validServerName(name); err != nil {
			return err
		}
		if _, taken := reg.byName(name); taken {
			return fmt.Errorf("a server named %s is already registered", name)
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), serverAddTimeout)
	defer cancel()
	target := a.forServer(address)
	client, err := target.apiClient()
	if err != nil {
		return err
	}
	res, err := client.ListProjects(ctx)
	if err == nil {
		_, err = expectResponse[apimodel.ListProjectsBody](res)
	}
	if err != nil {
		return fmt.Errorf("reach %s: %w", address, err)
	}
	// The peer ID is what makes this address one more way to reach a server
	// already registered under another, which only the server can say.
	peerID := target.peerID(ctx)
	if i, ok := reg.byServer(address, peerID); ok {
		return fmt.Errorf("%s is peer %s, already registered as %s", address, peerID, reg.Servers[i].Name)
	}
	if name == "" {
		name = uniqueServerName(reg, registrationName(offeredName(ctx, client), address))
	}
	reg.Servers = append(reg.Servers, registeredServer{Name: name, Address: address, ID: peerID})
	if err := reg.save(); err != nil {
		return err
	}
	// Its discoboxes are listed from here now, so ssh reaches them from here
	// too: the same sync a create on that server does. A failure is a note
	// rather than the command's, since the server is registered either way and
	// `discobox admin ssh-config --write` is the way to try again.
	notes := printedNotes(cmd.ErrOrStderr())
	registered := &server{name: name, address: address, registered: true, app: a.forServer(address)}
	if err := registered.writeSSHConfig(ctx, "", notes); err != nil {
		notes("could not sync the SSH config for %s: %v", name, err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), name)
	return nil
}

func (a *App) newServersRenameCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "rename NAME NEW_NAME",
		Short:             "Rename a registered server",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeServerNames(1),
		RunE: func(_ *cobra.Command, args []string) error {
			from, to := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
			reg, err := loadServerRegistry()
			if err != nil {
				return err
			}
			i, ok := reg.byName(from)
			if !ok {
				return fmt.Errorf("no server named %s is registered", from)
			}
			if err := validServerName(to); err != nil {
				return err
			}
			if j, taken := reg.byName(to); taken && j != i {
				return fmt.Errorf("a server named %s is already registered", to)
			}
			reg.Servers[i].Name = to
			return reg.save()
		},
	}
}

func (a *App) newServersRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "rm NAME...",
		Aliases:           []string{"remove"},
		Short:             "Stop listing a registered server's discoboxes; the server is not touched",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeServerNames(-1),
		RunE: func(_ *cobra.Command, args []string) error {
			reg, err := loadServerRegistry()
			if err != nil {
				return err
			}
			for _, name := range args {
				i, ok := reg.byName(strings.TrimSpace(name))
				if !ok {
					return fmt.Errorf("no server named %s is registered", name)
				}
				reg.Servers = append(reg.Servers[:i], reg.Servers[i+1:]...)
			}
			return reg.save()
		},
	}
}

// completeServerNames offers the registered servers' names for the first
// positions arguments, or every argument when positions is negative.
func completeServerNames(positions int) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if positions >= 0 && len(args) >= positions {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		reg, err := loadServerRegistry()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names := make([]string, 0, len(reg.Servers))
		for _, entry := range reg.Servers {
			names = append(names, entry.Name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}
