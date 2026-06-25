package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

const (
	agentTerminalProtocol = "discobox-agent-terminal"

	agentTerminalFrameOutput byte = 1
	agentTerminalFrameInput  byte = 2
	agentTerminalFrameResize byte = 3
	agentTerminalFrameSignal byte = 4
	agentTerminalFrameError  byte = 5

	agentTerminalMaxPayload = 16 * 1024 * 1024
)

var errAgentTerminalDetached = errors.New("detached")

type sandboxTerminalCreateOptions struct {
	agentID string
	args    []string
	workdir string
	env     []string
	attach  bool
}

func (a *App) newSandboxTerminalsCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "terminals",
		Aliases: []string{"terminal"},
		Short:   "Manage sandbox terminals",
	}
	cmd.AddCommand(a.newSandboxTerminalListCommand(sandboxID))
	cmd.AddCommand(a.newSandboxTerminalCreateCommand(sandboxID))
	cmd.AddCommand(a.newSandboxTerminalAttachCommand(sandboxID))
	cmd.AddCommand(a.newSandboxTerminalDeleteCommand(sandboxID))
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
				return a.attachAgentTerminal(cmd.Context(), projectID, resolvedSandboxID, terminal.ID, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return a.writeAgentTerminal(cmd, &terminal)
		},
	}
	cmd.Flags().StringVar(&opts.agentID, "agent", "", "Agent ID to start; defaults to the sandbox configured agent")
	cmd.Flags().StringArrayVar(&opts.args, "arg", nil, "Additional command argument; repeat for multiple arguments")
	cmd.Flags().StringVar(&opts.workdir, "workdir", "", "Working directory inside the sandbox")
	cmd.Flags().StringArrayVar(&opts.env, "env", nil, "Environment variable in KEY=VALUE form; repeat for multiple variables")
	cmd.Flags().BoolVar(&opts.attach, "attach", false, "Attach after creating the terminal")
	return cmd
}

