package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/keys"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/health"
	"github.com/discobox-ai/discobox/serverstage"
	"github.com/discobox-ai/discobox/version"
)

const defaultProjectAlias = "default"

type App struct {
	serverURL     string
	irohRelayURLs string
	irohLogLevel  string
	projectID     string
	source        string
	token         string
	output        string
	quiet         bool
	debug         bool
	autoStart     autoStartServer
	errOut        io.Writer

	// serverSource is where the server binary comes from, as far as the flags
	// say. `admin server` is what sets it; everything else, the autolaunch
	// included, resolves from the environment and the machine.
	serverSource serverSource

	// leaderKey is the prefix key this invocation reserves in a terminal it
	// shows: the launcher's window commands, and the detach chord of an attach.
	// validate resolves it from the environment, so it is empty until then —
	// read it through leader() rather than directly.
	leaderKey string

	// autoLaunchOnce guards the one autolaunch attempt this invocation gets.
	// See ensureLocalServerOnce.
	autoLaunchOnce sync.Once

	// pushCache is what an automatic push resolves to per discobox, guarded by
	// pushMu: which of its sources this machine may push and the repository
	// each one is pushed from, none of which can change while the discobox
	// exists. Every attached client in this invocation shares it. See
	// push_auto.go.
	pushMu    sync.Mutex
	pushCache map[string]*pushTargets
	// pushInFlight counts transfers that have started and cannot be
	// interrupted, so a front end can wait for them on the way out rather than
	// ending one between receive-pack and the lease that guards it. See
	// App.waitForPushes.
	pushInFlight sync.WaitGroup

	// startedServer records that this invocation launched the server, which is
	// what makes it the one responsible for showing first-run setup.
	startedServer bool
	// stagingShownByUI is set by a front end that reports that setup itself, so
	// the launch path does not block on it.
	stagingShownByUI bool
	// runsAnImage is set by a command that is going to make a discobox run an
	// image, which is the only kind worth holding while a first run stages
	// them. See waitsOutFirstRunStaging.
	runsAnImage bool
}

// commandName is the name this binary was invoked as, which is what every
// example, usage line, and generated completion script calls it.
//
// There is more than one, because Homebrew refuses to link two formulae that
// install the same file: the latest channel installs the same binary as
// `discobox-dev` so it can sit beside a stable `discobox` (ADR 0105). One
// build, two names, and neither is knowable until it runs — so this reads
// argv[0] rather than a linker stamp.
//
// Anything that is not a discobox name is one: a `go test` binary, `go run`'s
// temporary file, a copy someone renamed. Those get the real name back, so that
// output stays what a reader expects and does not depend on where the binary
// happens to live.
func commandName() string {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if name == "discobox" || strings.HasPrefix(name, "discobox-") {
		return name
	}
	return "discobox"
}

func NewRootCommand() *cobra.Command {
	cmd, _ := newRootCommand()
	return cmd
}

