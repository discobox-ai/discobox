package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/pkg/endpoint"
	"github.com/spf13/cobra"
)

// prepareRuntimeDirectories runs before the command opens a transport or the
// startup lock. Consent belongs to this invocation, never to a dial callback.
func (a *App) prepareRuntimeDirectories(cmd *cobra.Command) error {
	plan, err := endpoint.InspectRuntimeDirectories(a.serverURL)
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	defer plan.Close()
	out := cmd.ErrOrStderr()
	fmt.Fprintln(out, "The local server's runtime directories have unsafe permissions:")
	for _, change := range plan.Changes() {
		fmt.Fprintf(out, "  %q: %04o → 0700\n", change.Path, change.Mode)
	}
	fmt.Fprintln(out, "The runtime base may be shared with other applications. The existing environment could be insecure. Restricting permissions does not undo previous exposure or prove the old server trustworthy. Proceed only if you trust this environment.")
	recovery := "runtime permissions were not repaired; rerun interactively to approve, or inspect the listed directories and repair their permissions manually"
	if !canAskInline(cmd) {
		return fmt.Errorf("%s", recovery)
	}
	fmt.Fprint(out, "Trust this environment and change these permissions? [y/N] ")
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		return fmt.Errorf("%s: %w", recovery, err)
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return plan.Repair()
	default:
		return fmt.Errorf("%s", recovery)
	}
}

// Offline commands must remain usable to diagnose or select another server.
// Remote registration prepares the address argument in addServer instead.
func runtimePreparationNeeded(cmd *cobra.Command) bool {
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name())
	switch strings.TrimSpace(path) {
	case "", "completion", "admin server stage", "admin server manifest", "admin server logs", "admin peer id":
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(path), "admin remote")
}
