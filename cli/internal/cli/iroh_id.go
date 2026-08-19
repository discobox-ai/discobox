package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (a *App) newIrohIDCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "iroh-id",
		Short: "Print this machine's iroh endpoint ID",
		Long: "Print this machine's iroh endpoint ID, generating the identity on first use.\n\n" +
			"An operator enrolls the printed ID in a server's authorized_ids file to let\n" +
			"this machine reach it over iroh. The ID is an address, not a secret: it is\n" +
			"disclosed to any relay that forwards for it, so enrolling it is what grants\n" +
			"access, and removing it is what revokes access.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				path = defaultIrohIdentityPath()
			}
			id, created, err := loadOrCreateIrohIdentity(path)
			if err != nil {
				return err
			}
			if created {
				// Say so rather than silently minting a credential, and say
				// where, so it can be found, backed up, or deleted.
				fmt.Fprintf(cmd.ErrOrStderr(), "generated a new iroh identity at %s\n", path)
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "identity-file", "", "Path to the iroh identity key (default: the CLI state directory)")
	return cmd
}
