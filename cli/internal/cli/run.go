package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
	"github.com/spf13/cobra"
)

type runCommandOptions struct {
	prompt sandboxcreate.PromptOptions
	detach bool
}

func (a *App) newRunCommand() *cobra.Command {
	var opts runCommandOptions
	cmd := &cobra.Command{
		Use:     "run [flags] [PROMPT...]",
		Aliases: []string{"r"},
		Short:   "Launch prompt in new sandbox",
		Long: `Launch a prompt in a new sandbox against the current directory.

The arguments are the prompt. Use -- when the prompt needs to be separated from
command flags explicitly.

Every sandbox has one default terminal: the configured harness, or a shell when
no harness is configured. By default run waits for the sandbox to start and
attaches to that terminal, streaming it to your terminal (press Ctrl-P Ctrl-Q to
detach). Pass -d to create the sandbox and print it without attaching.

Uncommitted changes in the source directory are carried into the sandbox as a
snapshot on top of the checked-out commit. By default run asks before doing that
when there is a terminal to ask on; --include-dirty=true|false answers ahead of
time.`,
		Example: `  disco run fix the failing tests
  disco run --include-dirty=false fix the failing tests
  disco run -e GITHUB_TOKEN -e MODE=test fix the failing tests
  disco run -s OPENAI_API_KEY=sk-... -s GITHUB_TOKEN=<sec_123> fix the failing tests
  disco run -d fix the failing tests
  disco run -- prompt starting with --flag-like text`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.prompt.Source = a.source
			opts.prompt.ConfirmIncludeDirty = confirmIncludeDirty(cmd)
			parsedOpts, err := sandboxcreate.ParsePromptOptions(opts.prompt, args)
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			sandbox, err := sandboxcreate.CreatePromptSandbox(cmd.Context(), client, projectID, parsedOpts)
			if err != nil {
				return err
			}
			// A server that cannot reach this directory waits for us to push it.
			gitServerURL, releaseGitServerURL, err := a.gitServerURL(cmd.Context())
			if err != nil {
				return err
			}
			err = sandboxcreate.DeliverSource(cmd.Context(), client, projectID, sandbox, parsedOpts.Source, gitServerURL, a.token)
			releaseGitServerURL()
			if err != nil {
				return err
			}
			if opts.detach {
				return a.writeSandbox(cmd, sandbox)
			}
			return a.attachRunSandbox(cmd, client, projectID, sandbox)
		},
	}
	cmd.Flags().StringArrayVarP(&opts.prompt.Env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables. A KEY whose name contains KEY, TOKEN, PASS, or SECRET is treated as a secret; use KEY!=VALUE to force it to be a plain environment variable")
	cmd.Flags().StringArrayVarP(&opts.prompt.Secret, "secret", "s", nil, "Secret injected as a sentinel placeholder resolved by the proxy at runtime, as KEY=VALUE (inline value) or KEY=<SECRET_ID> (reference an existing secret); repeat for multiple secrets")
	cmd.Flags().StringVarP(&opts.prompt.Harness, "harness", "H", "", "Harness config to run, by slug (e.g. codex), name, or ID; defaults to the project default")
	cmd.Flags().BoolVarP(&opts.detach, "detach", "d", false, "Create the sandbox and print it without attaching to its terminal")
	cmd.Flags().Var(&opts.prompt.IncludeDirty, "include-dirty", "Carry uncommitted changes in the local source into the sandbox: true, false, or auto (ask when the workspace is dirty and this is a terminal)")
	cmd.Flags().Lookup("include-dirty").NoOptDefVal = string(sandboxcreate.IncludeDirtyAlways)
	return cmd
}

// confirmIncludeDirty asks whether uncommitted local work should be carried
// into the sandbox. It is only ever called for --include-dirty=auto against a
// dirty workspace. Without a terminal there is nobody to ask, so the work is
// included: that is what run has always done, and dropping edits silently is
// worse than carrying them.
func confirmIncludeDirty(cmd *cobra.Command) sandboxcreate.ConfirmIncludeDirtyFunc {
	return func(_ context.Context, workspace sandboxcreate.DirtyWorkspace) (bool, error) {
		if !isTerminalStream(cmd.InOrStdin()) || !isTerminalStream(cmd.ErrOrStderr()) {
			return true, nil
		}
		// Excluding leads, so the default answer is the one that changes nothing
		// about what the sandbox sees: the committed history.
		choice, err := pickOne(cmd, dirtyWorkspacePrompt(workspace), []pickerItem{
			{
				id:     "exclude",
				title:  "Start from the last commit",
				detail: "Leave the uncommitted changes here",
			},
			{
				id:     "include",
				title:  "Include uncommitted changes",
				detail: "Start the sandbox from a snapshot of the working tree",
			},
		}, pickerOptions{
			empty:     "no choice to make",
			ambiguous: "pass --include-dirty=true or --include-dirty=false",
		})
		if err != nil {
			return false, err
		}
		return choice == "include", nil
	}
}

