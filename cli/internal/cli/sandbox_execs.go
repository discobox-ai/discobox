package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type sandboxExecCreateOptions struct {
	workdir     string
	env         []string
	uid         string
	gid         string
	tty         bool
	interactive bool
	detach      bool
}

func (a *App) newSandboxExecCommand() *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:     "exec",
		Aliases: []string{"execs"},
		Short:   "Manage sandbox exec commands",
	}
	cmd.PersistentFlags().StringVar(&sandboxID, "sandbox-id", "", "Sandbox ID")
	_ = cmd.RegisterFlagCompletionFunc("sandbox-id", a.completeSandboxes)
	cmd.AddCommand(a.newSandboxExecCreateCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxExecListCommand(&sandboxID))
	cmd.AddCommand(a.newSandboxExecLogsCommand(&sandboxID))
	return cmd
}

func (a *App) newSandboxExecCreateCommand(sandboxID *string) *cobra.Command {
	var opts sandboxExecCreateOptions
	cmd := &cobra.Command{
		Use:   "create [flags] [--] COMMAND [ARG...]",
		Short: "Create a sandbox exec",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, _, err := a.sandboxExecRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			body, err := createSandboxExecBody(opts, args)
			if err != nil {
				return err
			}
			exec, err := a.createSandboxExec(cmd.Context(), projectID, resolvedSandboxID, body)
			if err != nil {
				return err
			}
			if opts.detach || a.output == "json" {
				exec, err = a.startSandboxExec(cmd.Context(), projectID, resolvedSandboxID, exec.ID)
				if err != nil {
					return err
				}
				return a.writeSandboxExec(cmd, &exec)
			}
			if err := a.attachSandboxExec(cmd.Context(), projectID, resolvedSandboxID, exec.ID, opts.interactive, opts.tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				return err
			}
			return a.returnSandboxExecStatus(cmd.Context(), projectID, resolvedSandboxID, exec.ID)
		},
	}
	cmd.Flags().StringVar(&opts.workdir, "workdir", "", "Working directory inside the sandbox")
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables")
	cmd.Flags().StringVar(&opts.uid, "uid", "", "User ID to run as inside the sandbox")
	cmd.Flags().StringVar(&opts.gid, "gid", "", "Group ID to run as inside the sandbox")
	cmd.Flags().BoolVarP(&opts.tty, "tty", "t", false, "Allocate a PTY")
	cmd.Flags().BoolVarP(&opts.interactive, "interactive", "i", false, "Accepted for docker exec -it compatibility")
	cmd.Flags().BoolVarP(&opts.detach, "detach", "d", false, "Start the exec and print its record without streaming logs")
	return cmd
}

func (a *App) newSandboxExecListCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sandbox execs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, resolvedSandboxID, _, err := a.sandboxExecRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			execs, err := a.listSandboxExecs(cmd.Context(), projectID, resolvedSandboxID)
			if err != nil {
				return err
			}
			return a.writeSandboxExecs(cmd, execs)
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSandboxExecLogsCommand(sandboxID *string) *cobra.Command {
	var includeInput bool
	cmd := &cobra.Command{
		Use:               "logs EXEC_ID",
		Short:             "Print sandbox exec logs",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeExecs(sandboxID),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, resolvedSandboxID, client, err := a.sandboxExecRequest(cmd.Context(), *sandboxID)
			if err != nil {
				return err
			}
			execID, err := a.resolveSandboxExecID(cmd.Context(), projectID, resolvedSandboxID, args[0])
			if err != nil {
				return err
			}
			res, err := client.ListSandboxExecLogs(cmd.Context(), apiclientgen.ListSandboxExecLogsParams{
				ProjectId: projectID,
				SandboxId: resolvedSandboxID,
				ExecId:    execID,
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
	cmd.Flags().BoolVar(&includeInput, "include-input", false, "Include input bytes as well as exec output")
	return cmd
}

func (a *App) sandboxExecRequest(ctx context.Context, sandboxArg string) (string, string, *apiclientgen.Client, error) {
	if strings.TrimSpace(sandboxArg) == "" {
		return "", "", nil, fmt.Errorf("--sandbox-id is required")
	}
	return a.sandboxRequest(ctx, sandboxArg)
}

func createSandboxExecBody(opts sandboxExecCreateOptions, command []string) (*apimodel.CreateSandboxExecRequest, error) {
	body := &apimodel.CreateSandboxExecRequest{}
	body.SetCommand(append([]string{}, command...))
	body.SetWorkdir(optString(opts.workdir))
	env, err := keyValueMapFromShell(opts.env)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		body.SetEnv(apiclientgen.NewOptCreateSandboxExecRequestEnv(apiclientgen.CreateSandboxExecRequestEnv(env)))
	}
	uid, err := parseOptionalInt64(opts.uid, "uid")
	if err != nil {
		return nil, err
	}
	if uid != nil {
		body.SetUID(apiclientgen.NewOptInt64(*uid))
	}
	gid, err := parseOptionalInt64(opts.gid, "gid")
	if err != nil {
		return nil, err
	}
	if gid != nil {
		body.SetGid(apiclientgen.NewOptInt64(*gid))
	}
	body.SetTty(apiclientgen.NewOptBool(opts.tty))
	if opts.tty {
		if cols, rows, ok := terminalSize(os.Stdin); ok {
			body.SetCols(apiclientgen.NewOptInt(cols))
			body.SetRows(apiclientgen.NewOptInt(rows))
		}
	}
	return body, nil
}

func keyValueMapFromShell(values []string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("env must be in KEY=VALUE or KEY form")
		}
		if ok {
			out[key] = val
			continue
		}
		if shellValue, exists := os.LookupEnv(key); exists {
			out[key] = shellValue
		}
	}
	return out, nil
}

