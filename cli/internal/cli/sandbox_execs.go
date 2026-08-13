package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/obot-platform/discobox/execstream"
	"github.com/obot-platform/discobox/execstream/client"
	"github.com/obot-platform/discobox/execstream/frame"
	"github.com/obot-platform/discobox/execstream/resume"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type sandboxExecCreateOptions struct {
	workdir     string
	env         []string
	user        string
	uid         string
	gid         []string
	tty         bool
	interactive bool
	detach      bool
	shell       bool
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
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !opts.shell {
				return fmt.Errorf("a command is required unless --shell is set")
			}
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
	cmd.Flags().StringVar(&opts.user, "user", "", "User name or UID[:GID] to run as inside the sandbox")
	cmd.Flags().StringVar(&opts.uid, "uid", "", "User ID to run as inside the sandbox")
	cmd.Flags().StringSliceVar(&opts.gid, "gid", nil, "Groups to run as inside the sandbox, each a name or a numeric GID. The first is the primary group and the rest are supplementary; omit to inherit the sandbox's own groups")
	cmd.Flags().BoolVar(&opts.shell, "shell", false, "Run the sandbox user's login shell instead of a command; the sandbox resolves which shell that is")
	cmd.Flags().BoolVarP(&opts.tty, "tty", "t", false, "Allocate a PTY")
	cmd.Flags().BoolVarP(&opts.interactive, "interactive", "i", false, "Accepted for docker exec -it compatibility")
	cmd.Flags().BoolVarP(&opts.detach, "detach", "d", false, "Start the exec and print its record without streaming logs")
	return cmd
}

func (a *App) newSandboxExecListCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List sandbox execs",
		Args:    cobra.NoArgs,
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
	// Which shell to run is the sandbox's answer, not this machine's: the local
	// $SHELL says nothing about the run user inside the sandbox.
	if opts.shell {
		if len(command) > 0 {
			return nil, fmt.Errorf("a command cannot be combined with --shell")
		}
		body.SetShell(apiclientgen.NewOptBool(true))
	} else {
		body.SetCommand(append([]string{}, command...))
	}
	body.SetWorkdir(optString(opts.workdir))
	env, err := keyValueMapFromShell(opts.env)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		body.SetEnv(apiclientgen.NewOptCreateSandboxExecRequestEnv(apiclientgen.CreateSandboxExecRequestEnv(env)))
	}
	user, err := sandboxExecUserFromOptions(opts)
	if err != nil {
		return nil, err
	}
	if user != nil {
		body.SetUser(apiclientgen.NewOptSandboxUser(*user))
	}
	body.SetTty(apiclientgen.NewOptBool(opts.tty))
	if opts.tty {
		if cols, rows, ok := client.NewOSConsole(os.Stdin).Size(); ok {
			body.SetCols(apiclientgen.NewOptInt(cols))
			body.SetRows(apiclientgen.NewOptInt(rows))
		}
	}
	return body, nil
}

func sandboxExecUserFromOptions(opts sandboxExecCreateOptions) (*apimodel.SandboxUser, error) {
	user := &apimodel.SandboxUser{}
	if value := strings.TrimSpace(opts.user); value != "" {
		if err := applySandboxExecUserFlag(user, value); err != nil {
			return nil, err
		}
	}
	uid, err := parseOptionalInt64(opts.uid, "uid")
	if err != nil {
		return nil, err
	}
	if uid != nil {
		user.SetUID(apiclientgen.NewOptInt64(*uid))
	}
	if err := applySandboxExecGroups(user, opts.gid); err != nil {
		return nil, err
	}
	if !user.Name.Set && !user.UID.Set && !user.Gid.Set && !user.GroupName.Set &&
		!user.HomeDirectory.Set && len(user.AdditionalGroups) == 0 {
		return nil, nil
	}
	return user, nil
}