// newRootCommand is NewRootCommand with the App it wired, so a test can ask the
// app about the tree it is part of.
func newRootCommand() (*cobra.Command, *App) {
	cobra.EnableCommandSorting = false

	app := &App{autoStart: autoStartServerAuto}
	var run runCommandOptions
	var showVersion bool
	var runFlags *pflag.FlagSet
	name := commandName()
	cmd := &cobra.Command{
		Use:   name + " [flags]",
		Short: "Discobox command line client",
		Long: fmt.Sprintf(`Discobox runs coding agents in isolated sandboxes on this machine.

Given a prompt, or any of the flags a run takes, this is "%[1]s run": the
command name can be left out of the thing you do most.

  %[1]s -p 'fix the failing tests'
  %[1]s -H codex -d -p 'fix the failing tests'

The prompt is -p here, and only -p: a word on its own is still a subcommand, so
a misspelled one says so rather than quietly becoming a prompt.

With nothing at all it opens the launcher, where the same run is one prompt and
an Enter. See "%[1]s run --help" for what the flags below mean.`, name),
		SilenceUsage:  true,
		SilenceErrors: true,
		// The flags in front of a subcommand are parsed by the command they
		// were written on, which is the only way the ones whose own flag
		// parsing is off ever see them. cobra's default hands every flag,
		// wherever it stood, to the command it finally found; for `cp` and
		// `tools ssh` that command parses none of them, so
		// `discobox --server ... cp` silently talked to the default endpoint
		// instead — and cannot simply parse them itself, since after the
		// command name they are scp's and ssh's own (`-o` is one flag on this
		// CLI and another on both of those).
		TraverseChildren: true,
		// What rootArgs suggests a misspelled command from. Cobra fills this
		// in when it makes the suggestions itself, which under
		// TraverseChildren it does not.
		SuggestionsMinimumDistance: 2,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			app.errOut = cmd.ErrOrStderr()
			// --version is answered whatever else the environment gets wrong,
			// for the reason the version command skips this hook altogether.
			if showVersion && cmd == cmd.Root() {
				return nil
			}
			if err := refuseRootOnlyArguments(cmd, args, runFlags); err != nil {
				return err
			}
			if err := app.validate(); err != nil {
				return err
			}
			// Every command, not just the ones a pane runs: which command a
			// pane is showing is the launcher's business, and a watch nobody
			// asked for is a no-op (watchParentProcess).
			cmd.SetContext(watchParentProcess(cmd.Context(), app.errOut))
			return nil
		},
		// The words a bare `discobox` is given are refused, so nothing below
		// has any to consider.
		Args: rootArgs,
		// Any of run's own flags makes this command a run: it is the one thing
		// anybody does often enough to resent typing the name of, and
		// `discobox` is already what you type to reach the same run in the
		// launcher.
		//
		// Bare `discobox` at a terminal opens the launcher: it is the one thing
		// you can ask for without knowing a subcommand, and typing the name of
		// a program is how you ask for it. Anywhere else — a pipe, a script,
		// CI — it prints its help, because a full-screen window is not an
		// answer to a program that expected output. A run says where its output
		// goes for itself, so this only covers the launcher.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showVersion {
				return app.printVersion(cmd)
			}
			if runRequested(runFlags) {
				return app.runPrompt(cmd, &run, nil)
			}
			if !isTerminalStream(cmd.OutOrStdout()) || !isTerminalStream(cmd.InOrStdin()) {
				return cmd.Help()
			}
			// No leader override here: validate already took the environment's,
			// and --leader is the tui command's own. A flag would have to be a
			// persistent one to be reachable from a bare `discobox`, and every
			// subcommand would then carry a flag that means nothing to it.
			return app.runTUI(cmd, "")
		},
	}
	cmd.PersistentFlags().StringVar(&app.serverURL, "server", envOrDefault("DISCOBOX_SERVER", endpoint.DefaultEndpoint()), "Discobox API server endpoint")
	// A client has to be told the same relays as the server it dials. An
	// address carries a peer ID and nothing else, so a server moved off n0's
	// public relays does not move its clients with it (ADR 0096 §6 on server
	// configuration).
	cmd.PersistentFlags().StringVar(&app.irohRelayURLs, "iroh-relay", envOrDefault("DISCOBOX_IROH_RELAY_URLS", ""), "Comma-separated iroh relay servers to use instead of the public ones; must match the server's")
	// An iroh connection fails in layers and reports only the top one. This
	// prints each layer as it happens — the socket, the relay, the dial, the
	// stream — plus iroh's own tracing at the same level. `discobox admin
	// server status` answers the same question after the fact; this one answers
	// it for the command that is failing.
	cmd.PersistentFlags().StringVar(&app.irohLogLevel, "iroh-log", envOrDefault(endpoint.IrohLogEnv, ""), "Log the iroh transport as it connects: off, error, warn, info, debug, or trace")
	// Long form only: -p is the prompt, on this command and on run, because a
	// prompt is what somebody typing `discobox -p ...` means every time and a
	// project is what a script names in full.
	cmd.PersistentFlags().StringVar(&app.projectID, "project", envOrDefault("DISCOBOX_PROJECT", defaultProjectAlias), "Project ID for this invocation; use default for the user's default project")
	// Advanced: everything a project is for lives under `discobox admin`, and a
	// user with one project — which is everyone until they make a second —
	// gains nothing from a flag on every command's help. It keeps working for
	// the scripts and the launcher that pass it; it just stops being offered.
	_ = cmd.PersistentFlags().MarkHidden("project")
	cmd.PersistentFlags().StringVarP(&app.source, "chdir", "C", ".", "Source directory or Git repository to act on, optionally with @REF: what run cuts a discobox from, and what ls lists this machine's discoboxes for")
	// Beta: the flag works but is undocumented until the source-selection UX is
	// settled, so it stays out of help text and examples.
	_ = cmd.PersistentFlags().MarkHidden("chdir")
	cmd.PersistentFlags().StringVar(&app.token, "token", os.Getenv("DISCOBOX_TOKEN"), "Bearer token for API requests")
	cmd.PersistentFlags().StringVarP(&app.output, "output", "o", "table", "Output format: table or json")
	cmd.PersistentFlags().BoolVar(&app.debug, "debug", false, "Print HTTP requests made by the API client, and the git commands run on this machine")
	cmd.PersistentFlags().Var(&app.autoStart, "auto-start-server", "Whether to start a local server when the endpoint is unavailable: true, false, or auto (the build and environment default, off for a development build)")
	cmd.PersistentFlags().Lookup("auto-start-server").NoOptDefVal = string(autoStartServerTrue)
	// The root's own flag rather than the one cobra adds for a Version: cobra
	// prints that from a template, and the server's half of the answer is a
	// request. The spelling is cobra's, -v included, so nothing that asked
	// before has to ask differently now.
	cmd.Flags().BoolVarP(&showVersion, "version", "v", false, "Print the client and server versions")
	_ = cmd.RegisterFlagCompletionFunc("project", app.completeProjects)
	// The run's own flags, on the command that stands in for it. They are local
	// rather than persistent, so `discobox ls --help` is still a list's flags
	// and nothing else: only the bare form is a run.
	runFlags = addRunFlags(cmd, &run)

	cmd.AddCommand(app.newRunCommand())
	cmd.AddCommand(app.newListCommand())
	cmd.AddCommand(app.newRemoveCommand())
	cmd.AddCommand(app.newShellCommand())
	cmd.AddCommand(app.newCPCommand())
	cmd.AddCommand(app.newAttachCommand())
	cmd.AddCommand(app.newProxyCommand())
	cmd.AddCommand(app.newApplyCommand())
	cmd.AddCommand(app.newPushCommand())
	cmd.AddCommand(app.newToolsCommand())
	cmd.AddCommand(app.newConfigureCommand())
	cmd.AddCommand(app.newIDCommand())
	cmd.AddCommand(app.newSecretCommand())
	cmd.AddCommand(app.newTUICommand())
	cmd.AddCommand(app.newCompletionCommand())
	cmd.AddCommand(app.newAdminCommand())
	cmd.AddCommand(app.newVersionCommand())
	// Cobra's usage template always lists a subcommand literally named "help",
	// even when hidden. Give the help command another name so it stays out of the
	// command list; the --help flag still works on every command.
	cmd.SetHelpCommand(&cobra.Command{Use: "no-help", Hidden: true})
	return cmd, app
}

