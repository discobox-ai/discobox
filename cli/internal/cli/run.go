package cli

import (
	"context"
	"fmt"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/spf13/cobra"
)

type runCommandOptions struct {
	prompt sandboxcreate.PromptOptions
	detach bool
	// noSource creates the discobox with nothing materialized in it. -C still
	// says where the create came from, so the discobox is filed under this
	// directory and listed here; it just is not cut from it.
	noSource bool
	// declaredSources is the flag's positive form: the option it settles is
	// "skip them", because bringing them in is what declaring them asks for.
	declaredSources bool
}

func (a *App) newRunCommand() *cobra.Command {
	var opts runCommandOptions
	cmd := &cobra.Command{
		Use:     "run [flags] [PROMPT...]",
		Aliases: []string{"r"},
		Short:   "Launch prompt in new discobox",
		Long: `Launch a prompt in a new discobox against the current directory.

The arguments are the prompt. Use -- when the prompt needs to be separated from
command flags explicitly.

Every discobox has one default terminal: the configured harness, or a shell when
no harness is configured. By default run waits for the discobox to start and
attaches to that terminal, streaming it to your terminal (press Ctrl-A d to
detach; DISCOBOX_LEADER changes the Ctrl-A). Pass -d to create the discobox and
print it without attaching.

Uncommitted changes in the source directory are carried into the discobox as a
snapshot on top of the checked-out commit. By default run asks before doing that
when there is a terminal to ask on; --include-dirty=true|false answers ahead of
time.

A source directory that is not in a Git repository works too: everything in it
is carried into the discobox as uncommitted changes on an empty first commit,
and nothing is written to the directory itself. Because that is the whole
directory, run asks first — with the size it would copy, counted while the
question is on screen — and not copying is the default answer: the discobox is
still created, with nothing checked out in it, exactly as --no-source does.
--include-dirty=true|false answers this one ahead of time too.

--no-source creates a discobox with nothing checked out in it, for work that
starts from an empty machine rather than from a repository. The directory you
run in is still what the discobox is filed under, so it is listed here like any
other, and the Git authorship it commits under is still read from here; only the
source is left out. -i still brings sources in, so --no-source -i ../foo is a
discobox holding foo and nothing else.

-i brings extra sources into the same discobox, repeat it for more than one. Each
is resolved exactly like the source directory is, uncommitted changes included,
and a local one keeps its own absolute path inside the discobox, so ../foo shows
up at the path readlink -f ../foo prints.

A repository can name the others it is worked on with, in .discobox/sources.json
at its root:

  {"foo": "https://github.com/acme/foo"}

Each is brought in the way -i would: the ../foo you already have checked out
when there is one, and a clone of the URL when there is not. Either way it lands
at the same path beside the source, so ../foo means the same thing inside the
discobox as it does here. --declared-sources=false leaves them out.`,
		Example: `  discobox run fix the failing tests
  discobox run --include-dirty=false fix the failing tests
  discobox run -i ../foo -i ../bar make them share one client
  discobox run --no-source draft a proposal for the new pricing page
  discobox run -e GITHUB_TOKEN -e MODE=test fix the failing tests
  discobox run -s OPENAI_API_KEY=sk-... -s GITHUB_TOKEN=<sec_123> fix the failing tests
  discobox run -d fix the failing tests
  discobox run -- prompt starting with --flag-like text`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.prompt.Source = a.source
			opts.prompt.NoSource = opts.noSource
			opts.prompt.ConfirmIncludeDirty = confirmIncludeDirty(cmd)
			opts.prompt.ConfirmCopyDirectory = confirmCopyDirectory(cmd)
			opts.prompt.SkipDeclaredSources = !opts.declaredSources
			opts.prompt.ReportDeclaredSource = reportDeclaredSource(cmd)
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
			// Creating and delivering a source are this client's own work, so
			// nothing but this process can say which of them is underway
			// (ADR 0060). The line comes back down before anything else is
			// written to stderr.
			status := newStatusLine(cmd.ErrOrStderr())
			defer status.clear()
			report := func(step sandboxcreate.Step) { status.set(string(step)) }
			sandbox, local, err := sandboxcreate.CreatePromptSandbox(cmd.Context(), client, projectID, parsedOpts, report)
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
			err = sandboxcreate.DeliverSource(cmd.Context(), client, projectID, sandbox, local, gitServerURL, a.token, report)
			releaseGitServerURL()
			local.Close()
			status.clear()
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
	cmd.Flags().StringArrayVarP(&opts.prompt.Include, "include", "i", nil, "Additional source directory or Git repository to bring into the discobox, optionally with @REF; repeat for more than one. A local directory keeps its own absolute path inside the discobox and is named after itself, so -i ../foo is the source foo")
	cmd.Flags().StringVarP(&opts.prompt.Harness, "harness", "H", "", "Harness config to run, by slug (e.g. codex), name, or ID; defaults to the project default")
	cmd.Flags().BoolVarP(&opts.detach, "detach", "d", false, "Create the discobox and print it without attaching to its terminal")
	cmd.Flags().BoolVar(&opts.noSource, "no-source", false, "Create the discobox with nothing checked out in it; the directory you run in still decides where it is filed and what Git authorship it commits under")
	cmd.Flags().BoolVar(&opts.declaredSources, "declared-sources", true, "Bring in the sources the repository declares in .discobox/sources.json, using a local checkout beside the source directory when there is one")
	cmd.Flags().Var(&opts.prompt.IncludeDirty, "include-dirty", "Carry uncommitted changes in the local source into the discobox: true, false, or auto (ask when the workspace is dirty and this is a terminal). A source directory in no Git repository is uncommitted in its entirety, so this decides whether the directory itself is copied in")
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
				detail: "Start the discobox from a snapshot of the working tree",
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

// confirmCopyDirectory asks whether a source directory that is in no Git
// repository is copied into the sandbox. Everything in such a directory is
// uncommitted work, so the question is the whole directory — which is why it is
// asked at all, and why not copying leads: `discobox run` in a home directory
// should not carry the home directory. Declining creates the discobox with
// nothing checked out in it, the way --no-source does (ADR 0077 §1).
//
// The size is counted behind the question rather than before it, so the prompt
// comes up immediately and fills its number in as the walk finds it.
//
// Without a terminal there is nobody to ask and the directory is copied, which
// is what a source directory has always meant here; the alternative is a
// discobox that silently came up with nothing in it.
func confirmCopyDirectory(cmd *cobra.Command) sandboxcreate.ConfirmCopyDirectoryFunc {
	return func(_ context.Context, directory sandboxcreate.DirectoryCopy) (bool, error) {
		if !isTerminalStream(cmd.InOrStdin()) || !isTerminalStream(cmd.ErrOrStderr()) {
			return true, nil
		}
		live := func() (string, bool) {
			total := directory.Size.Total()
			return directoryCopyPrompt(directory.Dir, total), total.Done
		}
		prompt, _ := live()
		choice, err := pickOne(cmd, prompt, []pickerItem{
			{
				id:     "exclude",
				title:  "Do not copy the directory",
				detail: "Create the discobox with nothing checked out in it",
			},
			{
				id:     "copy",
				title:  "Copy the directory in",
				detail: "Everything in it arrives as uncommitted changes",
			},
		}, pickerOptions{
			empty:     "no choice to make",
			ambiguous: "pass --include-dirty=true or --include-dirty=false",
			live:      live,
		})
		if err != nil {
			return false, err
		}
		return choice == "copy", nil
	}
}

// directoryCopyPrompt says what copying this directory would carry. The size
// gets a line of its own, because it is the whole of what the answer turns on.
func directoryCopyPrompt(dir string, total sandboxcreate.DirectoryTotal) string {
	return fmt.Sprintf("%s is not a Git repository, so copying it into the discobox means all of it:\n%s",
		dir, directoryCopySize(total))
}

// directoryCopySize is that line, with as much of the count as the walk behind
// the question has reached. Nothing counted yet says so rather than reporting a
// zero that is about to be wrong.
func directoryCopySize(total sandboxcreate.DirectoryTotal) string {
	if total.Files == 0 && !total.Done {
		return "calculating…"
	}
	counted := fmt.Sprintf("%s in %d %s", humanBytes(total.Bytes), total.Files, pluralize("file", int(total.Files)))
	if !total.Done {
		counted += ", still counting…"
	}
	return counted
}

// reportDeclaredSource says where each source the repository declared came
// from. It is stderr rather than stdout: a declared source is context for what
// is being created, not part of the sandbox record -d prints.
//
// It is always reported, never only on a surprise. These sources are in the
// sandbox because a file in the repository asked for them, which is exactly the
// kind of thing a caller who did not write that file needs told.
func reportDeclaredSource(cmd *cobra.Command) sandboxcreate.ReportDeclaredSourceFunc {
	return func(source sandboxcreate.DeclaredSource) {
		out := cmd.ErrOrStderr()
		switch {
		case !source.Local:
			fmt.Fprintf(out, "source %s: cloning %s (no checkout at %s)\n",
				source.Name, source.URL, source.Checkout)
		case source.Origin != "":
			// The checkout is used anyway — a fork next door is the usual
			// reason, and is what the caller has — but a directory that only
			// shares the name looks identical from here, so say which it is.
			fmt.Fprintf(out, "source %s: %s (origin %s, declared %s)\n",
				source.Name, source.Checkout, source.Origin, source.URL)
		default:
			fmt.Fprintf(out, "source %s: %s\n", source.Name, source.Checkout)
		}
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
	fmt.Fprintf(cmd.ErrOrStderr(), "Created discobox %s, attaching when it is ready (%s to detach)...\n", sandbox.ID, a.detachHint())
	// Attach the virtual primary id: the sandbox-agent resolves it to whichever
	// exec is currently primary and relaunches a stopped one, so a primary that
	// ended before the attach is resumed instead of failing on a dead session.
	// Replay is on, so the sandbox-agent's own driving of the terminal before
	// run connects is shown from the start rather than only output produced
	// after the attach. If the sandbox itself stops, the attach ends — run does
	// not restart the sandbox.
	return a.attachSandboxTerminal(ctx, projectID, sandbox.ID, primaryExecID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
}
