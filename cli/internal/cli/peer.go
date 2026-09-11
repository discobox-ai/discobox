package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// newPeerCommand groups this machine's own identity and the peers a server
// admits.
//
// Nothing here says "iroh": managing who may reach a server is a thing an
// operator does about Discobox, not about the transport underneath it
// (ADR 0097 §7).
//
// Only `id` is local. It reads and generates this machine's key file and talks
// to no server, which is what makes it usable from a machine that has no access
// yet; everything else is an ordinary API call and so works over whatever
// transport --server names (ADR 0095 §5 on iroh IDs).
func (a *App) newPeerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "peer",
		Aliases: []string{"peers"},
		Short:   "Manage this machine's peer ID and the peers this server admits",
	}
	cmd.AddCommand(a.newPeerIDCommand())
	cmd.AddCommand(a.newPeerListCommand())
	cmd.AddCommand(a.newPeerAddCommand())
	cmd.AddCommand(a.newPeerRemoveCommand())
	return cmd
}

func (a *App) newPeerIDCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "id",
		Short: "Print this machine's peer ID",
		Long: "Print this machine's peer ID, generating the identity on first use.\n\n" +
			"Enroll the printed ID on a server — `discobox admin peer add <id>` there, or in\n" +
			"its authorized_ids file — to let this machine reach it. The ID is an address,\n" +
			"not a secret: it is disclosed to any relay that forwards for it, so enrolling\n" +
			"it is what grants access, and removing it is what revokes access.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := a.localPeerID(cmd, path)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "identity-file", "", "Path to the iroh identity key (default: the CLI state directory)")
	return cmd
}

// newIrohAliasCommands keeps the spellings operators' notes already use:
// `discobox admin iroh-id`, and `discobox admin iroh` with the same verbs.
// Breaking them to tidy a command tree is a cost paid by users (ADR 0095 §5 on
// iroh IDs, ADR 0097 §7).
func (a *App) newIrohAliasCommands() []*cobra.Command {
	id := a.newPeerIDCommand()
	id.Use = "iroh-id"
	id.Hidden = true
	id.Short = "Print this machine's peer ID (use \"peer id\")"

	group := a.newPeerCommand()
	group.Use = "iroh"
	group.Aliases = nil
	group.Hidden = true
	group.Short = "Manage peers (use \"peer\")"
	return []*cobra.Command{id, group}
}

func (a *App) newPeerListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the peers enrolled on this server",
		Long: "List the peers enrolled on this server.\n\n" +
			"Entries in the server's authorized_ids file are a separate layer and do not\n" +
			"appear here: they are not API-managed, and `rm` cannot remove them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			res, err := client.ListPeers(cmd.Context())
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListPeersBody](res)
			if err != nil {
				return err
			}
			return a.writePeers(cmd, body.GetPeers())
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newPeerAddCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "add [PEER_ID]",
		Short: "Enroll a peer on this server",
		Long: "Enroll a peer, permitting it to connect to this server.\n\n" +
			"With no argument, this machine's own ID is enrolled — what `discobox admin\n" +
			"peer id` prints — which is the usual shape of the first enrollment, run on the\n" +
			"server host over its local socket. After that, an enrolled client can enroll\n" +
			"the next one remotely.\n\n" +
			"An enrolled ID authenticates as this server's default user and reaches\n" +
			"everything that user reaches. It is not scoped to a project.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			var endpointID string
			if len(args) == 1 {
				endpointID = strings.TrimSpace(args[0])
			} else {
				local, err := a.localPeerID(cmd, "")
				if err != nil {
					return err
				}
				endpointID = local
			}
			body := &apimodel.CreatePeerBody{PeerId: endpointID}
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				body.SetName(apiclientgen.NewOptString(trimmed))
			}
			res, err := client.CreatePeer(cmd.Context(), body)
			if err != nil {
				return err
			}
			enrolled, err := expectResponse[apimodel.Peer](res)
			if err != nil {
				return err
			}
			return a.writePeer(cmd, enrolled)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Optional label for the peer")
	return cmd
}

func (a *App) newPeerRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm PEER_ID...",
		Aliases: []string{"delete"},
		Short:   "Revoke enrolled peers",
		Long: "Revoke enrolled peers, by full peer ID or by an unambiguous prefix.\n\n" +
			"A revoked ID is refused on its next connection; connections it already has\n" +
			"are not torn down. This cannot remove an entry from the server's\n" +
			"authorized_ids file, which is not API-managed.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			return runActionMany(cmd, args, "peer", "revoked", func(arg string) (string, error) {
				res, err := client.DeletePeer(cmd.Context(), apiclientgen.DeletePeerParams{PeerId: arg})
				if err != nil {
					return "", err
				}
				if err := expectNoContent[apiclientgen.DeletePeerNoContent](res); err != nil {
					return "", err
				}
				return arg, nil
			})
		},
	}
}

// localPeerID returns this machine's peer ID, generating the identity if it
// has none and saying so when it does — minting a credential silently is how an
// operator ends up with one they cannot find.
func (a *App) localPeerID(cmd *cobra.Command, path string) (string, error) {
	if path == "" {
		path = defaultIrohIdentityPath()
	}
	id, created, err := loadOrCreateIrohIdentity(path)
	if err != nil {
		return "", err
	}
	if created {
		fmt.Fprintf(cmd.ErrOrStderr(), "generated a new iroh identity at %s\n", path)
	}
	return id.String(), nil
}
