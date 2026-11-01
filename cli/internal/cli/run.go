package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/sourceref"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type runCommandOptions struct {
	lendGitCredentials bool
	prompt             sandboxcreate.PromptOptions
	// promptFlag is -p: the prompt as one argument. The words after `run` are
	// the same prompt as the shell split it.
	promptFlag []string
	detach     bool
	// source is SOURCE, the first positional argument, or the --from-json
	// request's "source": what the discobox is cut from. Empty asks.
	source string
	// noSource creates the discobox with nothing materialized in it. It is
	// still filed under the directory run ran in and listed there; it just is
	// not cut from it. With it every positional argument is the prompt.
	noSource bool
	// includeDirty is --include-dirty: carry uncommitted work on top of the
	// checked-out commit. Off, only the committed history goes in.
	includeDirty bool
	// declaredSources is the flag's positive form: the option it settles is
	// "skip them", because bringing them in is what declaring them asks for.
	declaredSources bool
	// raw attaches this terminal to the discobox's terminal directly, instead
	// of opening the window on it.
	raw bool
	// bind and delegate are --bind and --delegate as given, and bindings what
	// the create makes of them: parsed from them, or read from a --from-json
	// request. discoboxAPI is --discobox-api, or the request's "discoboxApi".
	bind        []string
	delegate    []string
	bindings    []apimodel.SandboxBindingInput
	discoboxAPI bool
	// pool is --pool as given: the pool the discobox is placed on, by ID,
	// short ID, or name. Empty is the project's default pool.
	pool string
	// skillDirs and userSkills are --skills and --user-skills as given, from
	// the command line or a --from-json request: nil for one that was not, which
	// takes what client.yaml says (newSkills). userSkillsFlag is where the
	// flag itself lands.
	skillDirs      []string
	userSkills     *bool
	userSkillsFlag bool
	// noProjectSkills is --no-project-skills: the project's skills sources
	// are left out of this create (ADR 26-10-14-894 §2).
	noProjectSkills bool
	// fromJSON is --from-json: the request is read from stdin instead of the
	// command line, and the created discobox printed as JSON (runJSONRequest).
	fromJSON bool
	// flags is every flag a run takes, which --from-json refuses beside it.
	flags *pflag.FlagSet
}

// newRunCommand builds `run`, the command that makes a discobox. `new` and
// `create` are aliases, and so are `r` and `n`, the short forms `run` and
// `new` have always had. Help lists every spelling: someone reaching for
// "make a discobox" types any of them, and none is going away.
func (a *App) newRunCommand() *cobra.Command {
	var opts runCommandOptions
	cmd := &cobra.Command{
		Use:     "run [flags] [SOURCE [PROMPT...]]",
		Aliases: []string{"new", "create", "r", "n"},
		Short:   runCommandShort,
		Long:    runCommandLong,
		Example: runCommandExample,
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runPrompt(cmd, &opts, args)
		},
	}
	addRunFlags(cmd, &opts)
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	return cmd
}

const runCommandShort = "Run a prompt in a new discobox"