func parseOptionalInt64(value, name string) (*int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer", name)
	}
	return &parsed, nil
}

func (a *App) resolveSandboxExecID(ctx context.Context, projectID, sandboxID, value string) (string, error) {
	id, err := parseIDArg(value, "exec ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	execs, err := a.listSandboxExecs(ctx, projectID, sandboxID)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(execs))
	for _, exec := range execs {
		ids = append(ids, exec.ID)
	}
	return resolveShortID(id, "exec ID", ids)
}

func (a *App) writeSandboxExec(cmd *cobra.Command, exec *apimodel.SandboxExec) error {
	if exec == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), exec)
	}
	return a.writeSandboxExecs(cmd, []apimodel.SandboxExec{*exec})
}

func (a *App) writeSandboxExecs(cmd *cobra.Command, execs []apimodel.SandboxExec) error {
	if a.quiet {
		execs = sortedByCreatedAt(execs, func(exec apimodel.SandboxExec) time.Time { return exec.CreatedAt })
		return writeResourceIDs(cmd.OutOrStdout(), execs, func(exec apimodel.SandboxExec) string { return exec.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"execs": execs})
	}
	execs = sortedByCreatedAt(execs, func(exec apimodel.SandboxExec) time.Time { return exec.CreatedAt })
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tPID\tEXIT\tTTY\tWORKDIR\tCOMMAND\tCREATED")
	for _, exec := range execs {
		pid := ""
		if value, ok := exec.Pid.Get(); ok {
			pid = fmt.Sprint(value)
		}
		exitCode := ""
		if value, ok := exec.ExitCode.Get(); ok {
			exitCode = fmt.Sprint(value)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			shortID(exec.ID),
			exec.Status,
			pid,
			exitCode,
			exec.Tty,
			truncateTableValue(exec.Workdir, 36),
			truncateTableValue(strings.Join(exec.Command, " "), 48),
			formatTime(exec.CreatedAt),
		)
	}
	return tw.Flush()
}

func (a *App) attachSandboxExec(ctx context.Context, projectID, sandboxID, execID string, interactive, tty bool, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := a.openSandboxExecAttach(ctx, projectID, sandboxID, execID)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := a.startSandboxExec(ctx, projectID, sandboxID, execID); err != nil {
		return err
	}

	session := &sandboxExecAttachSession{
		conn:        conn,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		interactive: interactive,
		tty:         tty,
	}
	return session.run(ctx)
}

func (a *App) openSandboxExecAttach(ctx context.Context, projectID, sandboxID, execID string) (io.ReadWriteCloser, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("attach exec: unsupported websocket base URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/sandboxes/" + url.PathEscape(sandboxID) + "/execs/" + url.PathEscape(execID) + "/attach"
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if len(body) > 0 {
				return nil, fmt.Errorf("attach exec: %s: %s", resp.Status, strings.TrimSpace(string(body)))
			}
			return nil, fmt.Errorf("attach exec: %s", resp.Status)
		}
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	return websocket.NetConn(ctx, conn, websocket.MessageBinary), nil
}

type sandboxExecRecordResponse struct {
	Exec sandboxExecRecord `json:"exec"`
}

type sandboxExecRecordsResponse struct {
	Execs []sandboxExecRecord `json:"execs"`
}

