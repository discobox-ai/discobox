package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/health"
)

// dockerProvider is the alternative a server offers when its VM backend cannot
// run (ADR 0148 §2), and the one this CLI knows how to describe.
const dockerProvider = "docker"

// answerDefaultProviderChoice asks what a server holding its first start wants
// to know: its default provider cannot run on this host, so should it install
// Docker instead (ADR 0148 §3).
//
// Every time it asks, it says what the answer gives up: a sandbox in a Docker
// container runs on this machine's kernel, with no VM between them. Without a
// terminal to ask on, or with --quiet, it asks nobody and fails with the same
// reason and the command that answers it, so a script is never told yes.
func (a *App) answerDefaultProviderChoice(ctx context.Context, choice health.Choice, in io.Reader) error {
	reason := defaultProviderUnavailableText(choice)
	answer := fmt.Sprintf("discobox admin server choose-provider %s", dockerProvider)
	if !slices.Contains(choice.Alternatives, dockerProvider) {
		return fmt.Errorf("%s, and the server offers nothing this CLI can choose instead (%s)", reason, strings.Join(choice.Alternatives, ", "))
	}
	if a.quiet || a.errOut == nil || !isTerminalStream(in) || !isTerminalStream(a.errOut) {
		return fmt.Errorf("%s.\nThe server is waiting for a choice. To run sandboxes as Docker containers on this machine's kernel instead, with no VM boundary: %s", reason, answer)
	}
	fmt.Fprintf(a.errOut, "%s.\nSandboxes can run as Docker containers on this machine's kernel instead, with no VM boundary between them and this machine.\nUse Docker? [y/N] ", reason)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read the answer: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
	default:
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(a.errOut)
		}
		return fmt.Errorf("no provider was chosen, so the server is still waiting; to choose Docker later: %s", answer)
	}
	return endpoint.ChooseDefaultProvider(ctx, a.serverURL, dockerProvider)
}

// defaultProviderUnavailableText says why a server's default provider cannot
// run, in terms a user can act on.
func defaultProviderUnavailableText(choice health.Choice) string {
	switch choice.Reason {
	case health.ReasonKVMUnavailable:
		return fmt.Sprintf("%s cannot run here: KVM is not available (%s)", choice.Provider, choice.Detail)
	case health.ReasonArchUnsupported:
		return fmt.Sprintf("%s cannot run here: its VMs need an x86-64 Linux host", choice.Provider)
	case health.ReasonRuntimeUnloadable:
		return fmt.Sprintf("%s cannot run here: its runtime would not load (%s)", choice.Provider, choice.Detail)
	default:
		return fmt.Sprintf("%s cannot run here: %s", choice.Provider, choice.Detail)
	}
}

func (a *App) newServerChooseProviderCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "choose-provider <provider>",
		Short: "Answer a local server waiting for its default provider to be chosen",
		Long: `Answer a local server that is holding its first start because the provider it
would install by default cannot run on this host. On Linux that provider is
libkrun, which runs each pool in a VM; the alternative is docker, which runs
sandboxes as containers on this machine's kernel with no VM boundary.

The CLI asks this itself when it starts a server from a terminal. This command
is for a server started some other way, or a CLI that had no terminal to ask on.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := endpoint.ChooseDefaultProvider(cmd.Context(), a.serverURL, args[0]); err != nil {
				return err
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"defaultProvider": args[0]})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s chosen; the server is finishing its start\n", args[0])
			return err
		},
	}
}