const runCommandLong = `Launch a prompt in a new discobox cut from SOURCE.

SOURCE is the first argument: a directory (. for this one), a repository URL, or
GitHub OWNER/REPO, then optionally @REF and //PATH, the directory inside it the
harness starts in. The prompt is the words after it, or -p to give it as one
argument. Use -- when the words need to be separated from command flags
explicitly. This is the only command that makes a discobox: the bare "discobox"
takes no prompt and none of these flags.

Use - as the entire prompt to write it without shell escaping. At a terminal,
it opens a multiline composer: Enter submits, Shift-Enter adds a newline, Esc or
Ctrl-C cancels, and Alt-E opens $VISUAL/$EDITOR. With piped stdin, - reads the
prompt from stdin instead. An empty or whitespace-only prompt cancels the run.
Esc asks for confirmation before discarding a nonempty prompt.

By default run opens the console's window and makes the discobox there: a
question still open — where to cut from, whether to copy a directory in no
repository — is asked on it, the wait is drawn on it, and
what it lands on is the discobox itself — its default terminal (the configured
harness, or a shell when it has none), the shells and services running beside
it, and its forwarded ports. It is the same screen, in the same window, that
typing the same thing into "discobox console" gives you. Press Ctrl-A d to leave,
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

With no arguments at all (and no --no-source), run asks where to cut the
discobox from. In the window that is the same card Enter in the console opens,
with every other flag given already filled in; with -d or --raw it is an inline
picker. This directory is the first choice either way, and Esc creates nothing.
Without a terminal there is nobody to ask, and SOURCE is required.

Only committed history goes in by default: the discobox starts from the commit
checked out in SOURCE, and uncommitted changes there are left behind, with a
warning naming how many. --include-dirty carries them in as a snapshot on top of
that commit.

The source keeps its own path inside the discobox, so /home/you/src/app is
/home/you/src/app in there too. That holds for a directory under /home, /Users,
/mnt, /workspace, /Volumes, /media, /srv, /opt, /data or /var/home; everywhere
else the discobox's own system owns the path, so the source is placed at
/workspace/source instead and the discobox starts in the same subdirectory you
ran from. A source at one of those roots itself, or under /opt/discobox, is
placed that way too.

A source directory that is not in a Git repository works too: everything in it
is carried into the discobox as uncommitted changes on an empty first commit,
and nothing is written to the directory itself. There is no commit to start
from instead, so --include-dirty does not decide this one: at a terminal run
asks first, with the size it would copy counted while the question is on screen,
and copying is the default answer. Declining still creates the discobox, with
nothing checked out in it, exactly as --no-source does. Without a terminal the
directory is copied.

--no-source creates a discobox with nothing checked out in it, for work that
starts from an empty machine rather than from a repository. It takes SOURCE's
place, so every argument is the prompt. The directory you run in is still what
the discobox is filed under, so it is listed here like any other, and the Git
authorship it commits under is still read from here; only the source is left
out. -i still brings sources in, so --no-source -i ../foo is a discobox holding
foo and nothing else.

-i brings extra sources into the same discobox, repeat it for more than one. Each
is resolved exactly like SOURCE is, --include-dirty included,
and a local one keeps its own absolute path inside the discobox, so ../foo shows
up at the path readlink -f ../foo prints, under the same rule the source
directory follows above; one from anywhere else is placed under /workspace,
named after itself.

A repository can name the others it is worked on with, in .discobox/settings.json
at its root:

  {"sources": {"foo": "https://github.com/acme/foo"}}

Each is brought in the way -i would: the ../foo you already have checked out
when there is one, and a clone of the URL when there is not. Either way it lands
beside the source -- at the same path when the source kept its own, under
/workspace when it did not -- so ../foo means the same thing inside the
discobox as it does here. --declared-sources=false leaves them out.

--skills DIR installs skills in the discobox: every subdirectory of DIR that
holds a SKILL.md is one skill, named after it, and is copied into the harness's
skill directories on its first launch. Repeat it for more than one directory.
--user-skills does the same with your own ~/.claude/skills and
~/.agents/skills, read before any --skills. A skill of the same name in a later
directory replaces the earlier one, and a discobox's skills replace the image's
and the repository's of the same name. Links are followed and .git is left
out; the skills are carried in the request, at most 1 MiB in all.

--skills URL[@REF][//PATH] is a Git repository instead: it is checked out under
~/.discobox/skill-sources in the discobox, and every subdirectory of PATH
(the repository's root without one) that holds a SKILL.md is linked into the
skill directories, not copied, so an edit to a skill lands in the checkout and
can be committed and pushed back. Several --skills naming one repository at one
ref share its checkout. A linked skill replaces a copied one of the same name,
whatever order they were given in.

Either can be made the default for every new discobox, in the console too, in
client.yaml under <XDG config home>/discobox (or wherever
DISCOBOX_CLIENT_CONFIG_FILE names): run.skills and run.userSkills. When
that variable is unset and <XDG config home>/discobox holds no client.yaml,
the client writes client.example.yaml there, listing every setting. --skills
given replaces run.skills rather than adding to it, and --user-skills=false
turns run.userSkills off.

-s stores a secret of its own and binds it as KEY, read from where it already
is: KEY takes $KEY from this shell, KEY=@FILE reads FILE (one trailing newline
dropped). KEY=VALUE takes the value itself, which then sits in shell history
and the process list. It is sent anywhere unless @HOST follows: KEY@HOST,
KEY=@FILE@HOST, KEY=VALUE@HOST send it to HOST alone, which is exact ('*.HOST'
for the hosts beneath it).

--bind binds a project secret to the new discobox, SECRET[:VAR][@ROUTE,...]:
the discobox receives a sentinel in VAR — the secret's name upper-snaked when
left out, so github is GITHUB — which its pool swaps for the value where the
secret's routes allow, narrowed by any ROUTE given — each a host, exact,
'*.suffix' for the hosts beneath suffix or '*' for any, optionally =blocked —
as Copilot CLI's GitHub token is:
'github:COPILOT_GITHUB_TOKEN@api.github.com,*.githubcopilot.com'. Policy files supply reusable routes, with their own arguments:
--bind 'github,policy=issues.json,owner=acme,repo=widget'. Repeat policy=FILE
within one binding to merge routes; each following NAME=VALUE belongs to
that file. Policy files and inline @ROUTE values are mutually exclusive. A route
with methods, paths or a protocol is also bound with 'discobox secret bind
--route'. --delegate
SECRET[@ROUTE,...] lets the new discobox give the secret to the discoboxes it
creates instead. In --from-json, "bindings" lists them. A discobox creating one
binds only what it may delegate, and delegates nothing.

--discobox-api lets the new discobox call the discobox API through its pool,
acting only on the discoboxes it creates. A discobox creating one may set it
only when it has it itself.

--from-json reads the whole request from stdin as one JSON object instead,
creates the discobox without attaching, and prints it as JSON. It is for a
caller that would rather not quote a prompt through a shell — an agent in a
discobox making another, above all. "source" is SOURCE, required
unless "noSource" is set; every other field is optional, and each means what
its flag does. No argument or other run flag may be given beside it:

  {
    "source": "https://github.com/org/repo",
    "prompt": "fix the failing tests in pkg/foo",
    "harness": "codex",
    "bindings": [
      {"secret": "npm", "envVar": "NPM_TOKEN", "routes": ["registry.npmjs.org"]},
      {"secret": "github", "delegate": true}
    ],
    "discoboxApi": false,
    "env": ["MODE=test"],
    "secrets": ["OPENAI_API_KEY=sk-...@api.openai.com"],
    "include": ["../foo"],
    "skills": ["./team-skills"],
    "userSkills": true,
    "noSource": false,
    "includeDirty": true,
    "declaredSources": true
  }`