// rootArgs refuses the words that follow a bare `discobox`, which are never a
// prompt and never anything else this command can run.
//
// The prompt is -p, and words after the command are not a prompt. Taking them
// as one (ADR 0089, superseded by ADR 0100) makes every subcommand name one
// typo away from a sandbox: `discobox lst` would create a discobox prompted
// "lst" rather than saying what was misspelled. Nothing can tell a typo from
// the first word of a prompt — a prompt is words — so the prompt takes a flag
// and the words are subcommands (ADR 0100). Run keeps its trailing
// prompt, where the name in front of it says what the words are.
//
// The word that named no command is reported here rather than by cobra, whose
// own check runs from Find and so not at all under TraverseChildren. Two kinds
// of word arrive, and they are told different things. A word past a `--` is a
// prompt written the way run's help teaches, and is answered with -p: dropping
// it silently is the worse failure, since `discobox -d -- fix the failing
// tests` would otherwise create a discobox with an empty prompt and say
// nothing about the four words it lost. Anything else is a command that does
// not exist, and is answered with the ones it was near.
func rootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	if cmd.ArgsLenAtDash() == 0 {
		return wordsAreNotAPrompt(cmd, args)
	}
	suggestions := ""
	if found := cmd.SuggestionsFor(args[0]); len(found) > 0 {
		suggestions = "\n\nDid you mean this?\n\t" + strings.Join(found, "\n\t")
	}
	return fmt.Errorf("unknown command %q for %q%s", args[0], cmd.CommandPath(), suggestions)
}

