package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// statusOptions are `disco status`'s git-compatible flags: `git status`'s own,
// in git's spelling, for the subset that still means something when the working
// tree being reported on is a sandbox's. The ones about a local repository —
// pathspec files read from this machine, submodule recursion into a checkout
// that is not here — are absent, and so is anything that would write.
type statusOptions struct {
	// Output format. Each is git's own flag and is passed straight through.
	short     bool
	long      bool
	porcelain string
	branch    bool
	showStash bool
	zero      bool
	column    string
	verbose   int

	// git's own negatable pairs, kept as the two flags git spells them as:
	// "not given" and "given false" are different answers, and only the flag
	// the user actually wrote is passed on.
	aheadBehind   bool
	noAheadBehind bool
	renames       bool
	noRenames     bool

	// What to report on.
	untrackedFiles   string
	ignored          string
	ignoreSubmodules string

	color string
}

// machineReadable reports whether the user asked for one of the formats meant
// to be parsed rather than read, which is what keeps per-source headings off
// stdout.
func (o statusOptions) machineReadable() bool {
	return o.porcelain != "" || o.zero
}

// gitArgs turns the options into `git status` arguments, in git's own spelling.
func (o statusOptions) gitArgs() []string {
	var args []string
	for _, flag := range []struct {
		name string
		on   bool
	}{
		{"--short", o.short},
		{"--long", o.long},
		{"--branch", o.branch},
		{"--show-stash", o.showStash},
		{"--null", o.zero},
		{"--ahead-behind", o.aheadBehind},
		{"--no-ahead-behind", o.noAheadBehind},
		{"--renames", o.renames},
		{"--no-renames", o.noRenames},
	} {
		if flag.on {
			args = append(args, flag.name)
		}
	}
	for _, valued := range []struct {
		name  string
		value string
	}{
		{"--porcelain", o.porcelain},
		{"--column", o.column},
		{"--untracked-files", o.untrackedFiles},
		{"--ignored", o.ignored},
		{"--ignore-submodules", o.ignoreSubmodules},
	} {
		if valued.value != "" {
			args = append(args, valued.name+"="+valued.value)
		}
	}
	for range o.verbose {
		args = append(args, "--verbose")
	}
	return args
}