const runCommandExample = `  discobox run . 'fix the failing tests'
  discobox run . -
  cat prompt.md | discobox run . -
  discobox run -H codex -d . 'fix the failing tests'
  discobox run ../api -p 'fix the failing tests'
  discobox run org/repo@main 'fix the failing tests'
  discobox run --include-dirty . 'finish what I started'
  discobox run . -i ../foo -i ../bar 'make them share one client'
  discobox run --user-skills --skills ./team-skills . 'fix the failing tests'
  discobox run --no-source 'draft a proposal for the new pricing page'
  discobox run -e GITHUB_TOKEN -e MODE=test . 'fix the failing tests'
  discobox run -s OPENAI_API_KEY@api.openai.com -s 'NPM_TOKEN=@~/.npm-token' . 'fix the failing tests'
  discobox run --pool mac-mini . 'fix the failing tests'
  discobox run -d --bind github:GH_TOKEN . 'fix issue 42'
  discobox run -d --bind npm-token --delegate github . 'fan out the issues'
  echo '{"source": ".", "prompt": "fix the failing tests"}' | discobox run --from-json
  discobox run --raw . 'fix the failing tests'
  discobox run . fix the failing tests
  discobox run . -- prompt starting with --flag-like text
  discobox run`

// runPrompt creates the discobox a run describes.
//
// args are the positional arguments: SOURCE and then the prompt as the shell
// split it, after -p, which is the same prompt given as one argument. With
// --no-source there is no SOURCE, and every one is the prompt. Which is which
// is decided by position and the flag alone, never by what an argument looks
// like.
func (a *App) runPrompt(cmd *cobra.Command, opts *runCommandOptions, args []string) error {
	if opts.fromJSON {
		if err := opts.readJSONRequest(cmd, args); err != nil {
			return err
		}
		args = nil
	} else {
		if !opts.noSource && len(args) > 0 {
			source, err := runSourceArgument(args[0])
			if err != nil {
				return err
			}
			opts.source, args = source, args[1:]
		}
		opts.prompt.IncludeDirty = sandboxcreate.IncludeDirtyNever
		if opts.includeDirty {
			opts.prompt.IncludeDirty = sandboxcreate.IncludeDirtyAlways
		}
		bindings, err := sandboxcreate.ParseBindings(opts.bind, opts.delegate)
		if err != nil {
			return err
		}
		opts.bindings = bindings
		if opts.flags.Changed("user-skills") {
			opts.userSkills = &opts.userSkillsFlag
		}
	}
	// What client.yaml says about skills is settled here, whether or not a
	// window is about to open: a file that does not parse is this command's
	// error to report, on this terminal, like a flag that does not.
	var err error
	if opts.prompt.SkillDirs, opts.prompt.UserSkills, err = newSkills(opts.skillDirs, opts.userSkills); err != nil {
		return err
	}
	// Creating and delivering a source are this client's own work, so
	// nothing but this process can say which of them is underway
	// (ADR 0060). The line comes back down before anything else is
	// written to stderr — and everything else this command writes there
	// while it is up goes through it, since it owns the row it is on.
	status := newStatusLine(cmd.ErrOrStderr())
	defer status.clear()
	// Where a line that is not a phase of the wait goes: what a declared source
	// resolved to, which SSH key was enrolled, which ssh_config was written.
	//
	// A run that ends in an attach hands this terminal to the discobox's own
	// full-screen program, and nothing may be left standing above it: those
	// rows are content that program never drew and will not redraw, so what it
	// paints lands shifted off the screen it believes it has. Such a run notes
	// them on the status line, which takes its row back before the terminal
	// starts. -d returns to the shell instead, where the scrollback is exactly
	// where they belong.
	notes := noteFunc(status.note)
	if opts.detach {
		notes = status.print
	}
	// -p and the words after `run` are the same prompt, so a caller can use
	// whichever the shell makes easier and both arrive as argv tokens. The flag
	// leads because it is the one that had to be quoted.
	prompt := append(append([]string(nil), opts.promptFlag...), args...)
	// JSON prompts are literal data, including a prompt containing only "-".
	if !opts.fromJSON && len(prompt) == 1 && prompt[0] == "-" {
		text, err := readRunPrompt(cmd)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return errPickCanceled
		}
		prompt = []string{text}
	}
	// A run that named no source is asked one wherever there is somebody to
	// ask: on the window's create card, or — for -d and --raw at a terminal —
	// inline. Without anybody, SOURCE was required.
	askSource := opts.source == "" && !opts.noSource
	window := !opts.detach && !opts.raw && canOpenWindow(cmd)
	if askSource && (opts.fromJSON || !window && !canAskInline(cmd)) {
		return errRunNeedsSource
	}
	// Until it is asked, the flags are checked against this directory: the
	// answer the question leads with.
	opts.prompt.Source = opts.source
	if askSource {
		opts.prompt.Source = "."
	}
	opts.prompt.NoSource = opts.noSource
	// A JSON request came in on stdin, so there is nobody on it to ask: a
	// directory in no repository is copied.
	if !opts.fromJSON {
		opts.prompt.ConfirmCopyDirectory = confirmCopyDirectory(cmd, status)
	}
	opts.prompt.SkipDeclaredSources = !opts.declaredSources
	opts.prompt.ReportDeclaredSource = reportDeclaredSource(notes)
	opts.prompt.Warn = func(warning string) { notes("%s", warningLine(warning)) }
	opts.prompt.ReportInlineSecret = func(line string) { notes("%s", terminalSafe(line)) }
	parsedOpts, err := sandboxcreate.ParsePromptOptions(opts.prompt, prompt)
	if err != nil {
		return err
	}
	// The window is where a run goes unless something says otherwise,
	// and it is given the request rather than the discobox: the create
	// happens inside it, so a question still open — the source, or copying a
	// directory in no repository — is the
	// window's own dialog and the wait is its own screen rather than a
	// list with a busy line under it. The flags are parsed above
	// either way — a flag that contradicts itself is this command's
	// error to report, on this terminal, before a window opens over it.
	//
	// -d has nothing to open a window for: it prints the discobox and
	// returns, and printing is stdout, which the window would be
	// sitting on.
	if window {
		return a.runConsole(cmd, "", tui.WithRun(a.runWindowRequest(opts, prompt), askSource))
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	if askSource {
		source, noSource, err := askRunSource(cmd, client, projectID, status)
		if err != nil {
			return err
		}
		opts.prompt.Source, opts.prompt.NoSource = source, noSource
		if parsedOpts, err = sandboxcreate.ParsePromptOptions(opts.prompt, prompt); err != nil {
			return err
		}
	}
	parsedOpts.Bindings = opts.bindings
	parsedOpts.DiscoboxAPI = opts.discoboxAPI
	if parsedOpts.PoolID, parsedOpts.Platform, err = a.newSandboxPlacement(cmd.Context(), client, projectID, opts.pool); err != nil {
		return err
	}
	if !opts.noProjectSkills {
		if parsedOpts.ProjectSkills, err = projectSkills(cmd.Context(), client, projectID); err != nil {
			return err
		}
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
	err = sandboxcreate.DeliverSource(cmd.Context(), client, projectID, sandbox, local, gitServerURL, a.token, report, credentialDeliveryOptions(cmd, status, opts.lendGitCredentials))
	releaseGitServerURL()
	local.Close()
	if err != nil {
		return err
	}
	// Still under the status line, and named on it: the sync generates a key,
	// enrolls it and rewrites two files, which is a wait like any other and the
	// last thing between the create and the terminal. The window narrates the
	// same step in the same words. The line comes down before either of the two
	// things that follow — printing the discobox, or attaching to it.
	status.set("syncing SSH config")
	err = a.syncSSHConfigAfterCreate(cmd.Context(), client, projectID, notes)
	status.clear()
	if err != nil {
		return err
	}
	if opts.fromJSON {
		return writeJSON(cmd.OutOrStdout(), sandbox)
	}
	if opts.detach {
		return a.writeSandbox(cmd, sandbox)
	}
	return a.attachRunSandbox(cmd, projectID, sandbox)
}

// addRunFlags gives cmd everything a run takes, and keeps the set in opts for
// --from-json to check against.
func addRunFlags(cmd *cobra.Command, opts *runCommandOptions) {
	flags := pflag.NewFlagSet("run", pflag.ContinueOnError)
	flags.BoolVar(&opts.lendGitCredentials, "lend-git-credentials", false, "Allow lending Git HTTPS credentials to the pool for one source pull")

	flags.StringArrayVarP(&opts.promptFlag, "prompt", "p", nil, "Prompt for the harness, as one argument; repeat to pass more argv tokens. The same thing as the words after SOURCE")
	flags.StringArrayVarP(&opts.prompt.Env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables. A KEY whose name contains KEY, TOKEN, PASS, or SECRET is treated as a secret; use KEY!=VALUE to force it to be a plain environment variable")
	flags.StringArrayVarP(&opts.prompt.Secret, "secret", "s", nil, "A secret to store and bind as KEY: KEY reads $KEY from this shell, KEY=@FILE reads a file, KEY=VALUE takes the value itself (it then sits in shell history and the process list); add @HOST to send it to HOST alone, else it is sent anywhere. A value or path holding an @ ends with one. Repeat for more. A stored secret is bound with --bind")
	flags.StringArrayVarP(&opts.prompt.Include, "include", "i", nil, "Additional source to bring into the discobox, DIR, URL or GitHub OWNER/REPO, optionally with @REF; repeat for more than one. A local directory keeps its own absolute path inside the discobox where the discobox can hold that path, and is placed under the discobox's working root (/workspace on Linux) where it cannot; either way it is named after itself, so -i ../foo is the source foo")
	flags.StringArrayVar(&opts.skillDirs, "skills", nil, "Skills to install in the discobox: a directory, whose subdirectories holding a SKILL.md are each copied in as a skill, or a repository, URL or GitHub OWNER/REPO, then [@REF][//PATH], checked out under ~/.discobox/skill-sources with each skill in PATH linked in, so an edit can be pushed back; repeat for more than one. A later directory wins on a skill of the same name, and every one wins over --user-skills, but a linked skill wins over a copied one. Given, it replaces client.yaml's run.skills")
	flags.BoolVar(&opts.noProjectSkills, "no-project-skills", false, "Leave the project's skills repositories (discobox skills) out of this discobox. Otherwise they are added ahead of any --skills, which win on a skill of the same name")
	flags.BoolVar(&opts.userSkillsFlag, "user-skills", false, "Install your own skills from ~/.claude/skills and ~/.agents/skills in the discobox, before any --skills. Defaults to client.yaml's run.userSkills")
	flags.StringVarP(&opts.prompt.Harness, "harness", "H", "", "Harness config to run, by slug (e.g. codex), name, or ID; defaults to the project default")
	flags.StringVar(&opts.pool, "pool", "", "Pool to create the discobox on, by ID or name; defaults to the project's default pool. The discobox runs on the platform the pool hosts, and every path placed in it is that platform's")
	flags.BoolVarP(&opts.detach, "detach", "d", false, "Create the discobox and print it without attaching to its terminal")
	flags.BoolVar(&opts.raw, "raw", false, "Create the discobox here and attach this terminal straight to its terminal, instead of making it in the window")
	flags.BoolVar(&opts.noSource, "no-source", false, "Create the discobox with nothing checked out in it, in place of SOURCE, so every argument is the prompt; the directory you run in still decides where it is filed and what Git authorship it commits under")
	flags.BoolVar(&opts.declaredSources, "declared-sources", true, "Bring in the sources the repository declares in .discobox/settings.json, using a local checkout beside the source directory when there is one")
	flags.BoolVar(&opts.includeDirty, "include-dirty", false, "Carry the uncommitted changes in each local source into the discobox, as a snapshot on top of its checked-out commit; without it only committed history goes in. A directory in no Git repository has no commit to start from, and is copied whatever this says")
	flags.StringArrayVar(&opts.bind, "bind", nil, "Bind a project secret to the new discobox, as SECRET[:VAR][@ROUTE,...]: the variable it receives the sentinel in (default the secret's name upper-snaked), and routes that narrow where it may be sent; or SECRET[:VAR],policy=FILE,NAME=VALUE (repeat policy=FILE to merge files, arguments follow each file). Repeat for more. A discobox creating one binds only what it may delegate")
	flags.StringArrayVar(&opts.delegate, "delegate", nil, "Let the new discobox give a project secret to the discoboxes it creates, as SECRET[@ROUTE,...]; repeat for more. A person's to give")
	flags.BoolVar(&opts.discoboxAPI, "discobox-api", false, "Let the new discobox call the discobox API through its pool, acting only on the discoboxes it creates. A discobox creating one may set it only when it has it itself")
	flags.BoolVar(&opts.fromJSON, "from-json", false, "Read the request from stdin as one JSON object instead of from flags, create the discobox without attaching, and print it as JSON")
	cmd.Flags().AddFlagSet(flags)
	opts.flags = flags
}

// errRunNeedsSource is a run with no SOURCE and nobody to ask for one.
var errRunNeedsSource = errors.New("run needs SOURCE, what to cut the discobox from: a directory (. for this one), a repository URL or OWNER/REPO; or --no-source")

// runSourceArgument checks SOURCE before anything else reads it, and spells a
// GitHub OWNER/REPO the way every later step expects. A directory that does
// not exist is refused here, with the reminder that SOURCE comes first: it is
// what `discobox run 'fix the tests'`, written the way run used to take a
// prompt, turns into.
func runSourceArgument(arg string) (string, error) {
	source, err := sandboxcreate.ParseSource(arg)
	if err != nil {
		return "", err
	}
	if source.Kind == sourceref.Git {
		return source.String(), nil
	}
	dir := sandboxcreate.SourceDirectory(source)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("SOURCE %q: %w\nSOURCE comes first, before the prompt: discobox run . %s", arg, err, shellQuote(arg, runtime.GOOS == "windows"))
	}
	return arg, nil
}

// noSourceChoice is the source picker's id for --no-source. A picker's id is
// the column it draws beside the title, and each id there is what the command
// line would say instead — SOURCE, or this flag — which no path or URL can
// collide with.
const noSourceChoice = "--no-source"

// askRunSource is the create card's question asked inline, for a `run` that
// named no source and has no window to put the card in (-d, --raw): this
// directory, every other source the project has been cut from, or none. A
// place the project has never been cut from is given as SOURCE, which the
// prompt says.
func askRunSource(cmd *cobra.Command, client *apiclientgen.Client, projectID string, status *statusLine) (string, bool, error) {
	here, err := filepath.Abs(".")
	if err != nil {
		return "", false, err
	}
	items := []pickerItem{{id: ".", title: "this directory", detail: here}}
	sources, err := projectSources(cmd.Context(), client, projectID)
	if err != nil {
		return "", false, err
	}
	for _, source := range sources {
		if source.value == here {
			continue
		}
		detail := ""
		if source.remote {
			detail = "cloned by the discobox itself"
		}
		items = append(items, pickerItem{id: source.value, title: sourceTitle(source), detail: detail})
	}
	items = append(items, pickerItem{id: noSourceChoice, title: "no source", detail: "nothing checked out — an empty discobox"})
	defer status.suspend()()
	choice, err := pickOne(cmd, "Where should this discobox be cut from? (pass SOURCE to name another)", items, pickerOptions{
		empty:     "no choice to make",
		ambiguous: "pass SOURCE or --no-source",
	})
	if err != nil {
		return "", false, err
	}
	if choice == noSourceChoice {
		return "", true, nil
	}
	return choice, false, nil
}

// sourceTitle is what a source is called on the picker, its SOURCE value being in
// the column beside it: a directory's last element, or a repository URL's
// owner and name.
func sourceTitle(source runSource) string {
	value := strings.TrimSuffix(strings.TrimSuffix(source.value, "/"), ".git")
	if source.remote {
		parts := strings.Split(value, "/")
		if len(parts) >= 2 {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1]
		}
		return value
	}
	return filepath.Base(value)
}

