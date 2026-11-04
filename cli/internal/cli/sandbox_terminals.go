package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/client"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/keys"
)

var errTerminalDetached = errors.New("detached")

type sandboxTerminalCreateOptions struct {
	harnessID string
	args      []string
	workdir   string
	env       []string
	attach    bool
}

// A terminal is an exec created in harness mode: the CLI terminal commands are a
// thin preset over the sandbox exec API (create with harnessId, always TTY).
func (a *App) newSandboxTerminalsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "terminal",
		Aliases: []string{"terminals"},
		Short:   "Manage discobox terminals",
	}
	cmd.AddCommand(a.newSandboxTerminalListCommand())
	cmd.AddCommand(a.newSandboxTerminalCreateCommand())
	cmd.AddCommand(a.newSandboxTerminalAttachCommand())
	cmd.AddCommand(a.newSandboxTerminalLogsCommand())
	cmd.AddCommand(a.newSandboxTerminalScreenCommand())
	cmd.AddCommand(a.newSandboxTerminalInputCommand())
	cmd.AddCommand(a.newSandboxTerminalWaitCommand())
	cmd.AddCommand(a.newSandboxTerminalDeleteCommand())
	return cmd
}

func (a *App) newSandboxTerminalListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls [DISCOBOX]",
		Aliases: []string{"list"},
		Short:   "List discobox terminals",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, projectID, resolvedSandboxID, _, err := a.selectSandbox(cmd, discoboxArg(args))
			if err != nil {
				return err
			}
			terminals, observedAt, err := a.listSandboxTerminals(cmd.Context(), projectID, resolvedSandboxID)
			if err != nil {
				return err
			}
			return a.writeSandboxTerminals(cmd, terminals, observedAt)
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSandboxTerminalCreateCommand() *cobra.Command {
	var opts sandboxTerminalCreateOptions
	cmd := &cobra.Command{
		Use:   "create [DISCOBOX]",
		Short: "Create a discobox terminal",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, projectID, resolvedSandboxID, client, err := a.selectSandbox(cmd, discoboxArg(args))
			if err != nil {
				return err
			}
			// When a specific harness is requested, assign that harness's bound secrets
			// to the sandbox and carry their sentinels in this run's environment.
			if strings.TrimSpace(opts.harnessID) != "" {
				harnessConfigID, err := a.resolveHarnessConfigID(cmd.Context(), client, projectID, opts.harnessID)
				if err != nil {
					return err
				}
				opts.harnessID = harnessConfigID
				assigned, err := a.assignSandboxHarnessSecrets(cmd.Context(), client, projectID, resolvedSandboxID, harnessConfigID)
				if err != nil {
					return err
				}
				for env, sentinel := range assigned {
					opts.env = append(opts.env, env+"="+sentinel)
				}
			}
			body, err := createTerminalExecBody(opts)
			if err != nil {
				return err
			}
			terminal, err := a.createSandboxExec(cmd.Context(), projectID, resolvedSandboxID, body, false)
			if err != nil {
				return err
			}
			if opts.attach {
				return a.attachSandboxTerminal(cmd.Context(), projectID, resolvedSandboxID, terminal.ID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			started, err := a.startSandboxExec(cmd.Context(), projectID, resolvedSandboxID, terminal.ID)
			if err != nil {
				return err
			}
			return a.writeSandboxTerminal(cmd, &started)
		},
	}
	cmd.Flags().StringVar(&opts.harnessID, "harness", "", "Harness to start, by slug (e.g. codex), name, or ID; defaults to the discobox's configured harness")
	cmd.Flags().StringArrayVar(&opts.args, "arg", nil, "Additional command argument; repeat for multiple arguments")
	cmd.Flags().StringVar(&opts.workdir, "workdir", "", "Working directory inside the discobox")
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables")
	cmd.Flags().BoolVar(&opts.attach, "attach", false, "Attach after creating the terminal")
	return cmd
}

func (a *App) newSandboxTerminalAttachCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "attach DISCOBOX TERMINAL_ID",
		Short: "Attach to a discobox terminal",
		Long: `Attach to a discobox terminal.

Pass "primary" instead of a terminal ID to attach the discobox's primary
terminal, relaunching it with the harness's relaunch command when it has
stopped. Attaching a terminal by ID never relaunches it: an ID names one
session, and once that session has ended there is nothing to attach to.

The leader key then d — Ctrl-A d by default, and Ctrl-A Ctrl-D works too —
detaches without ending the session. Set DISCOBOX_LEADER to change the Ctrl-A.`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: a.completeTerminals,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, projectID, resolvedSandboxID, _, err := a.selectSandbox(cmd, args[0])
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[1])
			if err != nil {
				return err
			}
			return a.attachSandboxTerminal(cmd.Context(), projectID, resolvedSandboxID, terminalID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func (a *App) newSandboxTerminalLogsCommand() *cobra.Command {
	var includeInput bool
	cmd := &cobra.Command{
		Use:               "logs DISCOBOX TERMINAL_ID",
		Short:             "Print discobox terminal output logs",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: a.completeTerminals,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, projectID, resolvedSandboxID, client, err := a.selectSandbox(cmd, args[0])
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[1])
			if err != nil {
				return err
			}
			res, err := client.ListSandboxExecLogs(cmd.Context(), apiclientgen.ListSandboxExecLogsParams{
				ProjectId: projectID,
				SandboxId: resolvedSandboxID,
				ExecId:    terminalID,
			})
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.SandboxExecLogsResponse](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), body)
			}
			return writeSandboxExecLogs(cmd.OutOrStdout(), cmd.ErrOrStderr(), body.GetEntries(), includeInput)
		},
	}
	cmd.Flags().BoolVar(&includeInput, "include-input", false, "Include input bytes as well as terminal output")
	return cmd
}