// dirtyWorkspacePrompt names what the choice is about: how many paths differ
// from the checked-out commit, and enough of them to recognize the change.
func dirtyWorkspacePrompt(workspace sandboxcreate.DirtyWorkspace) string {
	const shown = 3
	paths := make([]string, 0, len(workspace.Changes))
	for _, change := range workspace.Changes {
		paths = append(paths, change.Path)
	}
	summary := strings.Join(paths[:min(shown, len(paths))], ", ")
	if len(paths) > shown {
		summary = fmt.Sprintf("%s and %d more", summary, len(paths)-shown)
	}
	return fmt.Sprintf("%s has %d uncommitted %s (%s)", workspace.RepoRoot, len(paths), pluralize("change", len(paths)), summary)
}

// attachRunSandbox waits for the freshly created sandbox to start and for its
// default terminal to come up, then attaches the caller's stdio to it. Every
// sandbox gets one primary terminal from the sandbox-agent — the configured
// harness, or a plain shell when it has none — so run attaches to it unless
// --detach was passed.
func (a *App) attachRunSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID string, sandbox *apimodel.Sandbox) error {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()
	fmt.Fprintf(stderr, "Created sandbox %s, provisioning (fetching source, starting container)...\n", sandbox.ID)
	if _, err := a.waitForSandbox(cmd, client, projectID, sandbox.ID, 2*time.Minute); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "Sandbox running, preparing default terminal...")
	terminal, err := a.waitForPrimaryTerminal(ctx, stderr, projectID, sandbox.ID, 2*time.Minute)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Attaching to terminal %s (Ctrl-P Ctrl-Q to detach)\n", terminal.ID)
	// Attach the virtual primary id rather than the id just polled for: the
	// sandbox-agent resolves it to whichever exec is currently primary and
	// relaunches a stopped one, so a primary that ended between the wait and the
	// attach is resumed instead of failing on a dead session. Replay is on, so the
	// sandbox-agent's own driving of the terminal before run connects is shown
	// from the start rather than only output produced after the attach. If the
	// sandbox itself stops, the attach ends — run does not restart the sandbox.
	return a.attachSandboxTerminal(ctx, projectID, sandbox.ID, primaryExecID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// waitForPrimaryTerminal polls the sandbox terminals until the primary
// (default) terminal launched by the sandbox-agent is ready to attach. The
// primary appears in the "installing" phase while its hooks and files are
// prepared, so this reports that phase to
// progress and only returns once the terminal is past installing.
func (a *App) waitForPrimaryTerminal(ctx context.Context, progress io.Writer, projectID, sandboxID string, timeout time.Duration) (apimodel.SandboxExec, error) {
	if timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastErr error
	announcedInstalling := false
	for {
		if terminals, err := a.listSandboxTerminals(ctx, projectID, sandboxID); err != nil {
			lastErr = err
		} else if terminal, ok := primaryTerminal(terminals); ok {
			lastErr = nil
			if terminal.Status == apiclientgen.SandboxExecStatusInstalling {
				if !announcedInstalling && progress != nil {
					fmt.Fprintf(progress, "Preparing harness %s...\n", runHarnessLabel(terminal))
					announcedInstalling = true
				}
			} else {
				return terminal, nil
			}
		} else {
			lastErr = nil
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return apimodel.SandboxExec{}, fmt.Errorf("waiting for sandbox terminal: %w", lastErr)
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if announcedInstalling {
					return apimodel.SandboxExec{}, errors.New("timed out while the harness was still preparing hooks and files (see `disco box terminal logs`)")
				}
				return apimodel.SandboxExec{}, errors.New("timed out waiting for the sandbox's default terminal; it may have failed to start (see `disco box terminal logs`)")
			}
			return apimodel.SandboxExec{}, fmt.Errorf("waiting for sandbox terminal: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// runHarnessLabel names the harness a terminal runs for progress messages, falling
// back to a generic label when the harness id is not set.
func runHarnessLabel(terminal apimodel.SandboxExec) string {
	if harness := strings.TrimSpace(terminal.HarnessId.Or("")); harness != "" {
		return fmt.Sprintf("%q", harness)
	}
	return "harness"
}

// primaryTerminal selects the sandbox's default terminal: the one flagged
// primary by the sandbox-agent, falling back to the oldest terminal.
func primaryTerminal(terminals []apimodel.SandboxExec) (apimodel.SandboxExec, bool) {
	var oldest *apimodel.SandboxExec
	for i := range terminals {
		if terminals[i].Primary.Or(false) {
			return terminals[i], true
		}
		if oldest == nil || terminals[i].CreatedAt.Before(oldest.CreatedAt) {
			oldest = &terminals[i]
		}
	}
	if oldest != nil {
		return *oldest, true
	}
	return apimodel.SandboxExec{}, false
}
