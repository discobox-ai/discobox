package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	sessions "github.com/obot-platform/discobox/sessions"
	"github.com/obot-platform/discobox/sessions/client"
	"github.com/obot-platform/discobox/sessions/daemon"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var version string

type sessionPaths struct {
	SessionID     string
	RepoRoot      string
	StateDir      string
	RuntimeDir    string
	SupervisorDir string
	Socket        string
	Lock          string
	DB            string
	Runtime       string
}

type cliOptions struct {
	sessionID string
	repoRoot  string
	noStart   bool
	output    string
	stdout    io.Writer
	stderr    io.Writer
	stdin     io.Reader
}

type app struct {
	opts cliOptions
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], cliOptions{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}))
}

func run(ctx context.Context, args []string, opts cliOptions) int {
	cmd := newRootCommand(opts)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
		return 1
	}
	return 0
}

func newRootCommand(opts cliOptions) *cobra.Command {
	if opts.stdin == nil {
		opts.stdin = os.Stdin
	}
	if opts.stdout == nil {
		opts.stdout = os.Stdout
	}
	if opts.stderr == nil {
		opts.stderr = os.Stderr
	}
	if opts.output == "" {
		opts.output = "table"
	}
	a := &app{opts: opts}
	cmd := &cobra.Command{
		Use:           "discobox-session",
		Short:         "Manage local coding-agent sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			switch a.opts.output {
			case "table", "json":
				return nil
			default:
				return fmt.Errorf("unsupported output format %q; expected table or json", a.opts.output)
			}
		},
	}
	cmd.SetIn(opts.stdin)
	cmd.SetOut(opts.stdout)
	cmd.SetErr(opts.stderr)
	cmd.PersistentFlags().StringVar(&a.opts.sessionID, "session-id", a.opts.sessionID, "daemon session id (default DISCOBOX_SESSION_ID or repo hash)")
	cmd.PersistentFlags().StringVar(&a.opts.repoRoot, "repo-root", a.opts.repoRoot, "repository root (default: git rev-parse --show-toplevel)")
	cmd.PersistentFlags().BoolVar(&a.opts.noStart, "no-start", a.opts.noStart, "do not start daemon when socket is unavailable")
	cmd.PersistentFlags().StringVarP(&a.opts.output, "output", "o", a.opts.output, "Output format: table or json")

	cmd.AddCommand(a.newDaemonCommand())
	cmd.AddCommand(a.newAgentsCommand())
	cmd.AddCommand(a.newListCommand())
	cmd.AddCommand(a.newCreateCommand())
	cmd.AddCommand(a.newAttachCommand())
	return cmd
}

func (a *app) newDaemonCommand() *cobra.Command {
	var idle time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the session daemon in the foreground",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := a.sessionPaths(cmd.Context())
			if err != nil {
				return err
			}
			return daemon.Run(cmd.Context(), daemon.Config{
				SessionID:     paths.SessionID,
				RepoRoot:      paths.RepoRoot,
				DBPath:        paths.DB,
				SocketPath:    paths.Socket,
				RuntimePath:   paths.Runtime,
				SupervisorDir: paths.SupervisorDir,
				Version:       currentBuildVersion(),
				IdleTimeout:   idle,
			})
		},
	}
	cmd.Flags().DurationVar(&idle, "idle-timeout", 0, "daemon idle timeout")
	cmd.Flags().Bool("foreground", true, "run daemon in foreground")
	cmd.AddCommand(a.newDaemonStatusCommand())
	cmd.AddCommand(a.newShutdownCommand())
	cmd.AddCommand(a.newSupervisorCommand())
	return cmd
}

