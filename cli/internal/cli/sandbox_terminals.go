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
	"text/tabwriter"
	"time"

	"github.com/discobox-ai/discobox/execstream/client"

	"github.com/discobox-ai/discobox/execstream/frame"

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
	var sandboxID string
	cmd := &cobra.Command{
		Use:     "terminal",
		Aliases: []string{"terminals"},
		Short:   "Manage discobox terminals",
	}
	cmd.PersistentFlags().StringVar(&sandboxID, "discobox-id", "", "Discobox ID")
	_ = cmd.RegisterFlagCompletionFunc("discobox-id", a.completeSandboxes)
	cmd.AddCommand(a.newSandboxTerminalListCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalCreateCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalAttachCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalLogsCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalDeleteCommand(&sandboxID))
	return cmd
}

func (a *App) newSandboxTerminalListCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List discobox terminals",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, _, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminals, err := a.listSandboxTerminals(cmd.Context(), projectID, resolvedSandboxID)
			if err != nil {
				return err
			}
			return a.writeSandboxTerminals(cmd, terminals)
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSandboxTerminalCreateCommand(sandboxID *string) *cobra.Command {
	var opts sandboxTerminalCreateOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a discobox terminal",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
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
			terminal, err := a.createSandboxExec(cmd.Context(), projectID, resolvedSandboxID, body)
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
	cmd.Flags().StringVar(&opts.harnessID, "harness", "", "Harness ID to start; defaults to the discobox configured harness")
	cmd.Flags().StringArrayVar(&opts.args, "arg", nil, "Additional command argument; repeat for multiple arguments")
	cmd.Flags().StringVar(&opts.workdir, "workdir", "", "Working directory inside the discobox")
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables")
	cmd.Flags().BoolVar(&opts.attach, "attach", false, "Attach after creating the terminal")
	return cmd
}