// applySandboxExecGroups splits --gid into the primary group and the
// supplementary ones. Each entry is a group name or a numeric GID; the sandbox
// resolves both the same way, since only it has the group file (ADR 0025 §3).
//
// An empty list means "inherit the sandbox's groups" -- groups are
// all-or-nothing, so naming any replaces them all (ADR 0025 §2).
func applySandboxExecGroups(user *apimodel.SandboxUser, values []string) error {
	groups := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			groups = append(groups, value)
		}
	}
	if len(groups) == 0 {
		return nil
	}
	primary := groups[0]
	if user.Gid.Set {
		return fmt.Errorf("group %q conflicts with the group already given by --user", primary)
	}
	if parsed, ok := parseOptionalUserID(primary); ok {
		user.SetGid(apiclientgen.NewOptInt64(parsed))
	} else {
		user.SetGroupName(apiclientgen.NewOptString(primary))
	}
	if len(groups) > 1 {
		user.SetAdditionalGroups(append([]string(nil), groups[1:]...))
	}
	return nil
}

func applySandboxExecUserFlag(user *apimodel.SandboxUser, value string) error {
	nameOrUID, group, hasGroup := strings.Cut(value, ":")
	nameOrUID = strings.TrimSpace(nameOrUID)
	if nameOrUID == "" {
		return fmt.Errorf("user must include a username or UID")
	}
	if parsed, ok := parseOptionalUserID(nameOrUID); ok {
		user.SetUID(apiclientgen.NewOptInt64(parsed))
	} else {
		user.SetName(apiclientgen.NewOptString(nameOrUID))
	}
	if hasGroup {
		group = strings.TrimSpace(group)
		if group == "" {
			return fmt.Errorf("user group must include a GID")
		}
		parsed, ok := parseOptionalUserID(group)
		if !ok {
			return fmt.Errorf("user group must be a numeric GID")
		}
		user.SetGid(apiclientgen.NewOptInt64(parsed))
	}
	return nil
}

func parseOptionalUserID(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	return parsed, err == nil
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

// primaryExecID is the virtual exec id that resolves, on the sandbox-agent, to
// the sandbox's current primary (default) terminal and relaunches it with the
// harness's relaunch command when it has stopped. It must match
// terminal.PrimaryExecID in the sandbox-agent; the control plane proxies exec
// ids opaquely, so the client just uses this value in place of a real id.
const primaryExecID = "primary"

func (a *App) resolveSandboxExecID(ctx context.Context, projectID, sandboxID, value string) (string, error) {
	id, err := parseIDArg(value, "exec ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	// The virtual primary id is resolved by the sandbox-agent, not here: it names
	// whichever exec is the current primary terminal, and attaching it relaunches
	// a stopped one. Matching it against the exec listing would only fail.
	if id == primaryExecID {
		return id, nil
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
	execs = sortedByRecency(execs, func(exec apimodel.SandboxExec) time.Time { return exec.CreatedAt })
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), execs, func(exec apimodel.SandboxExec) string { return exec.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"execs": execs})
	}
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
			exec.ID,
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

// attachSandboxExec attaches to a plain (non-terminal) exec. A TTY exec
// reconnects on a dropped websocket exactly like a terminal attach does — the
// sandbox-agent's replay repaints the current screen, which is well-defined
// for a PTY. A non-TTY exec has no such buffer to replay (no byte-exact
// resumption of piped stdout), so it stays on the direct, fail-on-disconnect
// transport.
func (a *App) attachSandboxExec(ctx context.Context, projectID, sandboxID, execID string, interactive, tty bool, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := a.openExecAttachConn(ctx, projectID, sandboxID, execID, tty)
	if err != nil {
		return a.execAttachError(ctx, projectID, sandboxID, execID, err)
	}
	defer conn.Close()

	opts := client.Options{
		Conn:    conn,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		Console: client.NewOSConsole(stdin),
		Kind:    "sandbox exec",
		Action:  "attach exec",
		RawMode: interactive && tty,
		Resize:  tty,
		CopyInput: func(ctx context.Context, s *client.Session) error {
			return copySandboxExecInput(ctx, s, interactive)
		},
	}
	if tty {
		// SignalReady pairs with the replay this dial asked for: the remote must
		// not stream replay history before the output reader is running. OtherErr
		// keeps a transient close in the auxiliary goroutines (resize, signals,
		// input) from ending the session while the resumable Conn is still
		// reconnecting underneath them — only copyOutput's own read failure, via
		// the Done callback, gets to decide the attach is really over.
		opts.SignalReady = true
		opts.OtherErr = func(err error) (bool, error) {
			if client.IsDone(err) {
				return true, nil
			}
			return false, err
		}
	}
	session := client.New(opts)
	if tty {
		if err := session.WriteInitialResize(); err != nil {
			return err
		}
	}
	if _, err := a.startSandboxExec(ctx, projectID, sandboxID, execID); err != nil {
		return err
	}
	return session.Run(ctx)
}

// openExecAttachConn opens the attach connection for a plain exec: the
// reconnecting, replay transport for a TTY, the direct one otherwise.
func (a *App) openExecAttachConn(ctx context.Context, projectID, sandboxID, execID string, tty bool) (execstream.Conn, error) {
	if !tty {
		return a.openSandboxExecAttach(ctx, projectID, sandboxID, execID, false)
	}
	return a.openReconnectingSandboxExecAttach(ctx, projectID, sandboxID, execID, execAttachOptions{replay: true})
}

// attachRejectedError is an attach the sandbox-agent refused with an HTTP
// status. It carries the agent's own explanation, which for a terminal
// condition (an ended session, a missing exec) is more useful than anything the
// client can infer, so it is reported as-is.
type attachRejectedError struct {
	status     int
	statusText string
	message    string
}

func (e attachRejectedError) Error() string {
	if e.message == "" {
		return "attach exec: " + e.statusText
	}
	return "attach exec: " + e.message
}

// explained reports whether the agent gave a definitive reason, so the client
// does not append its own guess at what went wrong.
func (e attachRejectedError) explained() bool {
	return e.message != "" && (e.status == http.StatusConflict || e.status == http.StatusNotFound)
}

// attachErrorMessage unwraps the agent's JSON error envelope, falling back to
// the raw body for a response that is not one.
func attachErrorMessage(body []byte) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && strings.TrimSpace(envelope.Error) != "" {
		return strings.TrimSpace(envelope.Error)
	}
	return strings.TrimSpace(string(body))
}