func wordsAreNotAPrompt(cmd *cobra.Command, words []string) error {
	return fmt.Errorf("%s takes no arguments; the prompt is -p: discobox -p %q", cmd.CommandPath(), strings.Join(words, " "))
}

// commandWords is the command's name as it was typed, for putting back into a
// sentence a `--` split in half.
//
// The name is `CalledAs` rather than `Name` because the alias is what stands in
// the words — and it is the alias that reads like one of them: `c`, `r`,
// `init`, `ps` and `scp` are all commands here. Cobra fills `CalledAs` in for
// the command it ends at only, so a parent reached by an alias (`t` for
// `tools`) is still named canonically, and a flag written after the command
// name is not here at all — this is what reached the command, and its own flags
// were parsed out of it. Both are a word out of place in a line the reader can
// see their own sentence in, rather than a wrong answer, and neither is
// reachable from inside cobra's dispatch: what was actually typed is gone by
// the time any hook runs.
func commandWords(cmd *cobra.Command) []string {
	words := strings.Fields(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()))
	if called := cmd.CalledAs(); called != "" && len(words) > 0 {
		words[len(words)-1] = called
	}
	return words
}

// refuseRootOnlyArguments refuses what was written in front of a subcommand
// and means nothing there. Both kinds are silent failures rather than wrong
// answers, which is why they are worth a check of their own.
//
// Words are the first kind. A `--` in front of a subcommand ends the root's
// flags, and cobra's scan for the command name does not stop at one — it reads
// `--` as a flag awaiting a value, eats the word after it, and carries on
// looking. So `discobox -- please run the tests` finds `run` in the middle of a
// sentence and creates a discobox prompted "the tests". What gives it away is
// that the root parsed a positional word while a subcommand was found, which
// nothing else does: the words in front of a command name are only ever this.
// They get the same answer `discobox -- fix the tests` gets, reassembled from
// the root's leftovers, the command path, and what reached the command.
//
// Run's own flags are the second. They are the root's local flags, so
// TraverseChildren parses them wherever they stand — and in front of a
// subcommand they are parsed into a run that never happens. `discobox -p 'fix
// the tests' ls` would list, silently, with the prompt dropped; `discobox -p
// 'fix the tests' run` would create a discobox with an empty prompt, since run
// has its own copy of those flags and nothing was written after the name.
// Without TraverseChildren, Cobra would reject these as unknown flags for the
// subcommand, and this keeps that loudness.
//
// `version` carries its own PersistentPreRunE and so reaches neither check, on
// purpose: what somebody diagnosing a broken environment asks first is what
// they are running, and printing it is not something a smuggled word can spoil.
func refuseRootOnlyArguments(cmd *cobra.Command, args []string, runFlags *pflag.FlagSet) error {
	root := cmd.Root()
	if cmd == root {
		// rootArgs has already refused anything there is to refuse here.
		return nil
	}
	if words := root.Flags().Args(); len(words) > 0 {
		return wordsAreNotAPrompt(root, slices.Concat(words, commandWords(cmd), args))
	}
	var given *pflag.Flag
	runFlags.VisitAll(func(flag *pflag.Flag) {
		if given == nil && flag.Changed {
			given = flag
		}
	})
	if given != nil {
		return fmt.Errorf("--%s is a run's own flag and does nothing in front of %s: a run is `discobox run --%s ...`, or `discobox --%s ...` with no command name at all",
			given.Name, cmd.CommandPath(), given.Name, given.Name)
	}
	return nil
}

