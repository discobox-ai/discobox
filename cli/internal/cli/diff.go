package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/diffrender"
)

// diffOptions are `disco diff`'s git-compatible flags: the ones from `git diff`
// that still mean something when the diff is "everything this sandbox has
// changed since it started". Flags that select what to compare are absent —
// that is what the command decides — and so are the ones about a local
// repository, an index, or a pager, none of which the sandbox has a say in.
type diffOptions struct {
	// Output selection. Each is git's own flag, and each turns off the rendered
	// view: what these ask for is git's output, so it is passed through.
	stat       bool
	numstat    bool
	shortstat  bool
	nameOnly   bool
	nameStatus bool
	summary    bool
	patch      bool

	// Comparison behavior. These apply in every view.
	unified           int
	ignoreAllSpace    bool
	ignoreSpaceChange bool
	ignoreBlankLines  bool
	findRenames       bool
	findCopies        bool
	diffFilter        string

	// base overrides the commit the diff is measured from.
	base  string
	color string
}

// rawGitOutput reports whether the user asked for one of git's own output
// formats, which is passed through untouched rather than rendered.
func (o diffOptions) rawGitOutput() bool {
	return o.stat || o.numstat || o.shortstat || o.nameOnly || o.nameStatus || o.summary || o.patch
}

// gitArgs turns the options into `git diff` arguments, in git's own spelling.
func (o diffOptions) gitArgs(unifiedSet bool) []string {
	var args []string
	for _, flag := range []struct {
		name string
		on   bool
	}{
		{"--stat", o.stat},
		{"--numstat", o.numstat},
		{"--shortstat", o.shortstat},
		{"--name-only", o.nameOnly},
		{"--name-status", o.nameStatus},
		{"--summary", o.summary},
		{"--patch", o.patch},
		{"--ignore-all-space", o.ignoreAllSpace},
		{"--ignore-space-change", o.ignoreSpaceChange},
		{"--ignore-blank-lines", o.ignoreBlankLines},
		{"--find-renames", o.findRenames},
		{"--find-copies", o.findCopies},
	} {
		if flag.on {
			args = append(args, flag.name)
		}
	}
	if unifiedSet {
		args = append(args, "--unified="+strconv.Itoa(o.unified))
	}
	if o.diffFilter != "" {
		args = append(args, "--diff-filter="+o.diffFilter)
	}
	return args
}

// newDiffCommand implements `disco diff`: everything a sandbox has changed in
// its sources, measured from the commit each source was cloned at.
func (a *App) newDiffCommand() *cobra.Command {
	var sourceSlug string
	var opts diffOptions
	cmd := &cobra.Command{
		Use:   "diff [SANDBOX_ID] [flags] [-- PATH...]",
		Short: "Show what a sandbox has changed, since the commit it cloned",
		Long: `Show what a sandbox has changed in its sources, against the commit each
source was cloned at.

Without SANDBOX_ID the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

Every source on the sandbox is diffed by default (the primary source plus any
secondary ones); --source narrows to one, named by its slug. Paths after --
narrow the diff to them, as git pathspecs.

The diff covers everything that has happened in the sandbox since it started,
not just what has been committed there: commits, staged changes, unstaged
edits, and new files that were never added.

It is measured from the commit the source was cloned at, so this means what
"git diff <that commit>" means inside the sandbox — with the addition of files
git was never told about, which a plain git diff cannot see.

The exception is a sandbox that pulled from upstream after it was created: the
merge base with the branch it was cloned at is used instead, so commits it
fetched rather than wrote are left out. Every diff says which commit it used
and why.

Use --base to measure from any other commit. --base snapshot names the
workspace snapshot a sandbox created from a dirty local tree started with,
which answers the narrower question of what the sandbox changed on top of what
it was handed.

At a terminal the diff is rendered to be read — file headings, line numbers,
and the changed part of each line highlighted. Redirected, it is a plain
unified patch that "git apply" accepts. --patch is that patch at a terminal
too, and --stat and the other git output formats are git's own output,
unrendered.

Most of "git diff"'s flags mean the same thing here and are passed through.
The ones that choose what to compare are not: the right-hand side is always
the sandbox's working tree, and the left is the base described above.`,
		Example: `  disco diff
  disco diff --stat
  disco diff sbx_01hq -- cli/ server/
  disco diff --source docs > docs.patch`,
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
			return a.runDiff(cmd, sandboxArg, sourceSlug, opts, pathspecs)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&sourceSlug, "source", "s", "", "Diff only the source with this slug, instead of every source on the sandbox")
	flags.BoolVar(&opts.stat, "stat", false, "Print git's diffstat instead of the diff")
	flags.BoolVar(&opts.numstat, "numstat", false, "Print git's machine-readable diffstat")
	flags.BoolVar(&opts.shortstat, "shortstat", false, "Print only git's diffstat summary line")
	flags.BoolVar(&opts.nameOnly, "name-only", false, "Print only the changed paths")
	flags.BoolVar(&opts.nameStatus, "name-status", false, "Print the changed paths with their change type")
	flags.BoolVar(&opts.summary, "summary", false, "Print git's summary of creations, deletions, renames, and mode changes")
	flags.BoolVar(&opts.patch, "patch", false, "Print the plain unified patch, unrendered, even at a terminal")
	flags.IntVarP(&opts.unified, "unified", "U", 3, "Lines of context around each change")
	flags.BoolVarP(&opts.ignoreAllSpace, "ignore-all-space", "w", false, "Ignore whitespace entirely when comparing lines")
	flags.BoolVarP(&opts.ignoreSpaceChange, "ignore-space-change", "b", false, "Ignore changes in the amount of whitespace")
	flags.BoolVar(&opts.ignoreBlankLines, "ignore-blank-lines", false, "Ignore changes that only add or remove blank lines")
	flags.BoolVarP(&opts.findRenames, "find-renames", "M", false, "Detect renames")
	flags.BoolVar(&opts.findCopies, "find-copies", false, "Detect copies as well as renames")
	flags.StringVar(&opts.diffFilter, "diff-filter", "", "Only show files with these change types, as git's A/C/D/M/R/T letters")
	flags.StringVar(&opts.base, "base", "", "Commit to diff against, instead of the one the source was cloned at; \"snapshot\" names the dirty-workspace snapshot the sandbox started with")
	flags.StringVar(&opts.color, "color", "auto", "When to colorize: auto, always, or never")
	return cmd
}