func (a *App) newSandboxTerminalDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "rm DISCOBOX TERMINAL_ID",
		Aliases:           []string{"delete", "remove"},
		Short:             "Delete a discobox terminal",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: a.completeTerminals,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, projectID, resolvedSandboxID, _, err := a.selectSandbox(cmd, args[0])
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[1])
			if err != nil {
				return err
			}
			if err := a.execJSON(cmd.Context(), http.MethodDelete, projectID, resolvedSandboxID, "/"+url.PathEscape(terminalID), nil, nil); err != nil {
				return err
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"deleted": true, "terminalId": terminalID})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted terminal %s\n", terminalID)
			return err
		},
	}
}

// assignSandboxHarnessSecrets asks the control plane to materialize the harness
// config's bound secrets for the running sandbox and returns their env->sentinel
// map to inject into this run.
func (a *App) assignSandboxHarnessSecrets(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, harnessConfigID string) (map[string]string, error) {
	res, err := client.AssignSandboxHarnessSecrets(ctx, &apimodel.AssignSandboxHarnessSecretsBody{HarnessConfigId: harnessConfigID}, apiclientgen.AssignSandboxHarnessSecretsParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.SandboxHarnessSecretsResponse](res)
	if err != nil {
		return nil, err
	}
	return body.Secrets, nil
}

func createTerminalExecBody(opts sandboxTerminalCreateOptions) (*apimodel.CreateSandboxExecRequest, error) {
	body := &apimodel.CreateSandboxExecRequest{}
	body.SetHarnessId(optString(opts.harnessID))
	if len(opts.args) > 0 {
		body.SetArgs(append([]string{}, opts.args...))
	}
	body.SetWorkdir(optString(opts.workdir))
	env, err := keyValueMapFromShell(opts.env)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		body.SetEnv(apiclientgen.NewOptCreateSandboxExecRequestEnv(apiclientgen.CreateSandboxExecRequestEnv(env)))
	}
	body.SetTty(apiclientgen.NewOptBool(true))
	if cols, rows, ok := client.NewOSConsole(os.Stdin, os.Stdout).Size(); ok {
		body.SetCols(apiclientgen.NewOptInt(cols))
		body.SetRows(apiclientgen.NewOptInt(rows))
	}
	return body, nil
}

// listSandboxTerminals returns the execs that were created in harness (terminal)
// mode, i.e. those carrying a harnessId.
func (a *App) listSandboxTerminals(ctx context.Context, projectID, sandboxID string) ([]apimodel.SandboxExec, time.Time, error) {
	execs, observedAt, err := a.listSandboxExecs(ctx, projectID, sandboxID)
	if err != nil {
		return nil, time.Time{}, err
	}
	out := make([]apimodel.SandboxExec, 0, len(execs))
	for _, exec := range execs {
		if strings.TrimSpace(exec.HarnessId.Or("")) != "" {
			out = append(out, exec)
		}
	}
	return out, observedAt, nil
}