func (a *App) newSandboxTerminalAttachCommand(sandboxID *string) *cobra.Command {
	return &cobra.Command{
		Use:   "attach TERMINAL_ID",
		Short: "Attach to a sandbox terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxTerminalRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			terminalID, err := a.resolveAgentTerminalID(cmd.Context(), client, projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			return a.attachAgentTerminal(cmd.Context(), projectID, resolvedSandboxID, terminalID, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func (a *App) newSandboxTerminalDeleteCommand(sandboxID *string) *cobra.Command {
	return &cobra.Command{
		Use:     "delete TERMINAL_ID",
		Aliases: []string{"rm", "remove"},
		Short:   "Delete a sandbox terminal",
		Args:    cobra.ExactArgs(1),
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
	env, err := keyValueMap(opts.env, "env")
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

func keyValueMap(values []string, name string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("%s must be in KEY=VALUE form", name)
		}
		out[key] = val
	}
	return out, nil
}

func (a *App) resolveAgentTerminalID(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, value string) (string, error) {
	id, err := parseIDArg(value, "terminal ID")
	if err != nil || len(id) != shortIDLength {
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

func (a *App) attachAgentTerminal(ctx context.Context, projectID, sandboxID, terminalID string, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := a.openAgentTerminalAttach(ctx, projectID, sandboxID, terminalID)
	if err != nil {
		return err
	}
	defer conn.Close()

	session := &agentTerminalAttachSession{
		conn:   conn,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
	}
	err = session.run(ctx)
	if errors.Is(err, errAgentTerminalDetached) {
		return nil
	}
	return err
}

func (a *App) openAgentTerminalAttach(ctx context.Context, projectID, sandboxID, terminalID string) (io.ReadWriteCloser, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/sandboxes/" + url.PathEscape(sandboxID) + "/agent-terminals/" + url.PathEscape(terminalID) + "/attach"
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

type agentTerminalAttachSession struct {
	conn   io.ReadWriteCloser
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	mu     sync.Mutex
}

func (s *agentTerminalAttachSession) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if file, ok := s.stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		state, err := term.MakeRaw(int(file.Fd()))
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(int(file.Fd()), state) }()
	}

	errCh := make(chan error, 4)
	go func() { errCh <- s.copyOutput() }()
	go func() { errCh <- s.copyInput(ctx) }()
	go func() { errCh <- s.watchResize(ctx, os.Stdin) }()
	go func() { errCh <- s.proxySignals(ctx) }()

	err := <-errCh
	cancel()
	_ = s.conn.Close()
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (s *agentTerminalAttachSession) copyOutput() error {
	for {
		frame, err := readAgentTerminalFrame(s.conn)
		if err != nil {
			return err
		}
		switch frame.typ {
		case agentTerminalFrameOutput:
			if _, err := s.stdout.Write(frame.payload); err != nil {
				return err
			}
		case agentTerminalFrameError:
			message := strings.TrimSpace(string(frame.payload))
			if message != "" {
				_, _ = fmt.Fprintln(s.stderr, message)
			}
			return nil
		default:
			return fmt.Errorf("attach terminal: unexpected frame type %d", frame.typ)
		}
	}
}

func (s *agentTerminalAttachSession) copyInput(ctx context.Context) error {
	buf := make([]byte, 32*1024)
	pendingCtrlP := false
	for {
		n, err := s.stdin.Read(buf)
		if n > 0 {
			payload, detach := filterDetachSequence(buf[:n], &pendingCtrlP)
			if len(payload) > 0 {
				if writeErr := s.writeFrame(agentTerminalFrameInput, payload); writeErr != nil {
					return writeErr
				}
			}
			if detach {
				return errAgentTerminalDetached
			}
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
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

func (s *agentTerminalAttachSession) watchResize(ctx context.Context, file *os.File) error {
	cols, rows, ok := terminalSize(file)
	if ok {
		if err := s.writeResize(cols, rows); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			nextCols, nextRows, ok := terminalSize(file)
			if !ok || (nextCols == cols && nextRows == rows) {
				continue
			}
			cols, rows = nextCols, nextRows
			if err := s.writeResize(cols, rows); err != nil {
				return err
			}
		}
	}
}

func (s *agentTerminalAttachSession) proxySignals(ctx context.Context) error {
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, proxiedSignals()...)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sig := <-signals:
			name, ok := signalName(sig)
			if !ok {
				continue
			}
			if err := s.writeFrame(agentTerminalFrameSignal, []byte(name)); err != nil {
				return err
			}
		}
	}
}

func (s *agentTerminalAttachSession) writeResize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > math.MaxUint16 || rows > math.MaxUint16 {
		return nil
	}
	payload, err := json.Marshal(struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}
	return s.writeFrame(agentTerminalFrameResize, payload)
}

func (s *agentTerminalAttachSession) writeFrame(typ byte, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAgentTerminalFrame(s.conn, typ, payload)
}

func terminalSize(file *os.File) (cols, rows int, ok bool) {
	if file == nil || !term.IsTerminal(int(file.Fd())) {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(int(file.Fd()))
	return cols, rows, err == nil && cols > 0 && rows > 0
}

type agentTerminalFrame struct {
	typ     byte
	payload []byte
}

func writeAgentTerminalFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > agentTerminalMaxPayload {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var header [5]byte
	header[0] = typ
	size := uint32(len(payload)) // #nosec G115 -- payload length is bounded by agentTerminalMaxPayload.
	binary.BigEndian.PutUint32(header[1:], size)
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func readAgentTerminalFrame(r io.Reader) (agentTerminalFrame, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return agentTerminalFrame{}, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > agentTerminalMaxPayload {
		return agentTerminalFrame{}, fmt.Errorf("frame payload too large: %d", size)
	}
	payload := make([]byte, int(size))
	if size > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return agentTerminalFrame{}, err
		}
	}
	return agentTerminalFrame{typ: header[0], payload: payload}, nil
}