func (a *App) newSandboxTerminalAttachCommand(sandboxID *string) *cobra.Command {
	return &cobra.Command{
		Use:   "attach TERMINAL_ID",
		Short: "Attach to a discobox terminal",
		Long: `Attach to a discobox terminal.

Pass "primary" instead of a terminal ID to attach the discobox's primary
terminal, relaunching it with the harness's relaunch command when it has
stopped. Attaching a terminal by ID never relaunches it: an ID names one
session, and once that session has ended there is nothing to attach to.

The leader key then d — Ctrl-A d by default, and Ctrl-A Ctrl-D works too —
detaches without ending the session. Set DISCOBOX_LEADER to change the Ctrl-A.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, _, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			return a.attachSandboxTerminal(cmd.Context(), projectID, resolvedSandboxID, terminalID, execAttachOptions{}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func (a *App) newSandboxTerminalLogsCommand(sandboxID *string) *cobra.Command {
	var includeInput bool
	cmd := &cobra.Command{
		Use:               "logs TERMINAL_ID",
		Short:             "Print discobox terminal output logs",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[0])
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

func (a *App) newSandboxTerminalDeleteCommand(sandboxID *string) *cobra.Command {
	return &cobra.Command{
		Use:               "delete TERMINAL_ID",
		Aliases:           []string{"rm", "remove"},
		Short:             "Delete a discobox terminal",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, _, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[0])
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

func (a *App) sandboxTerminalRequest(ctx context.Context, sandboxArg string) (string, string, *apiclientgen.Client, error) {
	if strings.TrimSpace(sandboxArg) == "" {
		return "", "", nil, fmt.Errorf("--discobox-id is required")
	}
	return a.sandboxRequest(ctx, sandboxArg)
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
	if cols, rows, ok := client.NewOSConsole(os.Stdin).Size(); ok {
		body.SetCols(apiclientgen.NewOptInt(cols))
		body.SetRows(apiclientgen.NewOptInt(rows))
	}
	return body, nil
}

// listSandboxTerminals returns the execs that were created in harness (terminal)
// mode, i.e. those carrying a harnessId.
func (a *App) listSandboxTerminals(ctx context.Context, projectID, sandboxID string) ([]apimodel.SandboxExec, error) {
	execs, err := a.listSandboxExecs(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]apimodel.SandboxExec, 0, len(execs))
	for _, exec := range execs {
		if strings.TrimSpace(exec.HarnessId.Or("")) != "" {
			out = append(out, exec)
		}
	}
	return out, nil
}

func (a *App) writeSandboxTerminal(cmd *cobra.Command, terminal *apimodel.SandboxExec) error {
	if terminal == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), terminal)
	}
	return a.writeSandboxTerminals(cmd, []apimodel.SandboxExec{*terminal})
}

func (a *App) writeSandboxTerminals(cmd *cobra.Command, terminals []apimodel.SandboxExec) error {
	terminals = sortedByRecency(terminals, func(t apimodel.SandboxExec) time.Time { return t.CreatedAt })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), terminals, func(t apimodel.SandboxExec) string { return t.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"terminals": terminals})
	}
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
			terminal.HarnessId.Or(""),
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
	status := newStatusLine(stderr)
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
		Console:     client.NewOSConsole(stdin),
		Kind:        "harness terminal",
		Action:      "attach terminal",
		RawMode:     true,
		Resize:      true,
		SignalReady: true,
		CopyInput: func(ctx context.Context, s *client.Session) error {
			return copyTerminalInput(ctx, s, chord)
		},
		ErrorFrame: printAttachErrorFrame(stderr),
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
	return err
}

func copyTerminalInput(ctx context.Context, s *client.Session, chord *detachFilter) error {
	buf := make([]byte, 32*1024)
	stdin := s.Stdin()
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			payload, detach := chord.filter(buf[:n])
			if len(payload) > 0 {
				if writeErr := s.WriteFrame(frame.Input, payload); writeErr != nil {
					return writeErr
				}
			}
			if detach {
				return errTerminalDetached
			}
		}
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

const (
	// detachKey is the second key of the detach chord: the leader, then d, the
	// key screen and tmux both detach on. The launcher's pane uses q instead
	// only because its leader also carries the list's own keys, and d among
	// them is diff.
	detachKey = 'd'
	// detachCtrlKey is the same key with Ctrl still held. Letting go precisely
	// between the two keystrokes of a chord is a skill nobody should have to
	// acquire, so both forms detach — screen has bound its commands both ways
	// for decades, and the launcher's panes match the same way.
	detachCtrlKey = detachKey - 'a' + 1
)

// detachFilter watches the bytes on their way to a terminal for the detach
// chord, and passes everything else through untouched.
//
// Nothing is taken from the program outright. The leader is held back only
// until the next keystroke says what it meant: a leader that qualified nothing
// is delivered along with the key that followed it, and the leader typed twice
// sends one literal leader, which is how you type the key the chord took. This
// is what screen and tmux do, and what the launcher's panes do, so the same
// keystrokes mean the same thing in a pane and in a plain attach.
type detachFilter struct {
	// leader is the prefix key as the terminal sends it: Ctrl-A is 0x01.
	leader byte
	// armed is set between the leader and the key it qualifies.
	armed bool
}

// newDetachFilter builds the filter for a leader key name, as the keys package
// normalizes it.
func newDetachFilter(leaderKey string) *detachFilter {
	leader := keys.ControlByte(leaderKey)
	if leader == 0 {
		leader = keys.ControlByte(keys.DefaultLeader)
	}
	return &detachFilter{leader: leader}
}

// filter returns the bytes to forward, and whether the chord completed. The
// rest of a read that completed it is dropped: the attach is over, and those
// keystrokes belong to whatever the caller returns to.
func (f *detachFilter) filter(in []byte) ([]byte, bool) {
	out := bytes.NewBuffer(make([]byte, 0, len(in)))
	for _, b := range in {
		if f.armed {
			f.armed = false
			switch b {
			case detachKey, detachCtrlKey:
				return out.Bytes(), true
			case f.leader:
				out.WriteByte(f.leader)
				continue
			}
			out.WriteByte(f.leader)
		}
		if b == f.leader {
			f.armed = true
			continue
		}
		out.WriteByte(b)
	}
	return out.Bytes(), false
}

// detachHint is how to get out of an attach, as the messages that mention it
// spell the keys.
func (a *App) detachHint() string {
	return keys.Describe(a.leader()) + " " + string(rune(detachKey))
}