func (a *App) addQuietFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&a.quiet, "quiet", "q", false, "Only display resource IDs")
}

func (a *App) validate() error {
	switch a.output {
	case "table", "json":
	default:
		return fmt.Errorf("unsupported output format %q; expected table or json", a.output)
	}
	leaderKey, err := keys.NormalizeLeader(os.Getenv(keys.LeaderEnv))
	if err != nil {
		// Resolved once, for every command, rather than at the attach that
		// happens to need it: a leader the environment spells wrong is worth
		// hearing about before a terminal has been handed over.
		return fmt.Errorf("%s: %w", keys.LeaderEnv, err)
	}
	a.leaderKey = leaderKey
	return nil
}

// leader is the prefix key this invocation reserves, as a Bubble Tea key name.
//
// One key covers both terminals discobox shows you — the launcher's panes and a
// plain attach — so there is one thing to learn and one thing to change when it
// collides with what you run inside a sandbox.
func (a *App) leader() string {
	if a.leaderKey == "" {
		return keys.DefaultLeader
	}
	return a.leaderKey
}

func (a *App) projectIDValue() (string, error) {
	projectID := strings.TrimSpace(a.projectID)
	if projectID == "" {
		return "", errMissingProject
	}
	return projectID, nil
}

func (a *App) apiClient() (*apiclientgen.Client, error) {
	baseURL, client, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	return apiclientgen.NewClient(baseURL, apiclientgen.WithClient(client))
}

func (a *App) httpClient() (string, *http.Client, error) {
	return a.httpClientWithAutoStart(shouldAutoLaunchServer(a.autoStart))
}

// shouldAutoLaunchServer reports whether this invocation may start a server for
// itself. --auto-start-server is the last word when it names one: "true" or
// "false" is the per-invocation override, and nothing about how the binary was
// built or what the environment says outranks somebody typing it. "auto" —
// what an invocation that never passes the flag gets — leaves the build and
// the environment's own answer standing (autoLaunchConfigured).
func shouldAutoLaunchServer(autoStart autoStartServer) bool {
	switch autoStart {
	case autoStartServerTrue:
		return true
	case autoStartServerFalse:
		return false
	default:
		return autoLaunchConfigured()
	}
}

func (a *App) httpClientWithAutoStart(autoStart bool) (string, *http.Client, error) {
	transport := http.DefaultTransport
	parsed := a.serverEndpoint()
	if autoStart && parsed.AutoLaunchable() {
		if err := a.ensureLocalServerOnce(); err != nil {
			return "", nil, err
		}
	}
	if err := configureIrohForEndpoint(parsed, a.irohRelayURLs, a.irohLogLevel); err != nil {
		return "", nil, err
	}
	baseURL, client, err := endpoint.HTTPClient(a.serverURL, transport)
	if err != nil {
		return "", nil, err
	}
	transport = client.Transport
	if a.debug {
		transport = debugTransport{
			out:  a.errOut,
			base: transport,
		}
	}
	transport = textPlainErrorTransport{
		base: transport,
	}
	if strings.TrimSpace(a.token) != "" {
		transport = requestHeaderTransport{
			token: strings.TrimSpace(a.token),
			base:  transport,
		}
	}
	// Outermost, so everything underneath is marked before it reaches a caller:
	// the dial, the relay, a connection lost mid-request. There is no shortcut
	// to http.DefaultClient any more — an unmarked client would be a request
	// whose failure cannot be told from any other in the process.
	transport = serverTransport{
		base: transport,
	}
	return baseURL, &http.Client{
		Transport: transport,
	}, nil
}