// execAttachError checks the authoritative exec record after an attach cannot
// be opened. A missing shim socket commonly means the command already exited;
// reporting only the transport error hides the command's actual result.
func (a *App) execAttachError(ctx context.Context, projectID, sandboxID, execID string, attachErr error) error {
	var rejected attachRejectedError
	if errors.As(attachErr, &rejected) && rejected.explained() {
		return attachErr
	}
	exec, err := a.getSandboxExec(ctx, projectID, sandboxID, execID)
	if err != nil {
		return attachErr
	}
	switch exec.Status {
	case apiclientgen.SandboxExecStatusExited, apiclientgen.SandboxExecStatusFailed, apiclientgen.SandboxExecStatusLost:
		detail := fmt.Sprintf("exec %s is %s", exec.ID, exec.Status)
		if code, ok := exec.ExitCode.Get(); ok {
			detail += fmt.Sprintf(" with exit code %d", code)
		}
		if message := strings.TrimSpace(exec.Error.Or("")); message != "" {
			detail += ": " + message
		}
		return fmt.Errorf("%w; %s", attachErr, detail)
	default:
		return attachErr
	}
}

func (a *App) openSandboxExecAttach(ctx context.Context, projectID, sandboxID, execID string, replay bool) (execstream.Conn, error) {
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
	if replay {
		u.RawQuery = "replay=true"
	}
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			return nil, attachRejectedError{status: resp.StatusCode, statusText: resp.Status, message: attachErrorMessage(body)}
		}
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	netConn := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	frames := &websocketAttachFrames{
		directAttachFrames: &directAttachFrames{conn: netConn},
		socket:             conn,
	}
	go func() {
		if err := pingAttachWebSocket(ctx, conn); err != nil {
			// The server side is gone; close the conn so the attach session's
			// blocked reads and writes unblock instead of hanging forever.
			_ = netConn.Close()
		}
	}()
	return frames, nil
}

// execAttachOptions tunes a reconnecting attach.
type execAttachOptions struct {
	// replay repaints the session's current screen on connect.
	replay bool
	// event receives transport lifecycle changes.
	event func(resume.Event)
	// timing receives transport heartbeat and positioned-action RTT samples from
	// the shared resumable stream.
	timing resume.TimingOptions
}