func (a *app) newSupervisorCommand() *cobra.Command {
	var sessionID, agentID, workdir, socketPath, runtimePath, commandEncoded string
	var rows, cols int
	cmd := &cobra.Command{
		Use:    "supervisor",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			commandJSON, err := base64.StdEncoding.DecodeString(commandEncoded)
			if err != nil {
				return err
			}
			var command []string
			if err := json.Unmarshal(commandJSON, &command); err != nil {
				return err
			}
			token := os.Getenv("DISCOBOX_SUPERVISOR_TOKEN")
			if token == "" {
				return fmt.Errorf("DISCOBOX_SUPERVISOR_TOKEN is required")
			}
			rows16, err := terminalDimensionFromInt(rows)
			if err != nil {
				return fmt.Errorf("invalid rows: %w", err)
			}
			cols16, err := terminalDimensionFromInt(cols)
			if err != nil {
				return fmt.Errorf("invalid cols: %w", err)
			}
			return daemon.RunSupervisor(cmd.Context(), daemon.SupervisorConfig{
				Session: sessions.Session{
					ID:        sessionID,
					AgentID:   agentID,
					Command:   command,
					Workdir:   workdir,
					Running:   true,
					CreatedAt: time.Now().UTC(),
				},
				SocketPath:  socketPath,
				RuntimePath: runtimePath,
				Rows:        rows16,
				Cols:        cols16,
				Token:       token,
			})
		},
	}
	cmd.Flags().StringVar(&sessionID, "coding-session-id", "", "coding session id")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "agent id")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory")
	cmd.Flags().StringVar(&socketPath, "socket", "", "supervisor socket path")
	cmd.Flags().StringVar(&runtimePath, "runtime", "", "supervisor runtime path")
	cmd.Flags().StringVar(&commandEncoded, "command", "", "base64 encoded command JSON")
	cmd.Flags().IntVar(&rows, "rows", 24, "initial PTY rows")
	cmd.Flags().IntVar(&cols, "cols", 80, "initial PTY cols")
	_ = cmd.MarkFlagRequired("coding-session-id")
	_ = cmd.MarkFlagRequired("agent-id")
	_ = cmd.MarkFlagRequired("workdir")
	_ = cmd.MarkFlagRequired("socket")
	_ = cmd.MarkFlagRequired("runtime")
	_ = cmd.MarkFlagRequired("command")
	return cmd
}

func (a *app) newDaemonStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show session daemon status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, paths, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			status, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			if status.SessionID == "" {
				status.SessionID = paths.SessionID
			}
			if status.RepoRoot == "" {
				status.RepoRoot = paths.RepoRoot
			}
			return a.writeStatus(cmd, status)
		},
	}
}

func (a *app) newShutdownCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Ask the session daemon to exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.Shutdown(cmd.Context()); err != nil {
				return err
			}
			return a.writeAction(cmd, map[string]bool{"shutdown": true}, "shutdown requested")
		},
	}
}

func (a *app) newAgentsCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "agents",
		Aliases: []string{"agent", "supported-agents", "supported"},
		Short:   "List supported coding agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			agents, err := c.Agents(cmd.Context())
			if err != nil {
				return err
			}
			return a.writeAgents(cmd, agents)
		},
	}
}

func (a *app) newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List coding-agent sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			sessionsList, err := c.List(cmd.Context())
			if err != nil {
				return err
			}
			return a.writeSessions(cmd, sessionsList)
		},
	}
}

func (a *app) newCreateCommand() *cobra.Command {
	var detach bool
	var workdir string
	cmd := &cobra.Command{
		Use:   "create AGENT [-- ARG ...]",
		Short: "Create a coding-agent session",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, paths, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			cols, rows := currentTerminalSize()
			if workdir == "" {
				workdir = paths.RepoRoot
			}
			session, err := c.Create(cmd.Context(), sessions.CreateRequest{
				AgentID: args[0],
				Args:    args[1:],
				Workdir: workdir,
				Cols:    cols,
				Rows:    rows,
			})
			if err != nil {
				return err
			}
			if detach {
				return a.writeSessionCreated(cmd, session)
			}
			if a.opts.output == "json" {
				if err := a.writeSessionCreated(cmd, session); err != nil {
					return err
				}
			}
			return a.attachSession(cmd.Context(), c, session.ID)
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "create the session without attaching")
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory for the agent (default: repo root)")
	return cmd
}