// positionalsBeforeDash counts the arguments that belong to the command itself
// rather than to the pathspecs after --.
func positionalsBeforeDash(cmd *cobra.Command, args []string) int {
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		return dash
	}
	return len(args)
}

func (a *App) runDiff(cmd *cobra.Command, sandboxArg, onlySlug string, opts diffOptions, pathspecs []string) error {
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

	out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	gitArgs := opts.gitArgs(cmd.Flags().Changed("unified"))
	// The rendered view needs the whole diff before it can lay any of it out,
	// so it is the one that buys the text instead of streaming it. Git's own
	// formats stream, which is what a piped patch or a very large diff wants.
	render := !opts.rawGitOutput() && isTerminalStream(out)
	// A rendered diff is a document, not a patch, so its headings belong with
	// it on stdout. A passed-through diff is a patch, and nothing but the patch
	// may reach stdout.
	headings := stderr
	if render {
		headings = out
	}

	var failures []string
	for i, entry := range sources {
		if i > 0 && render {
			fmt.Fprintln(headings)
		}
		workdir := sourceWorkdir(entry.source)
		base, upstreamRef, err := a.resolveDiffBase(ctx, projectID, sandboxID, workdir, entry.source, opts.base)
		if err != nil {
			fmt.Fprintf(stderr, "source %s: %v\n", entry.slug, err)
			failures = append(failures, entry.slug)
			continue
		}
		// The base is always named, for one source as much as for several: a
		// diff means nothing without the commit it is measured from, and this
		// one is chosen rather than given.
		fmt.Fprintf(headings, "source %s (%s) diffed from %s — %s\n\n",
			entry.slug, diffSourceLocation(entry.source), shortSHA(base.Commit), base.describe(upstreamRef))

		command := sandboxDiffCommand(base.Commit, gitArgs, pathspecs)
		var runErr error
		if render {
			runErr = a.renderSandboxDiff(ctx, cmd, projectID, sandboxID, workdir, command, opts, base.Commit)
		} else {
			runErr = a.streamSandboxDiff(ctx, cmd, projectID, sandboxID, workdir, command)
		}
		if runErr != nil {
			fmt.Fprintf(stderr, "source %s: %v\n", entry.slug, runErr)
			failures = append(failures, entry.slug)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d sources could not be diffed: %s", len(failures), len(sources), strings.Join(failures, ", "))
	}
	return nil
}

// streamSandboxDiff runs the diff and passes git's bytes straight through, the
// same way `disco exec` does: nothing here has to understand the output, and a
// diff of any size costs no memory.
func (a *App) streamSandboxDiff(ctx context.Context, cmd *cobra.Command, projectID, sandboxID, workdir string, command []string) error {
	// A PTY is only asked for when this really is a terminal on all three
	// streams, so a redirected diff stays a plain patch; when there is one, git
	// colors its own output.
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

// renderSandboxDiff runs the diff, then lays the patch out for reading.
func (a *App) renderSandboxDiff(ctx context.Context, cmd *cobra.Command, projectID, sandboxID, workdir string, command []string, opts diffOptions, base string) error {
	patch, errOutput, code, err := a.sandboxCommandOutput(ctx, projectID, sandboxID, workdir, command)
	if err != nil {
		return err
	}
	if code != 0 {
		message := strings.TrimSpace(errOutput)
		if message == "" {
			message = strings.TrimSpace(patch)
		}
		return fmt.Errorf("git diff exited %d: %s", code, message)
	}
	if strings.TrimSpace(errOutput) != "" {
		fmt.Fprint(cmd.ErrOrStderr(), errOutput)
	}

	files := diffrender.Parse(patch)
	if len(files) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no changes since %s\n", shortSHA(base))
		return nil
	}
	out := cmd.OutOrStdout()
	color := diffColorEnabled(opts.color, out)
	if color {
		// The profile writer downsamples 256-color styling for a 16-color
		// terminal and strips it for a dumb one, so the renderer never has to
		// ask what the terminal can do.
		writer := colorprofile.NewWriter(out, os.Environ())
		if opts.color == "always" && writer.Profile <= colorprofile.NoTTY {
			writer.Profile = colorprofile.ANSI256
		}
		out = writer
	}
	return diffrender.Render(out, files, diffrender.Options{
		Width: terminalWidth(cmd.OutOrStdout()),
		Color: color,
		Dark:  hasDarkTerminalBackground(cmd),
	})
}

// diffColorEnabled resolves git's own --color=auto|always|never against the
// output stream.
func diffColorEnabled(when string, out io.Writer) bool {
	switch when {
	case "never":
		return false
	case "always":
		return true
	default:
		return isTerminalStream(out)
	}
}

// hasDarkTerminalBackground asks the terminal for its background color, which
// decides whether the changed-line backgrounds are the dark or the light set.
// A terminal that will not answer is treated as dark: that is the common case,
// and the query is skipped entirely when there is no terminal to ask.
func hasDarkTerminalBackground(cmd *cobra.Command) bool {
	in, inOK := cmd.InOrStdin().(*os.File)
	out, outOK := cmd.OutOrStdout().(*os.File)
	if !inOK || !outOK || !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return true
	}
	return lipgloss.HasDarkBackground(in, out)
}