func (a *App) openReconnectingSandboxExecAttach(
	ctx context.Context,
	projectID, sandboxID, execID string,
	opts execAttachOptions,
) (*resume.Conn, error) {
	dial := func(ctx context.Context) (execstream.Conn, error) {
		conn, err := a.openSandboxExecAttach(ctx, projectID, sandboxID, execID, opts.replay)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	done := func(ctx context.Context) (bool, error) {
		return a.sandboxExecAttachDone(ctx, projectID, sandboxID, execID)
	}
	return resume.New(ctx, conn, resume.Options{
		Dial:   dial,
		Done:   done,
		Event:  opts.event,
		Timing: opts.timing,
	})
}

// sandboxExecAttachDone decides whether a broken attach should reconnect or end.
// The question it answers is whether the command itself finished or the runtime
// disappeared underneath it: a recorded exit is the command's own result and
// ends the session, while a lost unit is an ungraceful disappearance that the
// attach can recover from.
func (a *App) sandboxExecAttachDone(ctx context.Context, projectID, sandboxID, execID string) (bool, error) {
	exec, err := a.getSandboxExec(ctx, projectID, sandboxID, execID)
	if err != nil {
		// The exec read failed. A stopped or stopping sandbox took the terminal with
		// it, so end the attach instead of reconnecting forever; we never start the
		// sandbox back up here (a future `disco attach` will own autostart). Any
		// other failure does not prove the terminal ended — a transient control-plane
		// blip — so keep retrying the websocket.
		if done, stopErr := a.sandboxStoppedAttachDone(ctx, projectID, sandboxID); done {
			return true, stopErr
		}
		return false, err
	}
	switch exec.Status {
	case apiclientgen.SandboxExecStatusExited:
		if code, ok := exec.ExitCode.Get(); ok && code != 0 {
			return true, client.ExitError{Code: int(code)}
		}
		return true, nil
	case apiclientgen.SandboxExecStatusLost:
		// The unit vanished without the shim recording an exit — a killed or
		// restarted container, not a command that ended. Reconnecting relaunches it,
		// but only through the virtual primary id: a concrete exec id names one
		// session, and nothing will ever revive it.
		if execID == primaryExecID {
			return false, nil
		}
		return true, execAttachStatusError(exec)
	case apiclientgen.SandboxExecStatusFailed:
		// The launch itself failed, so retrying would fail identically.
		return true, execAttachStatusError(exec)
	default:
		return false, nil
	}
}

// sandboxStoppedAttachDone reports whether the sandbox itself has stopped (or is
// stopping or failed), which ends an attach: the terminal cannot outlive its
// sandbox, and reconnecting would loop against a runtime that is gone. A read
// that cannot establish this returns not-done, leaving the failure retriable.
func (a *App) sandboxStoppedAttachDone(ctx context.Context, projectID, sandboxID string) (bool, error) {
	sandbox, ok := a.sandboxSnapshot(ctx, projectID, sandboxID)
	if !ok {
		return false, nil
	}
	switch sandbox.Runtime.RuntimeState.Or("") {
	case sandboxRuntimeStateStopped, sandboxRuntimeStateStopping:
		return true, fmt.Errorf("sandbox %s is %s; detaching terminal", sandboxID, sandbox.Runtime.RuntimeState.Or(""))
	}
	if sandbox.Runtime.State == sandboxStateFailed {
		return true, fmt.Errorf("sandbox failed: %s", sandboxFailureReason(sandbox))
	}
	return false, nil
}

// sandboxSnapshot fetches the sandbox once, returning ok=false when its state
// cannot be read. Callers that only act on a definitive state treat an
// unreadable sandbox as "unknown" rather than any particular phase.
func (a *App) sandboxSnapshot(ctx context.Context, projectID, sandboxID string) (*apimodel.Sandbox, bool) {
	client, err := a.apiClient()
	if err != nil {
		return nil, false
	}
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return nil, false
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return nil, false
	}
	return sandbox, true
}

func execAttachStatusError(exec *apimodel.SandboxExec) error {
	if message := strings.TrimSpace(exec.Error.Or("")); message != "" {
		return errors.New(message)
	}
	return fmt.Errorf("terminal is %s", exec.Status)
}

