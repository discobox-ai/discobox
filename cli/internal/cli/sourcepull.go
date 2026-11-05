package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/spf13/cobra"
)

func credentialDeliveryOptions(cmd *cobra.Command, status *statusLine) sandboxcreate.DeliveryOptions {
	policy, path := sandboxcreate.GitCredentialPolicy(cmd.Context())
	options := sandboxcreate.DeliveryOptions{Credentials: policy, ConfigPath: path}
	if canAskInline(cmd) && !sandboxgit.Noninteractive(cmd.Context()) {
		options.RunCredential = func(_ context.Context, process *exec.Cmd) error {
			defer status.suspend()()
			process.Stderr = cmd.ErrOrStderr()
			return process.Run()
		}
		options.ConfirmCredential = func(_ context.Context, remote string) (bool, error) {
			defer status.suspend()()
			choice, err := pickOne(cmd, terminalSafe(remote)+" needs credentials to pull on the pool.", []pickerItem{{id: "lend", title: sandboxcreate.GitLendLabel, explanation: sandboxcreate.GitLendExplanation}, {id: "push", title: sandboxcreate.GitLocalLabel, explanation: sandboxcreate.GitLocalExplanation}}, pickerOptions{empty: "no choice to make", ambiguous: "choose --git-credentials=lend or --git-credentials=local"})
			return choice == "lend", err
		}
	}
	return options
}

// An explicit console choice lasts until exit; saved defaults are read again
// at each delivery, so removing saved lending consent takes effect immediately.
type gitCredentialOverrideKey struct{}

func resolveGitCredentials(ctx context.Context) (context.Context, error) {
	cfg, err := loadClientConfig()
	if err != nil {
		return nil, err
	}
	policy := cfg.Git.Credentials
	if override, ok := ctx.Value(gitCredentialOverrideKey{}).(sandboxcreate.GitCredentials); ok {
		policy = override
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return sandboxcreate.WithGitCredentials(ctx, policy, cfg.Path), nil
}

func (a *App) sourceRetryCommand(projectID, sandboxID string) string {
	return fmt.Sprintf("%s --server %s --project %s attach --git-credentials=local %s", commandName(), shellQuote(a.serverURL, runtime.GOOS == "windows"), shellQuote(projectID, runtime.GOOS == "windows"), shellQuote(sandboxID, runtime.GOOS == "windows"))
}