func (a *app) newAttachCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "attach [SESSION_ID]",
		Short: "Attach to a running coding-agent session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			var sessionID string
			if len(args) > 0 {
				sessionID = args[0]
			} else {
				sessionID, err = latestRunningSession(cmd.Context(), c)
				if err != nil {
					return err
				}
			}
			return a.attachSession(cmd.Context(), c, sessionID)
		},
	}
}

func (a *app) sessionPaths(ctx context.Context) (sessionPaths, error) {
	root := a.opts.repoRoot
	var err error
	if root == "" {
		root, err = gitRoot(ctx)
		if err != nil {
			return sessionPaths{}, fmt.Errorf("resolve git root: %w", err)
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return sessionPaths{}, err
	}
	return computeSessionPaths(root, resolveSessionID(a.opts.sessionID, root)), nil
}

func (a *app) client(ctx context.Context) (*client.Client, sessionPaths, error) {
	paths, err := a.sessionPaths(ctx)
	if err != nil {
		return nil, sessionPaths{}, err
	}
	c := client.New(paths.Socket)
	if !a.opts.noStart {
		if err := ensureDaemon(ctx, c, paths); err != nil {
			return nil, sessionPaths{}, err
		}
	}
	return c, paths, nil
}

func latestRunningSession(ctx context.Context, c *client.Client) (string, error) {
	list, err := c.List(ctx)
	if err != nil {
		return "", err
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	for _, session := range list {
		if session.Running {
			return session.ID, nil
		}
	}
	return "", fmt.Errorf("no running sessions")
}

func (a *app) attachSession(ctx context.Context, c *client.Client, sessionID string) error {
	stream, err := c.Attach(ctx, sessionID)
	if err != nil {
		return err
	}
	defer stream.Close()
	stdinFile, stdinOK := a.opts.stdin.(*os.File)
	stdoutFile, stdoutOK := a.opts.stdout.(*os.File)
	restoreTerminal := func() {}
	if stdinOK && term.IsTerminal(int(stdinFile.Fd())) {
		oldState, err := term.MakeRaw(int(stdinFile.Fd()))
		if err != nil {
			return err
		}
		var restoreOnce sync.Once
		restoreTerminal = func() {
			restoreOnce.Do(func() {
				_ = term.Restore(int(stdinFile.Fd()), oldState)
			})
		}
		defer func() {
			restoreTerminal()
		}()
	}
	if stdoutOK && term.IsTerminal(int(stdoutFile.Fd())) {
		cols, rows := currentTerminalSizeFrom(stdoutFile)
		if err := stream.Resize(cols, rows); err != nil {
			return err
		}
		if err := stream.WriteInput([]byte{0x0c}); err != nil {
			return err
		}
	}

	errCh := make(chan error, 3)
	done := make(chan struct{})
	var detachRequested atomic.Bool
	var closeOnce sync.Once
	closeDone := func() {
		closeOnce.Do(func() {
			close(done)
			_ = stream.Close()
		})
	}

	go func() {
		defer closeDone()
		for {
			frame, err := stream.ReadFrame()
			if err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
				} else {
					errCh <- err
				}
				return
			}
			switch frame.Type {
			case sessions.FrameOutput:
				if _, err := a.opts.stdout.Write(frame.Payload); err != nil {
					errCh <- err
					return
				}
			case sessions.FrameError:
				errCh <- fmt.Errorf("%s", strings.TrimSpace(string(frame.Payload)))
				return
			}
		}
	}()

	go func() {
		if err := copyInputWithDetach(done, a.opts.stdin, stream); err != nil {
			if errors.Is(err, errDetached) {
				detachRequested.Store(true)
			}
			errCh <- err
			closeDone()
		}
	}()

	if stdoutOK && term.IsTerminal(int(stdoutFile.Fd())) {
		go func() {
			errCh <- watchResize(done, stdoutFile, stream)
		}()
	}

	err = <-errCh
	closeDone()
	if errors.Is(err, errDetached) || detachRequested.Load() {
		restoreTerminal()
		_, _ = fmt.Fprintln(a.opts.stderr, "detached")
		return nil
	}
	return err
}

