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

const (
	agentTerminalProtocol = "discobox-agent-terminal"
)

var errAgentTerminalDetached = errors.New("detached")

type sandboxTerminalCreateOptions struct {
	agentID string
	args    []string
	workdir string
	env     []string
	attach  bool
}

func (a *App) newSandboxTerminalsCommand() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:     "terminal",
		Aliases: []string{"terminals"},
		Short:   "Manage sandbox terminals",
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
		Short: "List sandbox terminals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			res, err := client.ListAgentTerminals(cmd.Context(), apiclientgen.ListAgentTerminalsParams{ProjectId: projectID, SandboxId: resolvedSandboxID})
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.AgentTerminalsResponse](res)
			if err != nil {
				return err
			}
			return a.writeAgentTerminals(cmd, body.GetTerminals())
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSandboxTerminalCreateCommand(sandboxID *string) *cobra.Command {
	var opts sandboxTerminalCreateOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a sandbox terminal",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			body, err := createAgentTerminalBody(opts)
			if err != nil {
				return err
			}
			res, err := client.CreateAgentTerminal(cmd.Context(), body, apiclientgen.CreateAgentTerminalParams{ProjectId: projectID, SandboxId: resolvedSandboxID})
			if err != nil {
				return err
			}
			created, err := expectResponse[apimodel.CreateAgentTerminalResponse](res)
			if err != nil {
				return err
			}
			terminal := created.GetTerminal()
			if opts.attach {
				return a.attachAgentTerminal(cmd.Context(), projectID, resolvedSandboxID, terminal.ID, false, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			started, err := a.startAgentTerminal(cmd.Context(), projectID, resolvedSandboxID, terminal.ID)
			if err != nil {
				return err
			}
			return a.writeAgentTerminal(cmd, &started)
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
	var replay bool
	cmd := &cobra.Command{
		Use:               "attach TERMINAL_ID",
		Short:             "Attach to a sandbox terminal",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveAgentTerminalID(cmd.Context(), client, projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			return a.attachAgentTerminal(cmd.Context(), projectID, resolvedSandboxID, terminalID, replay, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&replay, "replay", false, "Replay the saved session history before live streaming")
	return cmd
}

func (a *App) newSandboxTerminalLogsCommand(sandboxID *string) *cobra.Command {
	var includeInput bool
	cmd := &cobra.Command{
		Use:               "logs TERMINAL_ID",
		Short:             "Print sandbox terminal output logs",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveAgentTerminalID(cmd.Context(), client, projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			res, err := client.ListAgentTerminalLogs(cmd.Context(), apiclientgen.ListAgentTerminalLogsParams{
				ProjectId:  projectID,
				SandboxId:  resolvedSandboxID,
				TerminalId: terminalID,
			})
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.AgentTerminalLogsResponse](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), body)
			}
			return writeAgentTerminalLogs(cmd.OutOrStdout(), body.GetEntries(), includeInput)
		},
	}
	cmd.Flags().BoolVar(&includeInput, "include-input", false, "Include input bytes as well as terminal output")
	return cmd
}

func (a *App) newSandboxTerminalDeleteCommand(sandboxID *string) *cobra.Command {
	return &cobra.Command{
		Use:               "delete TERMINAL_ID",
		Aliases:           []string{"rm", "remove"},
		Short:             "Delete a sandbox terminal",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeTerminals(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveAgentTerminalID(cmd.Context(), client, projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			res, err := client.DeleteAgentTerminal(cmd.Context(), apiclientgen.DeleteAgentTerminalParams{
				ProjectId:  projectID,
				SandboxId:  resolvedSandboxID,
				TerminalId: terminalID,
			})
			if err != nil {
				return err
			}
			if err := expectNoContent[apiclientgen.DeleteAgentTerminalNoContent](res); err != nil {
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

func createAgentTerminalBody(opts sandboxTerminalCreateOptions) (*apimodel.CreateAgentTerminalRequest, error) {
	body := &apimodel.CreateAgentTerminalRequest{}
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
		body.SetEnv(apiclientgen.NewOptCreateAgentTerminalRequestEnv(apiclientgen.CreateAgentTerminalRequestEnv(env)))
	}
	if cols, rows, ok := terminalSize(os.Stdin); ok {
		body.SetCols(apiclientgen.NewOptInt(cols))
		body.SetRows(apiclientgen.NewOptInt(rows))
	}
	return body, nil
}

func (a *App) resolveAgentTerminalID(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, value string) (string, error) {
	id, err := parseIDArg(value, "terminal ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListAgentTerminals(ctx, apiclientgen.ListAgentTerminalsParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.AgentTerminalsResponse](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetTerminals()))
	for _, terminal := range body.GetTerminals() {
		ids = append(ids, terminal.ID)
	}
	return resolveShortID(id, "terminal ID", ids)
}

func (a *App) getAgentTerminal(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, terminalID string) (apimodel.AgentTerminal, error) {
	res, err := client.ListAgentTerminals(ctx, apiclientgen.ListAgentTerminalsParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return apimodel.AgentTerminal{}, err
	}
	body, err := expectResponse[apimodel.AgentTerminalsResponse](res)
	if err != nil {
		return apimodel.AgentTerminal{}, err
	}
	for _, terminal := range body.GetTerminals() {
		if terminal.ID == terminalID {
			return terminal, nil
		}
	}
	return apimodel.AgentTerminal{}, fmt.Errorf("agent terminal %q not found", terminalID)
}

func (a *App) writeAgentTerminal(cmd *cobra.Command, terminal *apimodel.AgentTerminal) error {
	if terminal == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), terminal)
	}
	return a.writeAgentTerminals(cmd, []apimodel.AgentTerminal{*terminal})
}

func (a *App) writeAgentTerminals(cmd *cobra.Command, terminals []apimodel.AgentTerminal) error {
	if a.quiet {
		terminals = sortedByCreatedAt(terminals, func(terminal apimodel.AgentTerminal) time.Time { return terminal.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), terminals, func(terminal apimodel.AgentTerminal) string { return terminal.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"terminals": terminals})
	}
	terminals = sortedByCreatedAt(terminals, func(terminal apimodel.AgentTerminal) time.Time { return terminal.CreatedAt })
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

func writeAgentTerminalLogs(w io.Writer, entries []apimodel.AgentTerminalLogEntry, includeInput bool) error {
	for _, entry := range entries {
		if entry.Stream == apiclientgen.AgentTerminalLogEntryStreamInput && !includeInput {
			continue
		}
		if _, err := w.Write(entry.Data); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) attachAgentTerminal(ctx context.Context, projectID, sandboxID, terminalID string, replay bool, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := a.openAgentTerminalAttach(ctx, projectID, sandboxID, terminalID, replay)
	if err != nil {
		return err
	}
	defer conn.Close()

	session := &framedAttachSession{
		conn:        conn,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		kind:        "agent terminal",
		action:      "attach terminal",
		rawMode:     true,
		resize:      true,
		signalReady: replay,
		copyInput:   copyAgentTerminalInput,
		errorFrame:  printAttachErrorFrame(stderr),
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
	if _, err := a.startAgentTerminal(ctx, projectID, sandboxID, terminalID); err != nil {
		return err
	}
	err = session.run(ctx)
	if errors.Is(err, errAgentTerminalDetached) {
		return nil
	}
	return err
}

func (a *App) startAgentTerminal(ctx context.Context, projectID, sandboxID, terminalID string) (apimodel.AgentTerminal, error) {
	client, err := a.apiClient()
	if err != nil {
		return apimodel.AgentTerminal{}, err
	}
	res, err := client.StartAgentTerminal(ctx, apiclientgen.StartAgentTerminalParams{
		ProjectId:  projectID,
		SandboxId:  sandboxID,
		TerminalId: terminalID,
	})
	if err != nil {
		if isAgentTerminalStartDecodeEOF(err) {
			return a.getAgentTerminal(ctx, client, projectID, sandboxID, terminalID)
		}
		return apimodel.AgentTerminal{}, err
	}
	started, err := expectResponse[apimodel.AgentTerminal](res)
	if err != nil {
		if isAgentTerminalStartDecodeEOF(err) {
			return a.getAgentTerminal(ctx, client, projectID, sandboxID, terminalID)
		}
		return apimodel.AgentTerminal{}, err
	}
	return *started, nil
}

func isAgentTerminalStartDecodeEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "unexpected EOF")
}

func (a *App) openAgentTerminalAttach(ctx context.Context, projectID, sandboxID, terminalID string, replay bool) (io.ReadWriteCloser, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/sandboxes/" + url.PathEscape(sandboxID) + "/agent-terminals/" + url.PathEscape(terminalID) + "/attach"
	if replay {
		u.RawQuery = url.Values{"replay": {"true"}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", agentTerminalProtocol)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("attach terminal: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, fmt.Errorf("attach terminal: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if upgrade := resp.Header.Get("Upgrade"); !strings.EqualFold(upgrade, agentTerminalProtocol) {
		resp.Body.Close()
		return nil, fmt.Errorf("attach terminal: unexpected upgrade protocol %q", upgrade)
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("attach terminal: upgraded response body is not writable")
	}
	return conn, nil
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
