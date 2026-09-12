package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// Execute runs the Discobox CLI root command.
func Execute(ctx context.Context) error {
	cmd, err := NewRootCommand().ExecuteContextC(ctx)
	if err == nil {
		return nil
	}
	return withUnreachableServerHint(cmd, err)
}

// withUnreachableServerHint names the command that says where a connection
// stops, on the failures that mean it never got there.
//
// A request that dies in the transport arrives as the address it could not
// reach and the syscall that refused it — `dial unix …: connect: no such file
// or directory` — which says a server is not there and nothing about why; over
// iroh it says less still, because four layers fail with one sentence.
// `admin server status` is what takes that apart, and it is a command somebody
// has to be told about: it is deliberately not at the top level (ADR 0112), and
// a broken connection is the worst moment to go looking for it. So the failure
// names it.
func withUnreachableServerHint(cmd *cobra.Command, err error) error {
	if cmd == nil || !unreachableServer(err) {
		return err
	}
	// Not on the diagnosis itself. Its report has already named the layer that
	// failed and what to do about it, and telling somebody to run the command
	// they are running is the one hint that helps nobody.
	if cmd.Name() == "status" {
		return err
	}
	return fmt.Errorf("%w\nrun `%s admin server status` to see where the connection stops", err, cmd.Root().Name())
}

// unreachableServer reports whether err is the control-plane transport failing
// to reach the server rather than anything else that failed on the way.
//
// The mark serverTransport leaves is the whole test. Matching *url.Error or
// *net.OpError instead would catch every other request this process makes — the
// release asset `admin server stage` downloads above all, which is run by
// somebody who has no server yet and would be told to diagnose one — and a
// local listener that could not bind. A response the server produced is not
// here at all: the generated client hands those back as typed responses, and
// responseError has already said what is wrong with the request.
func unreachableServer(err error) bool {
	// A command the user interrupted, or a deadline this CLI set for itself, is
	// not a server that could not be reached.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var unreachable serverUnreachable
	return errors.As(err, &unreachable)
}
