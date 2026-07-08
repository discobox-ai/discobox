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

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

var errAgentTerminalDetached = errors.New("detached")

type sandboxTerminalCreateOptions struct {
	agentID string
	args    []string
	workdir string
	env     []string
	attach  bool
}

// A terminal is an exec created in agent mode: the CLI terminal commands are a
// thin preset over the sandbox exec API (create with agentId, always TTY).
func (a *App) newSandboxTerminalsCommand() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:     "terminal",
		Aliases: []string{"terminals"},
		Short:   "Manage sandbox agent terminals",
	}
	cmd.PersistentFlags().StringVar(&sandboxID, "sandbox-id", "", "Sandbox ID")
	_ = cmd.RegisterFlagCompletionFunc("sandbox-id", a.completeSandboxes)
	cmd.AddCommand(a.newSandboxTerminalListCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalCreateCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalAttachCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalLogsCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxTerminalDeleteCommand(&sandboxID))
	return cmd
}

func (a *App) newSandboxTerminalListCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sandbox agent terminals",
		Args:  cobra.NoArgs,
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
		Short: "Create a sandbox agent terminal",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, _, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
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
				return a.attachSandboxTerminal(cmd.Context(), projectID, resolvedSandboxID, terminal.ID, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			started, err := a.startSandboxExec(cmd.Context(), projectID, resolvedSandboxID, terminal.ID)
			if err != nil {
				return err
			}
			return a.writeSandboxTerminal(cmd, &started)
		},
	}
	cmd.Flags().StringVar(&opts.agentID, "agent", "", "Agent ID to start; defaults to the sandbox configured agent")
	cmd.Flags().StringArrayVar(&opts.args, "arg", nil, "Additional command argument; repeat for multiple arguments")
	cmd.Flags().StringVar(&opts.workdir, "workdir", "", "Working directory inside the sandbox")
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables")
	cmd.Flags().BoolVar(&opts.attach, "attach", false, "Attach after creating the terminal")
	return cmd
}

func (a *App) newSandboxTerminalAttachCommand(sandboxID *string) *cobra.Command {
	return &cobra.Command{
		Use:               "attach TERMINAL_ID",
		Short:             "Attach to a sandbox agent terminal",
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
			return a.attachSandboxTerminal(cmd.Context(), projectID, resolvedSandboxID, terminalID, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func (a *App) newSandboxTerminalLogsCommand(sandboxID *string) *cobra.Command {
	var includeInput bool
	cmd := &cobra.Command{
		Use:               "logs TERMINAL_ID",
		Short:             "Print sandbox agent terminal output logs",
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
		Short:             "Delete a sandbox agent terminal",
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
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "deleted terminal %s\n", shortID(terminalID))
			return err
		},
	}
}

func (a *App) sandboxTerminalRequest(ctx context.Context, sandboxArg string) (string, string, *apiclientgen.Client, error) {
	if strings.TrimSpace(sandboxArg) == "" {
		return "", "", nil, fmt.Errorf("--sandbox-id is required")
	}
	return a.sandboxRequest(ctx, sandboxArg)
}

func createTerminalExecBody(opts sandboxTerminalCreateOptions) (*apimodel.CreateSandboxExecRequest, error) {
	body := &apimodel.CreateSandboxExecRequest{}
	body.SetAgentId(optString(opts.agentID))
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
	if cols, rows, ok := terminalSize(os.Stdin); ok {
		body.SetCols(apiclientgen.NewOptInt(cols))
		body.SetRows(apiclientgen.NewOptInt(rows))
	}
	return body, nil
}

// listSandboxTerminals returns the execs that were created in agent (terminal)
// mode, i.e. those carrying an agentId.
func (a *App) listSandboxTerminals(ctx context.Context, projectID, sandboxID string) ([]apimodel.SandboxExec, error) {
	execs, err := a.listSandboxExecs(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]apimodel.SandboxExec, 0, len(execs))
	for _, exec := range execs {
		if strings.TrimSpace(exec.AgentId.Or("")) != "" {
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
	if a.quiet {
		terminals = sortedByCreatedAt(terminals, func(t apimodel.SandboxExec) time.Time { return t.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), terminals, func(t apimodel.SandboxExec) string { return t.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"terminals": terminals})
	}
	terminals = sortedByCreatedAt(terminals, func(t apimodel.SandboxExec) time.Time { return t.CreatedAt })
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(terminal.ID),
			terminal.AgentId.Or(""),
			terminal.Status,
			pid,
			exitCode,
			truncateTableValue(terminal.Workdir, 36),
			truncateTableValue(strings.Join(terminal.Command, " "), 48),
			formatTime(terminal.CreatedAt),
		)
	}
	return tw.Flush()
}

// attachSandboxTerminal attaches to a terminal exec over the exec websocket with
// scrollback replay and Ctrl-P Ctrl-Q detach handling.
func (a *App) attachSandboxTerminal(ctx context.Context, projectID, sandboxID, terminalID string, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := a.openSandboxExecAttach(ctx, projectID, sandboxID, terminalID, true)
	if err != nil {
		return err
	}
	defer conn.Close()

	session := &framedAttachSession{
		conn:       conn,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		kind:       "agent terminal",
		action:     "attach terminal",
		rawMode:    true,
		resize:     true,
		copyInput:  copyAgentTerminalInput,
		errorFrame: printAttachErrorFrame(stderr),
		otherErr: func(err error) (bool, error) {
			if isAttachDone(err) {
				return true, nil
			}
			return false, err
		},
	}
	if err := session.writeInitialResize(); err != nil {
		return err
	}
	if _, err := a.startSandboxExec(ctx, projectID, sandboxID, terminalID); err != nil {
		return err
	}
	err = session.run(ctx)
	if errors.Is(err, errAgentTerminalDetached) {
		return nil
	}
	return err
}

func copyAgentTerminalInput(ctx context.Context, s *framedAttachSession) error {
	buf := make([]byte, 32*1024)
	pendingCtrlP := false
	for {
		n, err := s.stdin.Read(buf)
		if n > 0 {
			payload, detach := filterDetachSequence(buf[:n], &pendingCtrlP)
			if len(payload) > 0 {
				if writeErr := s.writeFrame(attachFrameInput, payload); writeErr != nil {
					return writeErr
				}
			}
			if detach {
				return errAgentTerminalDetached
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

func filterDetachSequence(in []byte, pendingCtrlP *bool) ([]byte, bool) {
	out := bytes.NewBuffer(make([]byte, 0, len(in)))
	for _, b := range in {
		if *pendingCtrlP {
			*pendingCtrlP = false
			if b == 'q' || b == 'Q' {
				return out.Bytes(), true
			}
			out.WriteByte(0x10)
		}
		if b == 0x10 {
			*pendingCtrlP = true
			continue
		}
		out.WriteByte(b)
	}
	return out.Bytes(), false
}