type sandboxExecRecord struct {
	ID        string                         `json:"id"`
	Status    apiclientgen.SandboxExecStatus `json:"status"`
	Command   []string                       `json:"command"`
	Workdir   string                         `json:"workdir"`
	Env       map[string]string              `json:"env"`
	UID       *int64                         `json:"uid"`
	Gid       *int64                         `json:"gid"`
	Tty       bool                           `json:"tty"`
	Unit      *string                        `json:"unit"`
	Pid       *int64                         `json:"pid"`
	ExitCode  *int64                         `json:"exitCode"`
	Error     *string                        `json:"error"`
	CreatedAt time.Time                      `json:"createdAt"`
	StartedAt *time.Time                     `json:"startedAt"`
	ExitedAt  *time.Time                     `json:"exitedAt"`
	Metadata  map[string]string              `json:"metadata"`
}

func (a *App) createSandboxExec(ctx context.Context, projectID, sandboxID string, body *apimodel.CreateSandboxExecRequest) (apimodel.SandboxExec, error) {
	var response sandboxExecRecordResponse
	if err := a.execJSON(ctx, http.MethodPost, projectID, sandboxID, "", body, &response); err != nil {
		return apimodel.SandboxExec{}, err
	}
	return response.Exec.model(), nil
}

func (a *App) getSandboxExec(ctx context.Context, projectID, sandboxID, execID string) (*apimodel.SandboxExec, error) {
	var response sandboxExecRecord
	if err := a.execJSON(ctx, http.MethodGet, projectID, sandboxID, "/"+url.PathEscape(execID), nil, &response); err != nil {
		return nil, err
	}
	exec := response.model()
	return &exec, nil
}

func (a *App) listSandboxExecs(ctx context.Context, projectID, sandboxID string) ([]apimodel.SandboxExec, error) {
	var response sandboxExecRecordsResponse
	if err := a.execJSON(ctx, http.MethodGet, projectID, sandboxID, "", nil, &response); err != nil {
		return nil, err
	}
	execs := make([]apimodel.SandboxExec, 0, len(response.Execs))
	for _, exec := range response.Execs {
		execs = append(execs, exec.model())
	}
	return execs, nil
}

func (a *App) startSandboxExec(ctx context.Context, projectID, sandboxID, execID string) (apimodel.SandboxExec, error) {
	var response sandboxExecRecord
	if err := a.execJSON(ctx, http.MethodPost, projectID, sandboxID, "/"+url.PathEscape(execID)+"/start", nil, &response); err != nil {
		if errors.Is(err, io.EOF) {
			exec, getErr := a.getSandboxExec(ctx, projectID, sandboxID, execID)
			if getErr != nil {
				return apimodel.SandboxExec{}, err
			}
			return *exec, nil
		}
		return apimodel.SandboxExec{}, err
	}
	return response.model(), nil
}

