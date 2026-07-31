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
	base string
	// applyPreview measures from where apply would start instead, answering
	// what applying this sandbox would land here.
	applyPreview bool
	noPager      bool
	// dirOverrides names the local directory a source compares against, for
	// --base local; the same slug=path form apply uses.
	dirOverrides []string
	color        string
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

Use --base to measure from any other commit, and two keywords for the states
that are not commits you could name:

  --base local     this machine's working tree, uncommitted changes included,
                   so the diff is everything that differs between the two
                   checkouts. Unlike every other mode this one needs the
                   source's local repository on this machine, and takes --dir
                   slug=path when the sandbox does not know where that is.
  --base snapshot  the workspace snapshot a sandbox created from a dirty local
                   tree started with, which answers the narrower question of
                   what the sandbox changed on top of what it was handed.

--apply-preview answers a different question again: what "disco apply" would
land on the local branch, measured from where apply would start and with the
sandbox's uncommitted work included. Like --base local it needs the source's
local repository on this machine.

At a terminal the diff is rendered to be read — file headings, line numbers,
and the changed part of each line highlighted — and paged, like git's own diff.
The pager is DISCOBOX_PAGER, GIT_PAGER, PAGER, or less, and --no-pager prints
straight to stdout. Redirected, it is a plain unified patch that "git apply"
accepts, never paged. --patch is that patch at a terminal too, and --stat and
the other git output formats are git's own output, unrendered.