func (a *App) writeSandboxTerminal(cmd *cobra.Command, terminal *apimodel.SandboxExec) error {
	if terminal == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), terminal)
	}
	return a.writeSandboxTerminals(cmd, []apimodel.SandboxExec{*terminal}, terminal.ObservedAt.Or(time.Time{}))
}

func (a *App) writeSandboxTerminals(cmd *cobra.Command, terminals []apimodel.SandboxExec, observedAt time.Time) error {
	terminals = sortedByRecency(terminals, func(t apimodel.SandboxExec) time.Time { return t.CreatedAt })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), terminals, func(t apimodel.SandboxExec) string { return t.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), storedAnswerJSON("terminals", terminals, observedAt))
	}
	defer noteStoredAnswer(cmd, observedAt)
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tAGENT\tSTATUS\tPID\tEXIT\tWORKDIR\tCOMMAND\tCREATED")
	for _, terminal := range terminals {
		pid := ""
		if value, ok := terminal.Pid.Get(); ok {
			pid = fmt.Sprint(value)
		}
		exitCode := ""
		if value, ok := terminal.ExitCode.Get(); ok {
			exitCode = fmt.Sprint(value)
		}
		// A terminal runs as a shell so its command has real job control; the
		// harness command typed into that shell (what the user thinks of as
		// "the command") is StartupCommand when set, and Command otherwise
		// (a plain shell terminal, which has no separate startup command).
		command := terminal.Command
		if len(terminal.StartupCommand) > 0 {
			command = terminal.StartupCommand
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			terminal.ID,
			terminal.CurrentHarnessId.Or(terminal.HarnessId.Or("")),
			terminal.Status,
			pid,
			exitCode,
			truncateTableValue(terminal.Workdir, 36),
			truncateTableValue(strings.Join(command, " "), 48),
			formatTime(terminal.CreatedAt),
		)
	}
	return tw.Flush()
}

// attachSandboxTerminal attaches to a terminal exec over the exec websocket with
// scrollback replay and detach-chord handling. See detachFilter.
func (a *App) attachSandboxTerminal(ctx context.Context, projectID, sandboxID, terminalID string, opts execAttachOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	opts.replay = true
	// Going to work in a discobox is when the commits made here belong in its
	// origin, and attaching a terminal is that moment (ADR 26-09-05-008 §1 on automatic
	// push). It runs beside the attach rather than before it: the dial below
	// can block for minutes behind a cold image pull, and a push is not a
	// reason to wait longer to see the terminal. Everything it does is silent
	// until the stream is over — see autoPushWhileAttached.
	// Named for what it is: "client" in this file is the exec stream's.
	apiClient, err := a.apiClient()
	if err != nil {
		return err
	}
	// A discobox still waiting for its source from this machine gets it
	// first, and the dial below then waits on a discobox that is starting
	// rather than on a push nobody is making (ADR 26-09-24-005).
	delivering := newStatusLine(stderr, a.color)
	ui := &cobra.Command{}
	ui.SetIn(stdin)
	ui.SetOut(stdout)
	ui.SetErr(stderr)
	err = a.deliverBeforeAttach(ctx, apiClient, projectID, sandboxID, delivering.set, credentialDeliveryOptions(ui, delivering, false))
	delivering.clear()
	if err != nil {
		return err
	}
	stopPushing := a.autoPushWhileAttached(ctx, apiClient, projectID, sandboxID)
	defer stopPushing(stderr)
	// The dial blocks for as long as the sandbox takes to become attachable —
	// every tier below waits for the readiness only it can see (ADR 0039) —
	// which behind a cold image pull is minutes on a silent socket. Say what it
	// is waiting for while it waits (ADR 0060).
	//
	// The narration ends when the dial returns, which is exactly when there is
	// nothing left to wait for: the sandbox-agent accepts the websocket only
	// after the primary terminal is launched and installed, so a connection in
	// hand means the terminal is about to draw. Clearing here also keeps the
	// status line from ever sharing the screen with it.
	status := newStatusLine(stderr, a.color)
	watching, stopWatching := context.WithCancel(ctx)
	go a.watchProvisioning(watching, projectID, sandboxID, status.set)
	frames, err := a.openReconnectingSandboxExecAttach(ctx, projectID, sandboxID, terminalID, opts)
	stopWatching()
	status.clear()
	if err != nil {
		return a.execAttachError(ctx, projectID, sandboxID, terminalID, err)
	}
	defer frames.Close()

	// One filter for the whole attach: the chord is a pair of keystrokes and
	// they can land in separate reads, so what the leader armed has to outlive
	// the read that carried it.
	chord := newDetachFilter(a.leader())
	session := client.New(client.Options{
		Conn:        frames,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		Console:     client.NewOSConsole(stdin, stdout),
		Kind:        "harness terminal",
		Action:      "attach terminal",
		RawMode:     true,
		Terminal:    true,
		SignalReady: true,
		CopyInput: func(ctx context.Context, s *client.Session) error {
			return copyTerminalInput(ctx, s, chord)
		},
		InterruptNotice: interruptNotice(stderr, true, "the discobox"),
		ErrorFrame:      printAttachErrorFrame(stderr),
		OtherErr: func(err error) (bool, error) {
			if client.IsDone(err) {
				return true, nil
			}
			return false, err
		},
	})
	if err := session.WriteInitialResize(); err != nil {
		return err
	}
	if _, err := a.startSandboxExec(ctx, projectID, sandboxID, terminalID); err != nil {
		return err
	}
	err = session.Run(ctx)
	if errors.Is(err, errTerminalDetached) {
		return nil
	}
	if errors.Is(err, client.ErrInterrupted) {
		return interruptedExit()
	}
	return err
}