const (
	// The container being down is a runtime fact and a sandbox that could not
	// be built is an existence one, so the two live on different fields
	// (ADR 0034).
	sandboxRuntimeStateStopped  = "stopped"
	sandboxRuntimeStateStopping = "stopping"
	sandboxStateFailed          = "failed"
)

// attachPingInterval paces websocket keepalive pings on an exec attach. The
// pings keep an idle attach alive across NATs and proxies and detect a dead
// server, which an idle attach stream never would.
const attachPingInterval = 30 * time.Second

// attachPingTimeout bounds how long a ping waits for the server's pong before
// the connection is considered dead.
const attachPingTimeout = 10 * time.Second

// pingAttachWebSocket sends keepalive pings every attachPingInterval until ctx
// is canceled, returning an error when a ping goes unanswered for
// attachPingTimeout. Pongs are processed by the attach session's read loop.
func pingAttachWebSocket(ctx context.Context, conn *websocket.Conn) error {
	return pingAttachWebSocketWithIntervals(ctx, conn, attachPingInterval, attachPingTimeout)
}

func pingAttachWebSocketWithIntervals(ctx context.Context, conn *websocket.Conn, interval, timeout time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
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
	User      *sandboxExecUser               `json:"user"`
	Tty       bool                           `json:"tty"`
	Unit      *string                        `json:"unit"`
	Pid       *int64                         `json:"pid"`
	ExitCode  *int64                         `json:"exitCode"`
	Error     *string                        `json:"error"`
	CreatedAt time.Time                      `json:"createdAt"`
	StartedAt *time.Time                     `json:"startedAt"`
	ExitedAt  *time.Time                     `json:"exitedAt"`
	Metadata  map[string]string              `json:"metadata"`
	HarnessID *string                        `json:"harnessId"`
	Primary   *bool                          `json:"primary"`
}

type sandboxExecUser struct {
	Name          *string `json:"name"`
	UID           *int64  `json:"uid"`
	Gid           *int64  `json:"gid"`
	HomeDirectory *string `json:"homeDirectory"`
}

// sandboxCommandOutput runs command inside the sandbox and returns everything
// it wrote once it exits, rather than streaming it.
//
// This is for the callers that have to read the output before they can act on
// it — `disco apply`'s dirty-tree check — where
// an attach would hand them bytes they can only buffer themselves. An exit code
// of -1 means the exec ended without recording one.
func (a *App) sandboxCommandOutput(ctx context.Context, projectID, sandboxID, workdir string, command []string) (stdout, stderr string, exitCode int, err error) {
	body := &apimodel.CreateSandboxExecRequest{}
	body.SetCommand(append([]string{}, command...))
	body.SetWorkdir(optString(workdir))
	exec, err := a.createSandboxExec(ctx, projectID, sandboxID, body)
	if err != nil {
		return "", "", -1, err
	}
	if _, err := a.startSandboxExec(ctx, projectID, sandboxID, exec.ID); err != nil {
		return "", "", -1, err
	}
	final, err := a.waitSandboxExecExit(ctx, projectID, sandboxID, exec.ID)
	if err != nil {
		return "", "", -1, err
	}
	stdout, stderr, err = a.sandboxExecOutput(ctx, projectID, sandboxID, exec.ID)
	if err != nil {
		return "", "", -1, err
	}
	exitCode = -1
	if code, ok := final.ExitCode.Get(); ok {
		exitCode = int(code)
	}
	return stdout, stderr, exitCode, nil
}

// waitSandboxExecExitPollInterval paces polling an exec's status. The commands
// that are captured rather than attached are short ones, so a short interval
// keeps their callers responsive without hammering the API.
const waitSandboxExecExitPollInterval = 150 * time.Millisecond

func (a *App) waitSandboxExecExit(ctx context.Context, projectID, sandboxID, execID string) (apimodel.SandboxExec, error) {
	for {
		exec, err := a.getSandboxExec(ctx, projectID, sandboxID, execID)
		if err != nil {
			return apimodel.SandboxExec{}, err
		}
		switch exec.Status {
		case apiclientgen.SandboxExecStatusExited, apiclientgen.SandboxExecStatusFailed, apiclientgen.SandboxExecStatusLost:
			return *exec, nil
		}
		select {
		case <-ctx.Done():
			return apimodel.SandboxExec{}, ctx.Err()
		case <-time.After(waitSandboxExecExitPollInterval):
		}
	}
}

