package cli

import (
	"context"
	"os/exec"

	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/spf13/cobra"
)

func credentialDeliveryOptions(cmd *cobra.Command, status *statusLine, allowed bool) sandboxcreate.DeliveryOptions {
	options := sandboxcreate.DeliveryOptions{LendCredentials: allowed}
	if canAskInline(cmd) {
		options.RunCredential = func(_ context.Context, process *exec.Cmd) error {
			defer status.suspend()()
			process.Stderr = cmd.ErrOrStderr()
			return process.Run()
		}
		options.ConfirmCredential = func(_ context.Context, remote string) (bool, error) {
			defer status.suspend()()
			choice, err := pickOne(cmd, terminalSafe(remote)+" needs credentials to pull on the pool. Use your Git credentials?", []pickerItem{{id: "lend", title: "Use Git credentials for this pull"}, {id: "push", title: "Fetch here and push"}}, pickerOptions{empty: "no choice to make", ambiguous: "pass --lend-git-credentials to allow lending"})
			return choice == "lend", err
		}
	}
	return options
}