// escapeTime is how long a raw attach holds back the start of an Alt leader —
// an Escape — waiting for the letter that would make it the leader, before
// sending it on as the Escape key it then was.
//
// The terminal sends Alt-G as Escape and g in one write, and they nearly always
// arrive in one read; this is for the time they do not, over a slow link. It
// is short because a lone Escape is a key people press constantly — vim's way
// out of insert mode — and it waits this long before reaching the program.
// Fifty milliseconds is Neovim's own ttimeoutlen, the same wait on the same
// question one step further in. Only an Escape that ends a read waits at all,
// and only under an Alt leader: an arrow key's Escape arrives with the rest of
// its sequence and is sent on at once.
const escapeTime = 50 * time.Millisecond

func copyTerminalInput(ctx context.Context, s *client.Session, chord *detachFilter) error {
	// A leader's first byte that ends a read is held until either the next
	// read says what it was or escapeTime does. mu orders the timer's flush
	// against the next read, so a keystroke is never sent ahead of one typed
	// before it; generation tells a timer a read has already decided for it.
	var (
		mu         sync.Mutex
		timer      *time.Timer
		generation int
		flushErr   error
	)
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		generation++
		if timer != nil {
			timer.Stop()
		}
	}()
	buf := make([]byte, 32*1024)
	stdin := s.Stdin()
	for {
		n, err := stdin.Read(buf)
		mu.Lock()
		generation++
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		if flushErr != nil {
			mu.Unlock()
			return flushErr
		}
		var payload []byte
		detach := false
		if n > 0 {
			payload, detach = chord.filter(buf[:n])
		}
		if err != nil && !detach {
			// No more input is coming to finish what the filter holds, so
			// it was keystrokes like any others.
			payload = append(payload, chord.flush()...)
		}
		if len(payload) > 0 {
			if writeErr := s.WriteFrame(frame.Input, payload); writeErr != nil {
				mu.Unlock()
				return writeErr
			}
		}
		if detach {
			mu.Unlock()
			return errTerminalDetached
		}
		if chord.undecided() {
			armed := generation
			timer = time.AfterFunc(escapeTime, func() {
				mu.Lock()
				defer mu.Unlock()
				if generation != armed {
					return
				}
				if held := chord.flush(); len(held) > 0 {
					flushErr = s.WriteFrame(frame.Input, held)
				}
			})
		}
		mu.Unlock()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

// detachKey is the second key of the detach chord: the leader, then d, the key
// screen and tmux both detach on. The launcher's pane uses q instead only
// because its leader also carries the list's own keys, and d among them is
// diff.
const detachKey = "d"

// detachFilter watches the bytes on their way to a terminal for the detach
// chord, and passes everything else through untouched.
//
// Nothing is taken from the program outright. The leader is held back only
// until the next keystroke says what it meant: a leader that qualified nothing
// is delivered along with the key that followed it, and the leader typed twice
// sends one literal leader, which is how you type the key the chord took. This
// is what screen and tmux do, and what the launcher's panes do, so the same
// keystrokes mean the same thing in a pane and in a plain attach.
//
// A key here is a byte sequence, not a byte: an Alt leader is Escape then its
// letter, and nothing says the two arrive in one read. So the filter holds the
// bytes that could still become a key it is waiting for, and lets go of them
// the moment they cannot — an arrow key's Escape [ is sent on as soon as the [
// arrives. What it is still holding when a read ends is [detachFilter.undecided],
// and [detachFilter.flush] is the caller deciding, once [escapeTime] is up,
// that no more of it is coming.
type detachFilter struct {
	// leader is the prefix key as the terminal sends it: Ctrl-A is 0x01, Alt-G
	// is Escape g.
	leader []byte
	// detach are the second keys that complete the chord: d, and d with the
	// leader's modifier still held — letting go precisely between the two
	// keystrokes of a chord is a skill nobody should have to acquire, so both
	// forms detach, as screen has bound its commands both ways for decades and
	// the launcher's panes match.
	detach [][]byte
	// armed is set between the leader and the key it qualifies.
	armed bool
	// held are the bytes read so far of a key that is not yet decided: a
	// prefix of the leader, or once armed, of a key that follows it.
	held []byte
}

// newDetachFilter builds the filter for a leader key name, as the keys package
// normalizes it.
func newDetachFilter(leaderKey string) *detachFilter {
	if keys.Sequence(leaderKey) == nil {
		leaderKey = keys.DefaultLeader
	}
	return &detachFilter{
		leader: keys.Sequence(leaderKey),
		detach: [][]byte{
			keys.Sequence(detachKey),
			keys.Sequence(keys.Modifier(leaderKey) + detachKey),
		},
	}
}

// filter returns the bytes to forward, and whether the chord completed. The
// rest of a read that completed it is dropped: the attach is over, and those
// keystrokes belong to whatever the caller returns to.
func (f *detachFilter) filter(in []byte) ([]byte, bool) {
	out := bytes.NewBuffer(make([]byte, 0, len(in)+len(f.leader)))
	for _, b := range in {
		f.held = append(f.held, b)
		if f.resolve(out) {
			return out.Bytes(), true
		}
	}
	return out.Bytes(), false
}

// resolve decides what it can of the held bytes, writing what it lets go of to
// out, and reports whether they completed the chord.
func (f *detachFilter) resolve(out *bytes.Buffer) bool {
	for len(f.held) > 0 {
		if !f.armed {
			switch {
			case bytes.Equal(f.held, f.leader):
				f.armed, f.held = true, nil
			case bytes.HasPrefix(f.leader, f.held):
				// Could still be the leader: wait for the rest.
				return false
			default:
				// Its first byte begins no leader, so it is the program's;
				// what came after it may begin one.
				out.WriteByte(f.held[0])
				f.held = f.held[1:]
			}
			continue
		}
		for _, key := range f.detach {
			if bytes.Equal(f.held, key) {
				f.armed, f.held = false, nil
				return true
			}
		}
		if bytes.Equal(f.held, f.leader) {
			// The leader twice is how you type one.
			out.Write(f.leader)
			f.armed, f.held = false, nil
			return false
		}
		if f.prefixOfArmedKey() {
			return false
		}
		// A leader that qualified nothing is delivered along with the key
		// that followed it, which is read again unarmed: it may be the
		// leader itself, or begin it.
		out.Write(f.leader)
		f.armed = false
	}
	return false
}

// prefixOfArmedKey reports whether the held bytes could still become a key
// the armed leader is waiting for.
func (f *detachFilter) prefixOfArmedKey() bool {
	for _, key := range append([][]byte{f.leader}, f.detach...) {
		if len(f.held) < len(key) && bytes.HasPrefix(key, f.held) {
			return true
		}
	}
	return false
}

// undecided reports whether the filter is holding the start of a key it
// cannot decide on without more input. The armed leader alone is not that: it
// waits for its next key however long that takes, as screen's and tmux's do.
func (f *detachFilter) undecided() bool { return len(f.held) > 0 }

// flush gives up waiting for the rest of the held key, returning the bytes to
// forward: what was held was what it looked like, an Escape and nothing more.
// An armed leader followed by it is delivered along with it, as a leader that
// qualified nothing always is.
func (f *detachFilter) flush() []byte {
	if len(f.held) == 0 {
		return nil
	}
	var out []byte
	if f.armed {
		out = append(out, f.leader...)
		f.armed = false
	}
	out = append(out, f.held...)
	f.held = nil
	return out
}

// detachHint is how to get out of an attach, as the messages that mention it
// spell the keys.
func (a *App) detachHint() string {
	return keys.Describe(a.leader()) + " " + detachKey
}
