package cli

import (
	"context"
	"fmt"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type runCommandOptions struct {
	prompt sandboxcreate.PromptOptions
	// promptFlag is -p. The prompt is normally the words after the command,
	// which a shell has already split; this is the same prompt as one argument,
	// for a caller that would rather quote it than fight the shell — and the
	// only way to give one to the bare `discobox`, where an unquoted prompt
	// would be indistinguishable from a subcommand.
	promptFlag []string
	detach     bool
	// noSource creates the discobox with nothing materialized in it. -C still
	// says where the create came from, so the discobox is filed under this
	// directory and listed here; it just is not cut from it.
	noSource bool
	// declaredSources is the flag's positive form: the option it settles is
	// "skip them", because bringing them in is what declaring them asks for.
	declaredSources bool
	// raw attaches this terminal to the discobox's terminal directly, instead
	// of opening the window on it.
	raw bool
}

func (a *App) newRunCommand() *cobra.Command {
	var opts runCommandOptions
	cmd := &cobra.Command{
		Use:     "run [flags] [PROMPT...]",
		Aliases: []string{"r"},
		Short:   "Launch prompt in new discobox",
		Long: `Launch a prompt in a new discobox against the current directory.

The arguments are the prompt. Use -- when the prompt needs to be separated from
command flags explicitly, or pass it as -p to give it as one argument instead —
the only way to spell a prompt to the bare "discobox", which is this command in
every way that matters (see "discobox --help").

By default run opens the launcher's window and makes the discobox there: the
question about uncommitted work is asked on it, the wait is drawn on it, and
what it lands on is the discobox itself — its default terminal (the configured
harness, or a shell when it has none), the shells and services running beside
it, and its forwarded ports. It is the same screen, in the same window, that
typing the same thing into "discobox tui" gives you. Press Ctrl-A d to leave,
which detaches and exits; the discobox and everything in it keeps running
(DISCOBOX_LEADER changes the Ctrl-A).

--raw creates the discobox here instead and attaches this terminal to its
default terminal: the stream and nothing else, for a pipe, a recording, or a
terminal you would rather keep as it is. The questions are asked on this
terminal, one per source. Ctrl-A d detaches there too. If an interrupt stops
getting through — the discobox or the server has gone quiet — Ctrl-C again says
so, and one more quits, leaving the terminal running like a detach. Without a
terminal to draw a window on, run is raw whether or not the flag was given.

Pass -d to create the discobox and print it without attaching at all; there is
no window in that either.

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
  discobox run --raw fix the failing tests
  discobox run -- prompt starting with --flag-like text
  discobox -H codex -d -p 'fix the failing tests'`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runPrompt(cmd, &opts, args)
		},
	}
	addRunFlags(cmd, &opts)
	return cmd
}