// runSource is one place the project's discoboxes were cut from.
type runSource struct {
	value  string
	remote bool
}

// projectSources is what the project's discoboxes were cut from, newest first, each once, spelled the way SOURCE takes it: the URL a
// remote source clones, or the client directory a local one was cut from. It
// is the list the create card offers, read the same way (toTUISandbox).
func projectSources(ctx context.Context, client *apiclientgen.Client, projectID string) ([]runSource, error) {
	res, err := client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](res)
	if err != nil {
		return nil, err
	}
	sandboxes := sortedByRecency(body.GetSandboxes(), func(sb apimodel.Sandbox) time.Time { return sb.CreatedAt })
	seen := map[string]bool{}
	var out []runSource
	for _, sb := range sandboxes {
		source, ok := sb.Config.Source.Get()
		if !ok {
			continue
		}
		next := runSource{value: strings.TrimSpace(source.LocalDirectory.Or(""))}
		if url, ok := source.URL.Get(); ok {
			next = runSource{value: url.String(), remote: true}
		}
		if next.value == "" || seen[next.value] {
			continue
		}
		seen[next.value] = true
		out = append(out, next)
	}
	return out, nil
}

// confirmCopyDirectory asks whether a source directory that is in no Git
// repository is copied into the sandbox. Everything in such a directory is
// uncommitted work, so the question is the whole directory, and the size is
// why it is asked at all: a home directory run in by accident. Copying leads,
// because there is no commit to start from instead and the content is what
// naming the directory asks for (ADR 26-10-23-600 §4). Declining creates the
// discobox with nothing checked out in it, the way --no-source does (ADR 0077
// §1).
//
// The size is counted behind the question rather than before it, so the prompt
// comes up immediately and fills its number in as the walk finds it.
//
// Without a terminal there is nobody to ask and the directory is copied, which
// is what a source directory has always meant here; the alternative is a
// discobox that silently came up with nothing in it.
func confirmCopyDirectory(cmd *cobra.Command, status *statusLine) sandboxcreate.ConfirmCopyDirectoryFunc {
	return func(_ context.Context, directory sandboxcreate.DirectoryCopy) (bool, error) {
		if !canAskInline(cmd) {
			return true, nil
		}
		defer status.suspend()()
		live := func() (string, bool) {
			total := directory.Size.Total()
			return directoryCopyPrompt(directory.Dir, total), total.Done
		}
		prompt, _ := live()
		// Copying leads: there is no commit to start from instead, so the
		// directory's content is what running against it asks for, and the
		// size above is the reason to ask at all.
		choice, err := pickOne(cmd, prompt, []pickerItem{
			{
				id:     "copy",
				title:  "Copy the directory in",
				detail: "Everything in it arrives as uncommitted changes",
			},
			{
				id:     "exclude",
				title:  "Do not copy the directory",
				detail: "Create the discobox with nothing checked out in it",
			},
		}, pickerOptions{
			empty:     "no choice to make",
			ambiguous: "pass --no-source to leave it out",
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
// It goes through the run's note sink rather than to the stream directly,
// because this runs while the create is being narrated on that same stream and
// the narration owns the row it is on. Written past it, each of these lines came
// out with the spinner on the front of it — and a run that ends in an attach
// cannot leave them on the screen at all. See notes in runPrompt.
func reportDeclaredSource(notes noteFunc) sandboxcreate.ReportDeclaredSourceFunc {
	return func(source sandboxcreate.DeclaredSource) {
		notes("%s", declaredSourceLine(source))
	}
}

// declaredSourceLine is that report as one line. It is shared with the window,
// which narrates the same line on its busy line rather than printing it: the
// two frontends say the same thing about the same source, in the form each of
// them has for saying it.
func declaredSourceLine(source sandboxcreate.DeclaredSource) string {
	switch {
	case !source.Local:
		if source.Checkout == "" {
			return fmt.Sprintf("source %s: using remote %s", source.Name, source.URL)
		}
		return fmt.Sprintf("source %s: using remote %s (no checkout at %s)",
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

// warningLine is a create's warning as one line, shared with the window the
// way declaredSourceLine is. A warning can carry what a repository's own
// files say, which a discobox may have written, so it is escaped (cli/REVIEW.md).
func warningLine(warning string) string {
	return "warning: " + terminalSafe(warning)
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
		LendGitCredentials:  opts.lendGitCredentials,
		Prompt:              prompt,
		Source:              opts.source,
		NoSource:            opts.noSource,
		Harness:             opts.prompt.Harness,
		Env:                 opts.prompt.Env,
		Secret:              opts.prompt.Secret,
		Bind:                opts.bind,
		Delegate:            opts.delegate,
		DiscoboxAPI:         opts.discoboxAPI,
		Include:             opts.prompt.Include,
		SkillDirs:           opts.skillDirs,
		UserSkills:          opts.userSkills,
		NoProjectSkills:     opts.noProjectSkills,
		SkipDeclaredSources: !opts.declaredSources,
		Pool:                opts.pool,
	}
	// The flag is an answer already given, either way, so the window does not
	// ask about uncommitted work and hands it to the create for every source
	// it cuts from, as this command does. The copy question it still asks.
	req.IncludeDirty = "false"
	if opts.includeDirty {
		req.IncludeDirty = "true"
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
	err := a.attachSandboxTerminal(ctx, projectID, sandbox.ID, primaryExecID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	reportRunSandbox(cmd.ErrOrStderr(), sandbox.ID, sandbox.DisplayName)
	return err
}

// reportRunSandbox names the discobox a run made and attached to, once the
// attach is over. Whatever the attach drew — a full-screen harness, or the
// window — is gone from the terminal by then, and with it the only place the
// discobox was named, so somebody who detached would otherwise be left not
// knowing which one is still running for them.
//
// It says what was made and nothing about what to do with it: the discobox
// may have been archived from the window, the attach may have failed, and a
// command to return to it would have to repeat the --server and --project
// this one was given.
func reportRunSandbox(w io.Writer, id, name string) {
	if name = strings.TrimSpace(name); name != "" && name != id {
		fmt.Fprintf(w, "Created discobox %s (%s)\n", id, terminalSafe(name))
		return
	}
	fmt.Fprintf(w, "Created discobox %s\n", id)
}
