package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

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

This is the ProxyCommand ` + "`disco box ssh-config`" + ` writes. It reads SSH's wire
protocol on stdin, writes it on stdout, and carries it to the server's SSH
ingress over the same endpoint every other request uses — so ssh reaches a
sandbox whether or not the server binds an SSH port of its own.`,
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

// sshProxyCommandLine is the `ProxyCommand` line an emitted stanza carries: this
// executable, invoked with the endpoint this run was pointed at.
//
// The path is absolute rather than `disco`, because ssh runs the command
// through /bin/sh with whatever environment its caller had — and the caller is
// often a GUI editor whose PATH is not the shell's. --server is passed for the
// same reason: DISCOBOX_SERVER may not be set where ssh runs, and the config
// has to keep meaning what it meant when it was written.
func sshProxyCommandLine(serverURL string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this executable for the ssh_config ProxyCommand: %w", err)
	}
	return shellQuote(executable) + " --server " + shellQuote(serverURL) + " box ssh-proxy", nil
}

// shellQuote wraps a word so the shell reads it as one argument, whatever it
// contains. ssh hands ProxyCommand to a shell rather than running it directly,
// so a path with a space in it — the ordinary case on macOS and Windows —
// would otherwise arrive as two arguments.
//
// Which shell differs: /bin/sh everywhere but Windows, where OpenSSH hands the
// line to %COMSPEC%, which knows double quotes and not single ones. A Windows
// path cannot contain a double quote, so there is nothing to escape inside them.
func shellQuote(word string) string {
	if runtime.GOOS == "windows" {
		return `"` + word + `"`
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}