// serverTransport marks what the control-plane transport could not do as this
// client's own failure.
//
// A *url.Error means the request never completed, and nothing in its shape says
// which endpoint it was for: `admin server stage` failing to download a release
// asset produces exactly the same type, and a local listener that will not bind
// produces the *net.OpError underneath it. Telling either of those to go and
// diagnose the control plane sends the reader after a server that is answering
// perfectly. The mark is the discriminator, and it belongs here because this is
// the layer that knows whose transport this is. See withUnreachableServerHint.
type serverTransport struct {
	base http.RoundTripper
}

func (t serverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, serverUnreachable{err: err}
	}
	return resp, nil
}

// serverUnreachable is that mark. It carries the transport's own error and
// nothing of its own, so what a reader sees is unchanged and errors.Is still
// finds the syscall underneath.
type serverUnreachable struct {
	err error
}

func (e serverUnreachable) Error() string { return e.err.Error() }

func (e serverUnreachable) Unwrap() error { return e.err }

// gitServerURL is the base URL git commands address the server through, along
// with the func that releases it.
//
// git only speaks URLs, so a unix socket or named pipe endpoint has to be
// bridged: the returned URL is a loopback proxy onto the same local server the
// API client uses. An http(s) endpoint is already addressable and is returned
// as-is, with nothing to release.
func (a *App) gitServerURL(ctx context.Context) (string, func(), error) {
	if a.serverEndpoint().DirectlyDialable() {
		return a.serverURL, func() {}, nil
	}
	proxy, err := endpoint.StartLoopbackProxy(ctx, a.serverURL)
	if err != nil {
		return "", nil, err
	}
	return proxy.BaseURL(), func() { _ = proxy.Close() }, nil
}

// serverEndpoint parses --server so callers can ask what the endpoint supports
// rather than pattern-matching its scheme. A malformed endpoint answers "no" to
// every capability; the error itself surfaces from the call that goes on to use
// the endpoint, where it can say what failed.
func (a *App) serverEndpoint() endpoint.Endpoint {
	parsed, err := endpoint.Parse(a.serverURL)
	if err != nil {
		return endpoint.Endpoint{}
	}
	return parsed
}

// ensureLocalServerOnce autolaunches a server for this invocation, at most
// once.
//
// Starting one is a startup step, not something to reach for again every time a
// request fails. Anything that retries — a terminal attach reconnecting, a
// watch loop — asks for a client on every pass, and each of those asks would be
// a fresh chance to launch a server. A command that outlived its server would
// spend the rest of its life spawning replacements: one every few seconds, each
// of them losing the race for the data directory's singleton lock and exiting,
// for as long as the command ran. A server that was up and went away is a thing
// to report, not to paper over.
//
// Only the caller that made the attempt is told how it went. A later one is not
// handed a failure it did not cause and cannot act on; it goes on to dial the
// endpoint, where a server that never started shows up as the connection error
// it is.
func (a *App) ensureLocalServerOnce() error {
	var attempted bool
	var err error
	a.autoLaunchOnce.Do(func() {
		attempted = true
		err = a.ensureLocalServer(context.Background())
	})
	if !attempted {
		return nil
	}
	return err
}