// runPrompt creates the discobox a run describes. It is the body of `discobox
// run` and of the bare `discobox` that stands in for it: the shortcut is the
// same command reached without its name, not a second one that has to be kept
// in step with it.
//
// args is the prompt as the shell split it, after -p, which is the same prompt
// given as one argument.
func (a *App) runPrompt(cmd *cobra.Command, opts *runCommandOptions, args []string) error {
	// Creating and delivering a source are this client's own work, so
	// nothing but this process can say which of them is underway
	// (ADR 0060). The line comes back down before anything else is
	// written to stderr — and everything else this command writes there
	// while it is up goes through it, since it owns the row it is on.
	status := newStatusLine(cmd.ErrOrStderr())
	defer status.clear()
	// -p and the words after the command are the same prompt, so a caller can
	// use whichever the shell makes easier and both arrive as argv tokens. The
	// flag leads because it is the one that had to be quoted.
	prompt := append(append([]string(nil), opts.promptFlag...), args...)
	opts.prompt.Source = a.source
	opts.prompt.NoSource = opts.noSource
	opts.prompt.ConfirmIncludeDirty = confirmIncludeDirty(cmd, status)
	opts.prompt.ConfirmCopyDirectory = confirmCopyDirectory(cmd, status)
	opts.prompt.SkipDeclaredSources = !opts.declaredSources
	opts.prompt.ReportDeclaredSource = reportDeclaredSource(status)
	parsedOpts, err := sandboxcreate.ParsePromptOptions(opts.prompt, prompt)
	if err != nil {
		return err
	}
	// The window is where a run goes unless something says otherwise,
	// and it is given the request rather than the discobox: the create
	// happens inside it, so the question about uncommitted work is the
	// window's own dialog and the wait is its own screen rather than a
	// list with a busy line under it. The flags are parsed above
	// either way — a flag that contradicts itself is this command's
	// error to report, on this terminal, before a window opens over it.
	//
	// -d has nothing to open a window for: it prints the discobox and
	// returns, and printing is stdout, which the window would be
	// sitting on.
	if !opts.detach && !opts.raw && canOpenWindow(cmd) {
		return a.runTUI(cmd, "", tui.WithRun(a.runWindowRequest(opts, prompt)))
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
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
	if err := a.writeProjectSSHConfig(cmd, client, projectID, ""); err != nil {
		return fmt.Errorf("sync SSH config: %w", err)
	}
	if opts.detach {
		return a.writeSandbox(cmd, sandbox)
	}
	return a.attachRunSandbox(cmd, projectID, sandbox)
}

// addRunFlags gives cmd everything a run takes, and hands back the set it
// added. Both spellings of a run register them from here — `discobox run` and
// the bare `discobox` that stands in for it — so the two cannot drift into
// taking different flags, and the returned set is how the bare one asks whether
// any of them was given at all.
func addRunFlags(cmd *cobra.Command, opts *runCommandOptions) *pflag.FlagSet {
	flags := pflag.NewFlagSet("run", pflag.ContinueOnError)
	flags.StringArrayVarP(&opts.promptFlag, "prompt", "p", nil, "Prompt for the harness, as one argument; repeat to pass more argv tokens. The same thing as the words after the command, and the only spelling the bare \"discobox\" has for a prompt")
	flags.StringArrayVarP(&opts.prompt.Env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables. A KEY whose name contains KEY, TOKEN, PASS, or SECRET is treated as a secret; use KEY!=VALUE to force it to be a plain environment variable")
	flags.StringArrayVarP(&opts.prompt.Secret, "secret", "s", nil, "Secret injected as a sentinel placeholder resolved by the proxy at runtime, as KEY=VALUE (inline value) or KEY=<SECRET_ID> (reference an existing secret); repeat for multiple secrets")
	flags.StringArrayVarP(&opts.prompt.Include, "include", "i", nil, "Additional source directory or Git repository to bring into the discobox, optionally with @REF; repeat for more than one. A local directory keeps its own absolute path inside the discobox and is named after itself, so -i ../foo is the source foo")
	flags.StringVarP(&opts.prompt.Harness, "harness", "H", "", "Harness config to run, by slug (e.g. codex), name, or ID; defaults to the project default")
	flags.BoolVarP(&opts.detach, "detach", "d", false, "Create the discobox and print it without attaching to its terminal")
	flags.BoolVar(&opts.raw, "raw", false, "Create the discobox here and attach this terminal straight to its terminal, instead of making it in the window")
	flags.BoolVar(&opts.noSource, "no-source", false, "Create the discobox with nothing checked out in it; the directory you run in still decides where it is filed and what Git authorship it commits under")
	flags.BoolVar(&opts.declaredSources, "declared-sources", true, "Bring in the sources the repository declares in .discobox/sources.json, using a local checkout beside the source directory when there is one")
	flags.Var(&opts.prompt.IncludeDirty, "include-dirty", "Carry uncommitted changes in the local source into the discobox: true, false, or auto (ask when the workspace is dirty and this is a terminal). A source directory in no Git repository is uncommitted in its entirety, so this decides whether the directory itself is copied in")
	flags.Lookup("include-dirty").NoOptDefVal = string(sandboxcreate.IncludeDirtyAlways)
	cmd.Flags().AddFlagSet(flags)
	return flags
}

// runRequested reports whether an invocation of the bare `discobox` is a run
// rather than a launcher. A prompt is one, and so is any of run's own flags on
// their own: `discobox -d` has said enough about what it wants for a window to
// be the wrong answer, while `discobox` with nothing at all is the launcher it
// has always been.
func runRequested(flags *pflag.FlagSet, args []string) bool {
	if len(args) > 0 {
		return true
	}
	given := false
	flags.VisitAll(func(flag *pflag.Flag) { given = given || flag.Changed })
	return given
}

// confirmIncludeDirty asks whether uncommitted local work should be carried
// into the sandbox. It is only ever called for --include-dirty=auto against a
// dirty workspace. Without a terminal there is nobody to ask, so the work is
// included: that is what run has always done, and dropping edits silently is
// worse than carrying them.
func confirmIncludeDirty(cmd *cobra.Command, status *statusLine) sandboxcreate.ConfirmIncludeDirtyFunc {
	return func(_ context.Context, workspace sandboxcreate.DirtyWorkspace) (bool, error) {
		if !isTerminalStream(cmd.InOrStdin()) || !isTerminalStream(cmd.ErrOrStderr()) {
			return true, nil
		}
		// The question draws its own screen on the stream the wait is being
		// narrated on, so the narration gives the row up for as long as it is
		// on and comes back after it.
		defer status.suspend()()
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
func confirmCopyDirectory(cmd *cobra.Command, status *statusLine) sandboxcreate.ConfirmCopyDirectoryFunc {
	return func(_ context.Context, directory sandboxcreate.DirectoryCopy) (bool, error) {
		if !isTerminalStream(cmd.InOrStdin()) || !isTerminalStream(cmd.ErrOrStderr()) {
			return true, nil
		}
		defer status.suspend()()
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
//
// It goes through the status line rather than to the stream directly, because
// this runs while the create is being narrated on that same stream and the
// narration owns the row it is on. Written past it, each of these lines came
// out with the spinner on the front of it.
func reportDeclaredSource(status *statusLine) sandboxcreate.ReportDeclaredSourceFunc {
	return func(source sandboxcreate.DeclaredSource) {
		status.print("%s", declaredSourceLine(source))
	}
}

// declaredSourceLine is that report as one line. It is shared with the window,
// which narrates the same line on its busy line rather than printing it: the
// two frontends say the same thing about the same source, in the form each of
// them has for saying it.
func declaredSourceLine(source sandboxcreate.DeclaredSource) string {
	switch {
	case !source.Local:
		return fmt.Sprintf("source %s: cloning %s (no checkout at %s)",
			source.Name, source.URL, source.Checkout)
	case source.Origin != "":
		// The checkout is used anyway — a fork next door is the usual reason,
		// and is what the caller has — but a directory that only shares the
		// name looks identical from here, so say which it is.
		return fmt.Sprintf("source %s: %s (origin %s, declared %s)",
			source.Name, source.Checkout, source.Origin, source.URL)
	default:
		return fmt.Sprintf("source %s: %s", source.Name, source.Checkout)
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

// runWindowRequest is this command as the window takes it: `discobox run`'s
// flags in the shape the launcher's own Enter produces, so what the window
// creates is what this command describes.
//
// The values go over as they were given — the source with its @REF, the prompt
// as the words the shell split — because the window's create runs them through
// the same parse this command has just validated them with. Two flags do not go
// over: -d, which never reaches a window, and --raw, which is the choice not to
// open one.
func (a *App) runWindowRequest(opts *runCommandOptions, prompt []string) tui.RunRequest {
	req := tui.RunRequest{
		Prompt:              prompt,
		Source:              a.source,
		NoSource:            opts.noSource,
		Harness:             opts.prompt.Harness,
		Env:                 opts.prompt.Env,
		Secret:              opts.prompt.Secret,
		Include:             opts.prompt.Include,
		SkipDeclaredSources: !opts.declaredSources,
	}
	// auto is the window's empty: the question it puts up. The other two are
	// answers already given, and the window hands them to the create for every
	// source it cuts from, as this command does.
	switch opts.prompt.IncludeDirty {
	case sandboxcreate.IncludeDirtyAlways:
		req.IncludeDirty = "true"
	case sandboxcreate.IncludeDirtyNever:
		req.IncludeDirty = "false"
	}
	return req
}

// attachRunSandbox streams the freshly created sandbox's default terminal to
// this one. Every sandbox gets one primary terminal from the sandbox-agent —
// the configured harness, or a plain shell when it has none — so run attaches
// to it unless --detach was passed.
//
// This is the --raw path, and what a run with no terminal to draw a window on
// gets. A run that can open one never reaches here: it hands its request to the
// window, which creates the discobox itself and opens on it.
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
