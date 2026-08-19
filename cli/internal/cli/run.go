package cli

import (
	"context"
	"fmt"
	"strings"

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
attaches to that terminal, streaming it to your terminal (press Ctrl-A d to
detach; DISCOBOX_LEADER changes the Ctrl-A). Pass -d to create the sandbox and
print it without attaching.

Uncommitted changes in the source directory are carried into the sandbox as a
snapshot on top of the checked-out commit. By default run asks before doing that
when there is a terminal to ask on; --include-dirty=true|false answers ahead of
time.

A source directory that is not in a Git repository works too: everything in it
is carried into the sandbox as uncommitted changes on an empty first commit,
and nothing is written to the directory itself.

-i brings extra sources into the same sandbox, repeat it for more than one. Each
is resolved exactly like the source directory is, uncommitted changes included,
and a local one keeps its own absolute path inside the sandbox, so ../foo shows
up at the path readlink -f ../foo prints.`,
		Example: `  disco run fix the failing tests
  disco run --include-dirty=false fix the failing tests
  disco run -i ../foo -i ../bar make them share one client
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
			sandbox, local, err := sandboxcreate.CreatePromptSandbox(cmd.Context(), client, projectID, parsedOpts)
			if err != nil {
				return err
			}
			// The local source is done as soon as it has been delivered, which
			// for a directory that is not a repository means deleting the
			// repository built over it. The defer covers the paths that never
			// reach the delivery.
			defer local.Close()
			// A server that cannot reach this directory waits for us to push it.
			gitServerURL, releaseGitServerURL, err := a.gitServerURL(cmd.Context())
			if err != nil {
				return err
			}
			err = sandboxcreate.DeliverSource(cmd.Context(), client, projectID, sandbox, local, gitServerURL, a.token)
			releaseGitServerURL()
			local.Close()
			if err != nil {
				return err
			}
			if opts.detach {
				return a.writeSandbox(cmd, sandbox)
			}
			return a.attachRunSandbox(cmd, projectID, sandbox)
		},
	}
	cmd.Flags().StringArrayVarP(&opts.prompt.Env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables. A KEY whose name contains KEY, TOKEN, PASS, or SECRET is treated as a secret; use KEY!=VALUE to force it to be a plain environment variable")
	cmd.Flags().StringArrayVarP(&opts.prompt.Secret, "secret", "s", nil, "Secret injected as a sentinel placeholder resolved by the proxy at runtime, as KEY=VALUE (inline value) or KEY=<SECRET_ID> (reference an existing secret); repeat for multiple secrets")
	cmd.Flags().StringArrayVarP(&opts.prompt.Include, "include", "i", nil, "Additional source directory or Git repository to bring into the sandbox, optionally with @REF; repeat for more than one. A local directory keeps its own absolute path inside the sandbox and is named after itself, so -i ../foo is the source foo")
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

// attachRunSandbox attaches the caller's stdio to the freshly created
// sandbox's default terminal. Every sandbox gets one primary terminal from the
// sandbox-agent — the configured harness, or a plain shell when it has none —
// so run attaches to it unless --detach was passed.
//
// It does not wait for the sandbox first. The attach itself waits, at every
// tier that can see something the one above it cannot: the control plane for
// the sandbox to be dispatched and its pool to be up, the pool agent for the
// container, and the sandbox agent for the primary terminal's launch and
// install (ADR 0039). Polling here for readiness the server already knows
// about cost one request per second of provisioning and had to be reinvented
// by every client.
//
// Source delivery is the exception and stays above this call: a sandbox whose
// source must be pushed cannot start until this client pushes it, so nothing
// server-side can subsume that step.
func (a *App) attachRunSandbox(cmd *cobra.Command, projectID string, sandbox *apimodel.Sandbox) error {
	ctx := cmd.Context()
	fmt.Fprintf(cmd.ErrOrStderr(), "Created sandbox %s, attaching when it is ready (%s to detach)...\n", sandbox.ID, a.detachHint())
	// Attach the virtual primary id: the sandbox-agent resolves it to whichever
	// exec is currently primary and relaunches a stopped one, so a primary that
	// ended before the attach is resumed instead of failing on a dead session.
	// Replay is on, so the sandbox-agent's own driving of the terminal before
	// run connects is shown from the start rather than only output produced
	// after the attach. If the sandbox itself stops, the attach ends — run does
	// not restart the sandbox.
	return a.attachSandboxTerminal(ctx, projectID, sandbox.ID, primaryExecID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
}
