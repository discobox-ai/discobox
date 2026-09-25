package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Execute runs the Discobox CLI root command.
func Execute(ctx context.Context) error {
	cmd, err := NewRootCommand().ExecuteContextC(ctx)
	if err == nil {
		return nil
	}
	return withNewFlagHint(cmd, withUnreachableServerHint(cmd, err))
}

// withNewFlagHint points a flag of new's that was written without new at the
// command it belongs to. The bare command took them once (ADR 0089) and no
// longer does (ADR 26-09-25-027), so `discobox -p '...'` is in scripts and in
// fingers, and cobra's own answer — an unknown flag — says nothing about where
// it went.
//
// Only for a flag the root itself failed to parse: a flag unknown to any other
// command is that command's business. Cobra hands back the root for a parse
// that failed anywhere on the way down (TraverseChildren), so the root being
// returned is not enough. What tells them apart is that Traverse parses a
// command's flags before it descends into one of its children: a failure below
// the root has always parsed a child of it first.
func withNewFlagHint(cmd *cobra.Command, err error) error {
	var unknown *pflag.NotExistError
	if cmd == nil || cmd != cmd.Root() || !errors.As(err, &unknown) {
		return err
	}
	for _, child := range cmd.Commands() {
		if child.Flags().Parsed() {
			return err
		}
	}
	create, _, findErr := cmd.Find([]string{"new"})
	if findErr != nil || create == cmd {
		return err
	}
	name := unknown.GetSpecifiedName()
	spelled := "--" + name
	flag := create.Flags().Lookup(name)
	if unknown.GetSpecifiedShortnames() != "" {
		spelled = "-" + name
		flag = create.Flags().ShorthandLookup(name)
	}
	if flag == nil {
		return err
	}
	return fmt.Errorf("%w\n%s is new's flag, and only new makes a discobox: %s new %s", err, spelled, cmd.Name(), spelled)
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