func (a *App) execJSON(ctx context.Context, method, projectID, sandboxID, suffix string, in, out any) error {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/sandboxes/" + url.PathEscape(sandboxID) + "/execs" + suffix

	var body io.Reader
	if in != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(in); err != nil {
			return err
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return fmt.Errorf("request failed: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r sandboxExecRecord) model() apimodel.SandboxExec {
	exec := apimodel.SandboxExec{
		ID:        r.ID,
		Status:    r.Status,
		Command:   append([]string{}, r.Command...),
		Workdir:   r.Workdir,
		Tty:       r.Tty,
		CreatedAt: r.CreatedAt,
	}
	if r.Env != nil {
		exec.SetEnv(apiclientgen.NewOptSandboxExecEnv(apiclientgen.SandboxExecEnv(r.Env)))
	}
	if r.UID != nil {
		exec.SetUID(apiclientgen.NewOptInt64(*r.UID))
	}
	if r.Gid != nil {
		exec.SetGid(apiclientgen.NewOptInt64(*r.Gid))
	}
	if r.Unit != nil {
		exec.SetUnit(apiclientgen.NewOptString(*r.Unit))
	}
	if r.Pid != nil {
		exec.SetPid(apiclientgen.NewOptInt64(*r.Pid))
	}
	if r.ExitCode != nil {
		exec.SetExitCode(apiclientgen.NewOptInt64(*r.ExitCode))
	}
	if r.Error != nil {
		exec.SetError(apiclientgen.NewOptString(*r.Error))
	}
	if r.StartedAt != nil {
		exec.SetStartedAt(apiclientgen.NewOptDateTime(*r.StartedAt))
	}
	if r.ExitedAt != nil {
		exec.SetExitedAt(apiclientgen.NewOptDateTime(*r.ExitedAt))
	}
	if r.Metadata != nil {
		exec.SetMetadata(apiclientgen.NewOptSandboxExecMetadata(apiclientgen.SandboxExecMetadata(r.Metadata)))
	}
	return exec
}

type sandboxExecAttachSession struct {
	conn        io.ReadWriteCloser
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	interactive bool
	tty         bool
	mu          sync.Mutex
}

func (s *sandboxExecAttachSession) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if s.interactive && s.tty {
		if file, ok := s.stdin.(*os.File); ok && terminalSizeIsTTY(file) {
			state, err := makeRawTerminal(file)
			if err != nil {
				return err
			}
			defer restoreTerminal(file, state)
		}
	}

	outputErr := make(chan error, 1)
	otherErr := make(chan error, 3)
	go func() { outputErr <- s.copyOutput() }()
	go func() {
		if err := s.copyInput(ctx); err != nil {
			otherErr <- err
		}
	}()
	if s.tty {
		go func() {
			if err := s.watchResize(ctx, os.Stdin); err != nil {
				otherErr <- err
			}
		}()
	}
	go func() {
		if err := s.proxySignals(ctx); err != nil {
			otherErr <- err
		}
	}()

	for {
		select {
		case err := <-outputErr:
			cancel()
			_ = s.conn.Close()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case err := <-otherErr:
			cancel()
			_ = s.conn.Close()
			if isAttachDone(err) {
				return nil
			}
			return err
		case <-ctx.Done():
			_ = s.conn.Close()
			return ctx.Err()
		}
	}
}

func terminalSizeIsTTY(file *os.File) bool {
	_, _, ok := terminalSize(file)
	return ok
}

func makeRawTerminal(file *os.File) (*term.State, error) {
	return term.MakeRaw(int(file.Fd()))
}

func restoreTerminal(file *os.File, state *term.State) {
	_ = term.Restore(int(file.Fd()), state)
}

func (s *sandboxExecAttachSession) copyOutput() error {
	for {
		frame, err := readAgentTerminalFrame(s.conn)
		if err != nil {
			return err
		}
		switch frame.typ {
		case attachFrameOutput:
			if _, err := s.stdout.Write(frame.payload); err != nil {
				return err
			}
		case attachFrameError:
			return nil
		case attachFrameExit:
			return attachExitErrorFromPayload("sandbox exec", frame.payload)
		default:
			return fmt.Errorf("attach exec: unexpected frame type %d", frame.typ)
		}
	}
}

func (s *sandboxExecAttachSession) copyInput(ctx context.Context) error {
	if !s.interactive {
		return s.closeInput()
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := s.stdin.Read(buf)
		if n > 0 {
			if writeErr := s.writeFrame(attachFrameInput, buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return s.closeInput()
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

func (s *sandboxExecAttachSession) closeInput() error {
	return s.writeFrame(attachFrameCloseInput, nil)
}

func (s *sandboxExecAttachSession) watchResize(ctx context.Context, file *os.File) error {
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
			return nil
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

func (s *sandboxExecAttachSession) proxySignals(ctx context.Context) error {
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, proxiedSignals()...)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-signals:
			name, ok := signalName(sig)
			if !ok {
				continue
			}
			if err := s.writeFrame(attachFrameSignal, []byte(name)); err != nil {
				return err
			}
		}
	}
}

func (s *sandboxExecAttachSession) writeResize(cols, rows int) error {
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
	return s.writeFrame(attachFrameResize, payload)
}

func (s *sandboxExecAttachSession) writeFrame(typ byte, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeAgentTerminalFrame(s.conn, typ, payload)
}

func (a *App) returnSandboxExecStatus(ctx context.Context, projectID, sandboxID, execID string) error {
	exec, err := a.getSandboxExec(ctx, projectID, sandboxID, execID)
	if err != nil {
		return err
	}
	if code, ok := exec.ExitCode.Get(); ok && code != 0 {
		return exitCodeError{code: int(code)}
	}
	if exec.Status == apiclientgen.SandboxExecStatusFailed {
		return fmt.Errorf("exec %s failed: %s", shortID(exec.ID), exec.Error.Or(""))
	}
	if exec.Status == apiclientgen.SandboxExecStatusLost {
		return fmt.Errorf("exec %s lost: %s", shortID(exec.ID), exec.Error.Or(""))
	}
	return nil
}

func writeSandboxExecLogs(stdout, stderr io.Writer, entries []apimodel.SandboxExecLogEntry, includeInput bool) error {
	for _, entry := range entries {
		switch entry.Stream {
		case apiclientgen.SandboxExecLogEntryStreamInput:
			if !includeInput {
				continue
			}
			if _, err := stdout.Write(entry.Data); err != nil {
				return err
			}
		case apiclientgen.SandboxExecLogEntryStreamStderr:
			if _, err := stderr.Write(entry.Data); err != nil {
				return err
			}
		default:
			if _, err := stdout.Write(entry.Data); err != nil {
				return err
			}
		}
	}
	return nil
}
