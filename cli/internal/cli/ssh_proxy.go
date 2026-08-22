package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newSSHProxyCommand is the `ProxyCommand` the emitted ssh_config names. It
// splices its own stdin and stdout onto `GET /ssh/connect`, so `ssh` reaches
// the server's sshd over the endpoint the API already answers on.
//
// It is hidden because nothing types it: ssh runs it, once per connection, with
// the arguments the config was written with.
func (a *App) newSSHProxyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh-proxy",
		Short: "Carry one SSH connection to the server over the API endpoint",
		Long: `Carry one SSH connection to the server over the API endpoint.

This is the ProxyCommand ` + "`discobox admin ssh-config`" + ` writes. It reads SSH's wire
protocol on stdin, writes it on stdout, and carries it to the server's SSH
ingress over the same endpoint every other request uses — so ssh reaches a
discobox whether or not the server binds an SSH port of its own.`,
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runSSHProxy(cmd)
		},
	}
	return cmd
}

func (a *App) runSSHProxy(cmd *cobra.Command) error {
	dialer, err := a.sshConnectDialer()
	if err != nil {
		return err
	}
	remote, err := dialer.dial(cmd.Context())
	if err != nil {
		return fmt.Errorf("connect to the server's SSH ingress: %w", err)
	}
	defer remote.Close()
	// Nothing is written to stdout but the connection: it is the wire ssh is
	// reading. Errors go to stderr, which ssh already relays to its own.
	spliceSSHConnect(cmd.Context(), readWriter{r: cmd.InOrStdin(), w: cmd.OutOrStdout()}, remote)
	return nil
}

// readWriter pairs the two halves of a process's standard streams into the one
// stream the splice reads and writes. os.Stdin and os.Stdout are separate
// files, and the splice's local side is one connection's worth of bytes.
type readWriter struct {
	r io.Reader
	w io.Writer
}

func (rw readWriter) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw readWriter) Write(p []byte) (int, error) { return rw.w.Write(p) }