var errDetached = errors.New("detached")

func copyInputWithDetach(done <-chan struct{}, in io.Reader, stream *client.AttachStream) error {
	buf := make([]byte, 1)
	pendingCtrlP := false
	for {
		select {
		case <-done:
			return nil
		default:
		}
		n, err := in.Read(buf)
		if n > 0 {
			b := buf[0]
			if pendingCtrlP {
				pendingCtrlP = false
				if b == 'q' || b == 'Q' {
					return errDetached
				}
				if err := stream.WriteInput([]byte{0x10}); err != nil {
					return err
				}
			}
			if b == 0x10 {
				pendingCtrlP = true
				continue
			}
			if err := stream.WriteInput([]byte{b}); err != nil {
				return err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if pendingCtrlP {
					_ = stream.WriteInput([]byte{0x10})
				}
				return stream.CloseWrite()
			}
			return err
		}
	}
}

func watchResize(done <-chan struct{}, file *os.File, stream *client.AttachStream) error {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-done:
			return nil
		case <-ch:
			cols, rows := currentTerminalSizeFrom(file)
			if err := stream.Resize(cols, rows); err != nil {
				return err
			}
		}
	}
}

func currentTerminalSize() (uint16, uint16) {
	return currentTerminalSizeFrom(os.Stdout)
}

func currentTerminalSizeFrom(file *os.File) (uint16, uint16) {
	width, height, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return 80, 24
	}
	cols, err := terminalDimensionFromInt(width)
	if err != nil {
		cols = 80
	}
	rows, err := terminalDimensionFromInt(height)
	if err != nil {
		rows = 24
	}
	return cols, rows
}

func terminalDimensionFromInt(value int) (uint16, error) {
	if value <= 0 || value > 65535 {
		return 0, fmt.Errorf("dimension %d outside uint16 range", value)
	}
	return uint16(value), nil
}

func (a *app) writeStatus(cmd *cobra.Command, status *sessions.StatusResponse) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), status)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tREPO\tRUNNING\tTOTAL")
	running := 0
	for _, session := range status.Sessions {
		if session.Running {
			running++
		}
	}
	fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", status.SessionID, status.RepoRoot, running, len(status.Sessions))
	return tw.Flush()
}

func (a *app) writeAgents(cmd *cobra.Command, agents []sessions.Agent) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"agents": agents})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCOMMAND")
	for _, agent := range agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", agent.ID, agent.Name, strings.Join(agent.Command, " "))
	}
	return tw.Flush()
}

func (a *app) writeSessions(cmd *cobra.Command, list []sessions.Session) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"sessions": list})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tAGENT\tPID\tSTATUS\tWORKDIR\tCOMMAND")
	for _, session := range list {
		status := "exited"
		if session.Running {
			status = "running"
		} else if session.ExitCode != nil {
			status = fmt.Sprintf("exit:%d", *session.ExitCode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", session.ID, session.AgentID, session.PID, status, session.Workdir, strings.Join(session.Command, " "))
	}
	return tw.Flush()
}

func (a *app) writeSessionCreated(cmd *cobra.Command, session sessions.Session) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), session)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", session.ID)
	return nil
}

func (a *app) writeAction(cmd *cobra.Command, value any, message string) error {
	if a.opts.output == "json" {
		return writeJSON(cmd.OutOrStdout(), value)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), message)
	return err
}