// newStatusCommand implements `disco status`: `git status` for a sandbox's
// working trees, run where they are.
func (a *App) newStatusCommand() *cobra.Command {
	var sourceSlug string
	var opts statusOptions
	cmd := &cobra.Command{
		Use:   "status [SANDBOX_ID] [flags] [-- PATH...]",
		Short: "Show the state of a sandbox's working trees",
		Long: `Show the state of a sandbox's source working trees: what has been staged,
what has been changed but not staged, and what git has never been told about.

This is "git status" run inside the sandbox, against the source working trees
there rather than any checkout on this machine.

Without SANDBOX_ID the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Every source on the sandbox is reported by default (the primary source plus any
secondary ones), each under a heading naming it; --source narrows to one, named
by its slug. Paths after -- narrow the report to them, as git pathspecs.

"git status"'s own flags mean the same thing here and are passed through, in
git's spelling. A mode-taking flag needs its value attached with "=" — write
-u=no or --untracked-files=no, not -uno. The ones that are about a repository
on this machine are absent, and so is anything that writes.

Note that -s is git's --short here, not diff's --source; --source has no
shorthand in this command for exactly that reason.`,
		Example: `  disco status
  disco status --short --branch
  disco status sbx_01hq -- cli/ server/
  disco status --untracked-files=no
  disco status --porcelain --source docs`,
		Args: func(cmd *cobra.Command, args []string) error {
			if before := positionalsBeforeDash(cmd, args); before > 1 {
				return fmt.Errorf("accepts at most one sandbox ID, and %d were given; put pathspecs after --", before)
			}
			return nil
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if positionalsBeforeDash(cmd, args) > 0 {
				return nil, cobra.ShellCompDirectiveFilterDirs
			}
			return a.completeSandboxes(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var sandboxArg string
			var pathspecs []string
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				if dash > 0 {
					sandboxArg = args[0]
				}
				pathspecs = args[dash:]
			} else if len(args) > 0 {
				sandboxArg = args[0]
			}
			return a.runStatus(cmd, sandboxArg, sourceSlug, opts, pathspecs)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&sourceSlug, "source", "", "Report only the source with this slug, instead of every source on the sandbox")
	flags.BoolVarP(&opts.short, "short", "s", false, "Print status in git's short format")
	flags.BoolVar(&opts.long, "long", false, "Print status in git's long format, even when another format would be the default")
	flags.StringVar(&opts.porcelain, "porcelain", "", "Print status in git's machine-readable format, optionally versioned as v1 or v2")
	flags.Lookup("porcelain").NoOptDefVal = "v1"
	flags.BoolVarP(&opts.branch, "branch", "b", false, "Show the branch and tracking info even in short format")
	flags.BoolVar(&opts.showStash, "show-stash", false, "Show how many stash entries there are")
	flags.BoolVar(&opts.aheadBehind, "ahead-behind", false, "Compute the full ahead/behind counts against the upstream branch")
	flags.BoolVar(&opts.noAheadBehind, "no-ahead-behind", false, "Skip the ahead/behind counts and only report divergence")
	flags.BoolVar(&opts.renames, "renames", false, "Detect renames")
	flags.BoolVar(&opts.noRenames, "no-renames", false, "Do not detect renames")
	flags.BoolVarP(&opts.zero, "null", "z", false, "Terminate entries with NUL instead of newlines")
	flags.StringVar(&opts.column, "column", "", "Lay untracked files out in columns, as git's column options")
	flags.Lookup("column").NoOptDefVal = "always"
	flags.CountVarP(&opts.verbose, "verbose", "v", "Show the diff of staged changes; twice to also show the unstaged diff")
	flags.StringVarP(&opts.untrackedFiles, "untracked-files", "u", "", "Which untracked files to show: no, normal, or all")
	flags.Lookup("untracked-files").NoOptDefVal = "all"
	flags.StringVar(&opts.ignored, "ignored", "", "Which ignored files to show: traditional, matching, or no")
	flags.Lookup("ignored").NoOptDefVal = "traditional"
	flags.StringVar(&opts.ignoreSubmodules, "ignore-submodules", "", "When to ignore changes to submodules: none, untracked, dirty, or all")
	flags.Lookup("ignore-submodules").NoOptDefVal = "all"
	flags.StringVar(&opts.color, "color", "auto", "When to colorize: auto, always, or never")
	return cmd
}

func (a *App) runStatus(cmd *cobra.Command, sandboxArg, onlySlug string, opts statusOptions, pathspecs []string) error {
	ctx := cmd.Context()
	projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
	if err != nil {
		return err
	}
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return err
	}
	sources, err := selectSources(sandbox, onlySlug)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()
	gitArgs := opts.gitArgs()

	// A heading only earns a place on stdout when stdout is a person reading
	// several working trees at once. Anything being parsed — a porcelain or
	// NUL-separated listing, or any redirected output — gets git's bytes and
	// nothing else, with the headings on stderr.
	headings := stderr
	if !opts.machineReadable() && isTerminalStream(out) {
		headings = out
	}

	var failures []string
	for i, entry := range sources {
		if len(sources) > 1 {
			if i > 0 {
				fmt.Fprintln(headings)
			}
			fmt.Fprintf(headings, "source %s (%s)\n\n", entry.slug, diffSourceLocation(entry.source))
		}
		if err := a.streamSandboxStatus(ctx, cmd, projectID, sandboxID, sourceWorkdir(entry.source), statusCommand(gitArgs, opts.color, pathspecs)); err != nil {
			// A reader who quit a pipe has seen enough; that ends the run
			// rather than failing it, and nothing starts for the next source
			// whose output nothing would read either.
			if isBrokenPipe(err) {
				break
			}
			fmt.Fprintf(stderr, "source %s: %v\n", entry.slug, err)
			failures = append(failures, entry.slug)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d sources could not be reported: %s", len(failures), len(sources), strings.Join(failures, ", "))
	}
	return nil
}

// statusCommand builds the command run in a source's working tree. git status
// already reports untracked files, so unlike the diff there is no scratch index
// here: the working tree as it stands is exactly the subject.
//
// Color is a config setting rather than a flag on git status, so --color is
// spelled the way git itself would spell it; "auto" is git's own default and is
// left alone, so a PTY attach still colors and a pipe still does not.
func statusCommand(gitArgs []string, color string, pathspecs []string) []string {
	command := []string{"git"}
	if color == "always" || color == "never" {
		command = append(command, "-c", "color.status="+color)
	}
	command = append(command, "--no-pager", "status")
	command = append(command, gitArgs...)
	if len(pathspecs) > 0 {
		command = append(command, "--")
		command = append(command, pathspecs...)
	}
	return command
}

// streamSandboxStatus runs the status and passes git's bytes straight through,
// the same way `disco exec` and the passed-through `disco diff` do: nothing here
// has to understand the output.
func (a *App) streamSandboxStatus(ctx context.Context, cmd *cobra.Command, projectID, sandboxID, workdir string, command []string) error {
	// A PTY is only asked for when this really is a terminal on all three
	// streams, so a redirected status stays plain text; when there is one, git
	// colors and columnizes its own output.
	tty := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout()) && isTerminalStream(cmd.ErrOrStderr())
	body, err := createSandboxExecBody(sandboxExecCreateOptions{interactive: true, tty: tty, workdir: workdir}, command)
	if err != nil {
		return err
	}
	exec, err := a.createSandboxExec(ctx, projectID, sandboxID, body)
	if err != nil {
		return err
	}
	if err := a.attachSandboxExec(ctx, projectID, sandboxID, exec.ID, true, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return err
	}
	// git has already said why on stderr, so the exit status is the whole
	// message the caller needs to add.
	return a.returnSandboxExecStatus(ctx, projectID, sandboxID, exec.ID)
}