// sourceCheckoutCommit is the commit a source was cloned at, recorded on the
// sandbox at create. It is the sandbox's own starting point, which is what
// makes it the base a diff measures the sandbox's work from — including work
// carried in from a dirty local workspace, which arrives as uncommitted changes
// on top of exactly this commit.
func sourceCheckoutCommit(source apimodel.GitSource) string {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return ""
	}
	return strings.TrimSpace(checkout.Commit.Or(""))
}

// diffSourceLocation names a source in a heading: where it lives in the
// sandbox, falling back to the sandbox's default working directory when the
// source never recorded one.
func diffSourceLocation(source apimodel.GitSource) string {
	if workdir := sourceWorkdir(source); workdir != "" {
		return workdir
	}
	return "default working directory"
}

// sandboxDiffCommand builds the command run in a source's working tree: a
// tree-to-tree diff of the base against everything the sandbox has now.
//
// The right-hand side is built rather than named. `git diff BASE` compares the
// base against the working tree, which covers commits, staged and unstaged
// changes alike — but only for paths git already knows, and an agent's
// unfinished work is largely files git has never been told about. Worse,
// against a base that *does* contain them (a workspace snapshot) they are
// reported as deletions, because the index is what git consults for what
// exists.
//
// So the working tree is written into a scratch index as a real tree object,
// untracked files included, and the diff becomes an ordinary comparison of two
// trees. The scratch index is seeded from the real one, which keeps git's stat
// cache and so keeps this to hashing what actually changed; GIT_INDEX_FILE
// points every write at the copy, so the repository's own index — and the work
// going on in it — is never touched.
func sandboxDiffCommand(base string, gitArgs, pathspecs []string) []string {
	// "$tree" is the shell's own variable and so is the one word here that must
	// not be quoted as a literal; everything else is.
	diff := shellCommand(append(append([]string{"git", "--no-pager", "diff"}, gitArgs...), base)) +
		` "$tree" --`
	for _, pathspec := range pathspecs {
		diff += " " + shellQuote(pathspec)
	}
	script := `
index=$(mktemp) || exit 1
trap 'rm -f "$index"' EXIT
# mktemp leaves an empty file, and git rejects an empty index outright rather
# than initializing it, so the name is reserved and the file is not.
rm -f "$index"
# Seeding from the repository's own index is purely a speed matter: without it
# "git add" rehashes every file in the tree instead of consulting git's stat
# cache. A repository that has no index yet simply starts from none.
cp "$(git rev-parse --git-dir)/index" "$index" 2>/dev/null || :
GIT_INDEX_FILE="$index" git add -A || exit $?
tree=$(GIT_INDEX_FILE="$index" git write-tree) || exit $?
` + diff + `
`
	return []string{"sh", "-c", script}
}

// shellCommand renders argv as a shell command line, quoting every word so
// nothing in it can be read as syntax.
func shellCommand(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

// shellQuote wraps a word in single quotes, where the shell interprets nothing
// at all, and splices in any single quote the word itself contains.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