func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func computeSessionPaths(repoRoot, sessionID string) sessionPaths {
	root := filepath.Clean(repoRoot)
	sessionID = safePathComponent(sessionID)
	repoKey := repoStateKey(root)
	stateDir := filepath.Join(xdgStateHome(), "discobox", "session", sessionID, "sessions", repoKey)
	runtimeDir := filepath.Join(xdgRuntimeHome(), "discobox", "s", sessionID, "sessions", repoKey[:16])
	supervisorDir := filepath.Join(xdgRuntimeHome(), "discobox", "p", sessionID, repoKey[:8])
	return sessionPaths{
		SessionID:     sessionID,
		RepoRoot:      root,
		StateDir:      stateDir,
		RuntimeDir:    runtimeDir,
		SupervisorDir: supervisorDir,
		Socket:        filepath.Join(runtimeDir, "daemon.sock"),
		Lock:          filepath.Join(runtimeDir, "startup.lock"),
		DB:            filepath.Join(stateDir, "sessions.db"),
		Runtime:       filepath.Join(runtimeDir, "runtime.json"),
	}
}

func ensureDaemon(ctx context.Context, c *client.Client, paths sessionPaths) error {
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	info, err := c.PingInfo(pingCtx)
	cancel()
	if err == nil && !clientNewerThanDaemon(currentBuildVersion(), info.Version) {
		return nil
	}
	if err != nil && !errors.Is(err, client.ErrNotRunning) {
		return err
	}
	if err := os.MkdirAll(paths.RuntimeDir, 0o755); err != nil {
		return err
	}
	unlock, err := acquireStartupLock(paths.Lock)
	if err != nil {
		return err
	}
	defer unlock()
	pingCtx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
	info, err = c.PingInfo(pingCtx)
	cancel()
	if err == nil && !clientNewerThanDaemon(currentBuildVersion(), info.Version) {
		return nil
	}
	if err == nil && clientNewerThanDaemon(currentBuildVersion(), info.Version) {
		if err := replaceOlderDaemon(ctx, c); err != nil {
			return err
		}
	} else if !errors.Is(err, client.ErrNotRunning) {
		return err
	}
	if err := startDetachedDaemon(ctx, paths); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pingCtx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
		info, err = c.PingInfo(pingCtx)
		cancel()
		if err == nil && !clientNewerThanDaemon(currentBuildVersion(), info.Version) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready at %s: %w", paths.Socket, err)
}

func clientNewerThanDaemon(clientVersion, daemonVersion int64) bool {
	return clientVersion > daemonVersion
}

func replaceOlderDaemon(ctx context.Context, c *client.Client) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := c.Shutdown(shutdownCtx)
	cancel()
	if err != nil && !errors.Is(err, client.ErrNotRunning) {
		return fmt.Errorf("shutdown older daemon: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		err = c.Ping(pingCtx)
		cancel()
		if errors.Is(err, client.ErrNotRunning) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("older daemon did not stop at %s", c.SocketPath())
}

func acquireStartupLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func startDetachedDaemon(ctx context.Context, paths sessionPaths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, "--session-id", paths.SessionID, "--repo-root", paths.RepoRoot, "daemon", "--foreground")
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "DISCOBOX_SESSION_ID="+paths.SessionID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

func currentBuildVersion() int64 {
	if parsed, ok := parseVersion(version); ok {
		return parsed
	}
	exe, err := os.Executable()
	if err == nil {
		if info, err := os.Stat(exe); err == nil {
			return info.ModTime().Unix()
		}
	}
	return time.Now().Unix()
}

func parseVersion(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var parsed int64
	if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func gitRoot(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func resolveSessionID(explicit, repoRoot string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("DISCOBOX_SESSION_ID"); env != "" {
		return env
	}
	return repoStateKey(repoRoot)[:16]
}

func repoStateKey(root string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return hex.EncodeToString(sum[:])[:32]
}

func safePathComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "default"
	}
	return out
}

func xdgStateHome() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state")
	}
	return filepath.Join(os.TempDir(), "discobox-state")
}

func xdgRuntimeHome() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(xdgStateHome(), "discobox", "run")
}