// sandboxExecOutput replays an exited exec's recorded output, keeping the two
// streams apart: a caller that parses stdout must not have git's progress and
// warnings mixed into it.
func (a *App) sandboxExecOutput(ctx context.Context, projectID, sandboxID, execID string) (stdout, stderr string, err error) {
	client, err := a.apiClient()
	if err != nil {
		return "", "", err
	}
	res, err := client.ListSandboxExecLogs(ctx, apiclientgen.ListSandboxExecLogsParams{ProjectId: projectID, SandboxId: sandboxID, ExecId: execID})
	if err != nil {
		return "", "", err
	}
	body, err := expectResponse[apimodel.SandboxExecLogsResponse](res)
	if err != nil {
		return "", "", err
	}
	var out, errOut strings.Builder
	for _, entry := range body.GetEntries() {
		switch entry.Stream {
		case apiclientgen.SandboxExecLogEntryStreamStderr:
			errOut.Write(entry.Data)
		case apiclientgen.SandboxExecLogEntryStreamInput:
		default:
			out.Write(entry.Data)
		}
	}
	return out.String(), errOut.String(), nil
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
		// A started exec (notably a harness terminal) may return an empty or
		// truncated body; fall back to fetching its current state.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
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
	if r.User != nil {
		exec.SetUser(apiclientgen.NewOptSandboxUser(r.User.model()))
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
	if r.HarnessID != nil {
		exec.SetHarnessId(apiclientgen.NewOptString(*r.HarnessID))
	}
	if r.Primary != nil {
		exec.SetPrimary(apiclientgen.NewOptBool(*r.Primary))
	}
	return exec
}

func (u sandboxExecUser) model() apimodel.SandboxUser {
	out := apimodel.SandboxUser{}
	if u.Name != nil {
		out.SetName(apiclientgen.NewOptString(*u.Name))
	}
	if u.UID != nil {
		out.SetUID(apiclientgen.NewOptInt64(*u.UID))
	}
	if u.Gid != nil {
		out.SetGid(apiclientgen.NewOptInt64(*u.Gid))
	}
	if u.HomeDirectory != nil {
		out.SetHomeDirectory(apiclientgen.NewOptString(*u.HomeDirectory))
	}
	return out
}

func copySandboxExecInput(ctx context.Context, s *client.Session, interactive bool) error {
	if !interactive {
		return s.CloseInput()
	}
	buf := make([]byte, 32*1024)
	stdin := s.Stdin()
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if writeErr := s.WriteFrame(frame.Input, buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			return s.CloseInput()
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

func (a *App) returnSandboxExecStatus(ctx context.Context, projectID, sandboxID, execID string) error {
	exec, err := a.getSandboxExec(ctx, projectID, sandboxID, execID)
	if err != nil {
		return err
	}
	if code, ok := exec.ExitCode.Get(); ok && code != 0 {
		return client.ExitError{Code: int(code)}
	}
	if exec.Status == apiclientgen.SandboxExecStatusFailed {
		return fmt.Errorf("exec %s failed: %s", exec.ID, exec.Error.Or(""))
	}
	if exec.Status == apiclientgen.SandboxExecStatusLost {
		return fmt.Errorf("exec %s lost: %s", exec.ID, exec.Error.Or(""))
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

// directAttachFrames is an execstream.Conn over a single websocket, for attaches
// that do not reconnect.
type directAttachFrames struct {
	conn io.ReadWriteCloser
	mu   sync.Mutex
}

func (c *directAttachFrames) ReadFrame() (frame.Frame, error) { return frame.Read(c.conn) }

func (c *directAttachFrames) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return frame.Write(c.conn, typ, payload)
}

func (c *directAttachFrames) Close() error { return c.conn.Close() }

// websocketAttachFrames adds the websocket's native control ping/pong to the
// framed stream. The resumable library consumes this optional capability for
// timing events without putting diagnostic bytes into the terminal protocol.
type websocketAttachFrames struct {
	*directAttachFrames
	socket *websocket.Conn
}

func (c *websocketAttachFrames) Probe(ctx context.Context) error {
	return c.socket.Ping(ctx)
}