func (a *App) ensureLocalServer(ctx context.Context) error {
	// One line for the whole start, rewritten in place and taken back down
	// before the command that wanted the server writes anything of its own.
	// A line per phase would leave a first run five of them scrolled above
	// output that had nothing to do with them.
	progress := a.serverStartupLine()
	started, err := endpoint.EnsureRunning(ctx, endpoint.LaunchOptions{
		Endpoint: a.serverURL,
		// Resolved only if a server actually has to be started, because
		// resolving one can mean downloading it (ADR 0099) — and a machine
		// whose server is already running, or a development build with no
		// server to download, must not pay for that to find out.
		Command: func(ctx context.Context) (endpoint.Command, error) {
			path, err := a.resolveServer(ctx, func(report serverstage.Progress) {
				progress.set(serverStageText(report))
			})
			if err != nil {
				return endpoint.Command{}, err
			}
			return endpoint.Command{Path: path}, nil
		},
		Env:             localServerEnv(a.serverURL),
		ExpectedVersion: version.String(),
		OnProgress: func(status health.Status) {
			progress.set(serverStartupText(status))
		},
	})
	progress.clear()
	if err != nil {
		return err
	}
	if started {
		// A server started this way outlives the command that wanted it, which
		// is the point and also something the user did not ask for. Say that it
		// happened, where its output went — it has no terminal, so the file is
		// the only place it exists — and how to undo it. A caller that found one
		// already up says nothing.
		a.notify("started the discobox server in the background (logs: discobox admin server logs; stop it with: discobox admin server shutdown)")
		a.startedServer = true
		// Only a run holds for the images, and only when nothing else is
		// showing them. See waitsOutFirstRunStaging.
		if !a.waitsOutFirstRunStaging() {
			return nil
		}
		// And wait out the first run here, once, rather than letting it happen
		// inside whichever operation first needs an image (ADR 0069). Only the
		// caller that started the server does this: it is the one that knows
		// this is a first run, because it is the one that caused it.
		// A fresh line: clearing one is final, so the launch's line cannot be
		// reused for the wait that follows it.
		staging := a.serverStartupLine()
		a.waitForStagedPools(ctx, func(line string) { staging.set(line) })
		staging.clear()
	}
	return nil
}

// waitsOutFirstRunStaging reports whether this command should hold while a
// first run stages the images a discobox runs.
//
// Only a command that is going to run one. Staging is a head start for the
// images a discobox opens (ADR 0069), and `discobox ps` opens none: holding a
// list behind gigabytes it will never read is a first command that prints the
// line about the server it started and then appears to hang, which is exactly
// how it was reported. The pull is not lost by not waiting here — it happens
// inside the operation that needs the image, which narrates it there.
//
// A front end waits for nothing either, for the opposite reason: the window
// reports this under its own frame while the user gets on with the application,
// which beats a line they can only watch — so it says so, and this leaves the
// waiting to it.
func (a *App) waitsOutFirstRunStaging() bool {
	if a.stagingShownByUI {
		return false
	}
	return a.runsAnImage
}

// serverStartupLine is where a launch narrates, or nothing when there is
// nowhere to narrate to. --quiet asked for identifiers and nothing else, and a
// status line is the "nothing else".
//
// A nil line is the no-op: every statusLine method takes one, so the launch
// path has no reporting to switch on.
func (a *App) serverStartupLine() *statusLine {
	if a.quiet || a.errOut == nil {
		return nil
	}
	return newStatusLine(a.errOut)
}

// serverStartupText says what the server this CLI just launched is doing.
//
// Starting one can take a while on a first run — a database to migrate, a
// registry to reach for the built-in harness images — and were the whole of
// that silent, the only two outcomes a user would see are a prompt that comes
// back late and a timeout that explains nothing.
//
// The shape is the window's, and so is the grammar: sentence case, and the
// phase after a colon as a detail of the one thing being narrated rather than a
// second thing alongside it. One first run draws three of these on this one
// line — the server downloading (serverStageText), the server starting, the
// images staging (stageLine) — and three ways of writing the same sentence read
// as three unrelated programs taking turns. A server that reports no phase says
// only what it is, because "Starting server: starting" is the same word twice.
func serverStartupText(status health.Status) string {
	if phase := strings.TrimSpace(status.Phase); phase != "" {
		return "Starting server: " + phase
	}
	return "Starting server"
}

// notify writes one line about something done on the user's behalf.
func (a *App) notify(text string) {
	if a.quiet || a.errOut == nil {
		return
	}
	fmt.Fprintln(a.errOut, text)
}