Most of "git diff"'s flags mean the same thing here and are passed through.
The ones that choose what to compare are not: the right-hand side is always
the sandbox's working tree, and the left is the base described above.`,
		Example: `  disco diff
  disco diff --stat
  disco diff sbx_01hq -- cli/ server/
  disco diff --base local
  disco diff --apply-preview
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
	flags.StringVar(&opts.base, "base", "", "Commit to diff against, instead of the one the source was cloned at; \"local\" names this machine's working tree and \"snapshot\" the dirty-workspace snapshot the sandbox started with")
	flags.BoolVar(&opts.noPager, "no-pager", false, "Print straight to stdout instead of paging, even at a terminal")
	flags.BoolVar(&opts.applyPreview, "apply-preview", false, "Show what applying this sandbox would land on the local branch: measured from where apply would start, with the sandbox's uncommitted work included")
	flags.StringArrayVar(&opts.dirOverrides, "dir", nil, "Local directory a source compares against for --apply-preview and --base local, as slug=path; required for a source with no known local directory on this machine")
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

	if opts.applyPreview && opts.base != "" {
		return fmt.Errorf("--apply-preview chooses the base itself; --base cannot also be given")
	}
	dirOverrides, err := parseDirOverrides(opts.dirOverrides)
	if err != nil {
		return err
	}

	stderr := cmd.ErrOrStderr()
	// Everything that depends on the terminal is decided against the real
	// stdout, before the pager replaces it: once output goes down a pipe there
	// is no width to measure and no terminal to ask about its background.
	view := newDiffView(cmd, opts)
	gitArgs := opts.gitArgs(cmd.Flags().Changed("unified"))
	// git colors its own output only when writing to a terminal, and under a
	// pager it is writing to a pipe. Ask for color explicitly, exactly as git
	// does for itself. The rendered view never wants this: it colors the patch
	// itself, and escape codes in the text would corrupt the parse.
	if view.paging && view.color && !view.render {
		gitArgs = append(gitArgs, "--color=always")
	}

	closePager, err := view.startPager(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := closePager(); err != nil {
			fmt.Fprintf(stderr, "pager: %v\n", err)
		}
	}()
	headings := view.headings(stderr)

	// Quitting the pager closes the pipe under us, so every write from that
	// point on fails with EPIPE. That is a reader who has seen enough, not a
	// command that went wrong: it ends the run without a failure and without
	// starting the next source, whose output nothing would read either.
	var failures []string
	fail := func(slug string, err error) bool {
		if isBrokenPipe(err) {
			return true
		}
		fmt.Fprintf(stderr, "source %s: %v\n", slug, err)
		failures = append(failures, slug)
		return false
	}

	for i, entry := range sources {
		if i > 0 && view.render {
			fmt.Fprintln(headings)
		}
		workdir := sourceWorkdir(entry.source)

		// These are the modes whose two sides start out on different machines,
		// so they produce the patch here rather than in the sandbox.
		if opts.applyPreview || opts.base == diffBaseLocalKeyword {
			patch, base, err := a.localSideDiff(ctx, projectID, sandboxID, sandbox, entry, dirOverrides, opts, gitArgs, pathspecs)
			if err != nil {
				if fail(entry.slug, err) {
					break
				}
				continue
			}
			fmt.Fprintf(headings, "source %s (%s) diffed from %s — %s\n\n",
				entry.slug, diffSourceLocation(entry.source), shortSHA(base.Commit), base.describe(""))
			if err := view.writePatch(patch, base.describe("")); err != nil && fail(entry.slug, err) {
				break
			}
			continue
		}

		base, upstreamRef, err := a.resolveDiffBase(ctx, projectID, sandboxID, workdir, entry.source, opts.base)
		if err != nil {
			if fail(entry.slug, err) {
				break
			}
			continue
		}
		// The base is always named, for one source as much as for several: a
		// diff means nothing without the commit it is measured from, and this
		// one is chosen rather than given.
		fmt.Fprintf(headings, "source %s (%s) diffed from %s — %s\n\n",
			entry.slug, diffSourceLocation(entry.source), shortSHA(base.Commit), base.describe(upstreamRef))

		command := sandboxDiffCommand(base.Commit, gitArgs, pathspecs)
		var runErr error
		if view.render {
			runErr = a.renderSandboxDiff(ctx, view, projectID, sandboxID, workdir, command, base.Commit)
		} else {
			runErr = a.streamSandboxDiff(ctx, cmd, view, projectID, sandboxID, workdir, command)
		}
		if runErr != nil && fail(entry.slug, runErr) {
			break
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d sources could not be diffed: %s", len(failures), len(sources), strings.Join(failures, ", "))
	}
	return nil
}

// diffView is every decision that depends on the terminal, made once, against
// the real stdout — before the pager replaces it. Measuring width or asking the
// terminal for its background once output goes down a pipe answers about the
// pipe, not the screen.
type diffView struct {
	// out is where output goes: the pager's input once one is running.
	out io.Writer
	// terminal is the real stdout, which is what the pager writes to.
	terminal io.Writer
	opts     diffOptions
	render   bool
	paging   bool
	color    bool
	dark     bool
	width    int
}

func newDiffView(cmd *cobra.Command, opts diffOptions) *diffView {
	out := cmd.OutOrStdout()
	view := &diffView{
		out:      out,
		terminal: out,
		opts:     opts,
		// The rendered view needs the whole diff before it can lay any of it
		// out, so it is the one that buys the text instead of streaming it.
		// Git's own formats stream, which is what a piped patch or a very large
		// diff wants.
		render: !opts.rawGitOutput() && isTerminalStream(out),
		paging: !opts.noPager && isTerminalStream(out),
		color:  diffColorEnabled(opts.color, out),
		width:  terminalWidth(out),
	}
	// Asking the terminal for its background is a round trip, so it is only
	// worth making when something is actually going to be colored.
	if view.render && view.color {
		view.dark = hasDarkTerminalBackground(cmd)
	}
	return view
}

func (v *diffView) startPager(ctx context.Context) (func() error, error) {
	out, done := startPager(ctx, v.terminal, v.paging)
	v.out = out
	return done, nil
}

// headings sends the per-source headings wherever they cannot corrupt the
// output: with a rendered diff they are part of the document, but a
// passed-through diff is a patch, and nothing but the patch may reach stdout.
func (v *diffView) headings(stderr io.Writer) io.Writer {
	if v.render {
		return v.out
	}
	return stderr
}

// writePatch emits patch text through whichever view is in effect.
func (v *diffView) writePatch(patch, baseDescription string) error {
	if !v.render {
		_, err := io.WriteString(v.out, patch)
		return err
	}
	files := diffrender.Parse(patch)
	if len(files) == 0 {
		fmt.Fprintf(v.out, "no differences from %s\n", baseDescription)
		return nil
	}
	return diffrender.Render(v.writer(), files, diffrender.Options{
		Width: v.width,
		Color: v.color,
		Dark:  v.dark,
	})
}

// writer wraps the output so styling degrades to what the terminal can actually
// show: the profile writer downsamples 256-color styling for a 16-color
// terminal and strips it for a dumb one, so the renderer never has to ask.
func (v *diffView) writer() io.Writer {
	if !v.color {
		return v.out
	}
	// The profile is detected from the real terminal, not from the pager pipe,
	// which would report no color at all.
	writer := colorprofile.NewWriter(v.terminal, os.Environ())
	if writer.Profile <= colorprofile.NoTTY {
		writer.Profile = colorprofile.ANSI256
	}
	writer.Forward = v.out
	return writer
}

// streamSandboxDiff runs the diff and passes git's bytes straight through, the
// same way `disco exec` does: nothing here has to understand the output, and a
// diff of any size costs no memory.
func (a *App) streamSandboxDiff(ctx context.Context, cmd *cobra.Command, view *diffView, projectID, sandboxID, workdir string, command []string) error {
	// A PTY is only asked for when this really is a terminal on all three
	// streams, so a redirected diff stays a plain patch; when there is one, git
	// colors its own output. Under a pager stdout is a pipe and there is no PTY
	// to ask for — git is told to color explicitly instead.
	tty := !view.paging && isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout()) && isTerminalStream(cmd.ErrOrStderr())
	body, err := createSandboxExecBody(sandboxExecCreateOptions{interactive: true, tty: tty, workdir: workdir}, command)
	if err != nil {
		return err
	}
	exec, err := a.createSandboxExec(ctx, projectID, sandboxID, body)
	if err != nil {
		return err
	}
	if err := a.attachSandboxExec(ctx, projectID, sandboxID, exec.ID, true, tty, cmd.InOrStdin(), view.out, cmd.ErrOrStderr()); err != nil {
		return err
	}
	// git has already said why on stderr, so the exit status is the whole
	// message the caller needs to add.
	return a.returnSandboxExecStatus(ctx, projectID, sandboxID, exec.ID)
}

// renderSandboxDiff runs the diff, then lays the patch out for reading.
func (a *App) renderSandboxDiff(ctx context.Context, view *diffView, projectID, sandboxID, workdir string, command []string, base string) error {
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
		fmt.Fprint(os.Stderr, errOutput)
	}
	return view.writePatch(patch, shortSHA(base))
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
	return []string{"sh", "-c", sandboxWorkingTreeScript + diff + "\n"}
}

// sandboxWorkingTreeScript leaves the sandbox's entire working state in $tree
// as a tree object. Shared by the commands that need it — the diff itself, and
// the commit `--base local` fetches — so the two cannot drift.
const sandboxWorkingTreeScript = `
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
`

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