// localServerEnv configures the server this CLI launches for itself. It listens
// on the endpoint the CLI is about to dial and nothing else: an autolaunched
// server is this user's, and opening a TCP port on their machine is not
// something running `discobox` should imply. An operator who needs HTTP names it
// in DISCOBOX_SERVER_LISTEN and runs the server themselves.
func localServerEnv(endpoint string) []string {
	env := []string{
		"DISCOBOX_SERVER_LISTEN=" + endpoint,
		"DISCOBOX_SERVER=" + endpoint,
	}
	for _, key := range []string{
		"DATABASE_DSN",
		"DATABASE_READ_DSN",
		"DISCOBOX_CACHE_DIR",
		"DISCOBOX_CONFIG_DIR",
		"DISCOBOX_DATA_DIR",
		"DISCOBOX_DEFAULT_DISCOBOX_IMAGE",
		"DISCOBOX_ENCRYPTION_KEY",
		"DISCOBOX_ENV_FILE",
		"DISCOBOX_STATE_DIR",
		"OTEL_METRICS_EXPORTER",
		"PATH",
		"PORT",
	} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

type requestHeaderTransport struct {
	token string
	base  http.RoundTripper
}

func (t requestHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if t.token != "" {
		cloned.Header.Set("Authorization", "Bearer "+t.token)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

type textPlainErrorTransport struct {
	base http.RoundTripper
}

func (t textPlainErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 400 {
		return resp, err
	}
	ct, _, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if parseErr != nil || ct != "text/plain" || resp.Body == nil {
		return resp, err
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp, readErr
	}
	if closeErr != nil {
		return resp, closeErr
	}
	return nil, fmt.Errorf("request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

type debugTransport struct {
	out  io.Writer
	base http.RoundTripper
}

func (t debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := t.out
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "> %s %s\n", req.Method, req.URL.Redacted())
	for name, values := range req.Header {
		for _, value := range values {
			if strings.EqualFold(name, "Authorization") {
				value = "[REDACTED]"
			}
			fmt.Fprintf(out, "> %s: %s\n", name, value)
		}
	}
	req, err := logRequestBody(out, req)
	if err != nil {
		fmt.Fprintf(out, "> body error: %v\n", err)
		return nil, err
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(out, "< error: %v\n", err)
		return nil, err
	}
	fmt.Fprintf(out, "< %s\n", resp.Status)
	// A 101 hands the connection itself back as resp.Body, and the websocket
	// client takes it over by asserting io.ReadWriteCloser. Wrapping it fails
	// that assertion — every attach under --debug dies with "101 Switching
	// Protocols" — and would dump binary frames into the log besides.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return resp, nil
	}
	if resp.Body != nil && resp.Body != http.NoBody {
		fmt.Fprintln(out, "< body:")
		resp.Body = &debugBody{out: out, base: resp.Body}
	}
	return resp, nil
}

func logRequestBody(out io.Writer, req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return req, err
	}
	if err := req.Body.Close(); err != nil {
		return req, err
	}
	cloned := req.Clone(req.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	cloned.ContentLength = int64(len(body))
	if len(body) > 0 {
		fmt.Fprintln(out, "> body:")
		_, _ = out.Write(body)
		if body[len(body)-1] != '\n' {
			fmt.Fprintln(out)
		}
	}
	return cloned, nil
}

type debugBody struct {
	out       io.Writer
	base      io.ReadCloser
	wroteBody bool
	lastByte  byte
}

func (b *debugBody) Read(p []byte) (int, error) {
	n, err := b.base.Read(p)
	if n > 0 {
		_, _ = b.out.Write(p[:n])
		b.wroteBody = true
		b.lastByte = p[n-1]
	}
	return n, err
}

func (b *debugBody) Close() error {
	if b.wroteBody && b.lastByte != '\n' {
		fmt.Fprintln(b.out)
	}
	return b.base.Close()
}

func envOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}
