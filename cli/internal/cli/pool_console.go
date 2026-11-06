package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/pkg/execstream/client"
)

// newPoolConsoleCommand implements `discobox admin pool console`: a root shell on
// the machine hosting a pool's runtime, for debugging the backend itself.
//
// The console is not a sandbox and not the pool agent. It is a privileged
// container in the pool host's own namespaces, opened straight against that
// host's Docker daemon, so it answers even when the pool agent never came up.
func (a *App) newPoolConsoleCommand() *cobra.Command {
	var forceTTY bool
	command := &cobra.Command{
		Use:   "console [POOL] [--] [CMD...]",
		Short: "Open an admin console on a pool's host",
		Long: `Open an administrative console on the machine hosting a pool's runtime.

The console is a privileged container in the pool host's PID, IPC, network,
UTS, and cgroup namespaces, running as root with the host filesystem at /host
and the host's Docker socket in place. It runs the pool image, so docker, git,
and curl are already there.

It is meant for debugging a backend — a WSL or macOS VM that will not boot
Docker, an agent that never registers — and reaches the host through the
provider driver rather than through the pool agent, so it still answers when
the agent does not.

A vm pool has no such container: on a remote driver the console is a login shell
in the pool's machine, and on a local one there is none to open, because the
pool agent runs on this machine itself — read its log with "discobox admin pool
logs" instead.

One console container is kept per pool host and reattached, so a capture or a
trace started in it survives a detach. The shell exits when you type exit, and
the next console starts a fresh one.

The command follows POOL; an optional -- separates them explicitly.
Use a leading -- to omit POOL and use the default pool.
CLI flags go before POOL; everything after it is passed to the command.
Stdin is attached, stdout and stderr stay separate without a TTY, and the
command's exit status is returned.
A TTY is allocated when stdin is a terminal, or with -t; TTY output is merged.
One-off commands have the same host access and do not change the persistent shell.

Without POOL the project's default pool is used.`,
		Example: `  discobox admin pool console
  discobox admin pool console pool_01hq
  discobox admin pool console -- docker ps -a
  discobox admin pool console pool_01hq docker ps -a`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.completePools,
		RunE: func(cmd *cobra.Command, args []string) error {
			line, err := splitTargetCommand(cmd, args)
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			apiClient, err := a.apiClient()
			if err != nil {
				return err
			}
			var poolID string
			if line.target != "" {
				poolID, err = a.resolvePoolID(cmd.Context(), apiClient, projectID, line.target)
			} else {
				poolID, err = a.defaultPoolID(cmd.Context(), apiClient, projectID)
				if err == nil && strings.TrimSpace(poolID) == "" {
					err = errors.New("no default pool for this project; pass a pool ID")
				}
			}
			if err != nil {
				return err
			}
			if len(line.command) > 0 {
				tty := forceTTY || isTerminalStream(cmd.InOrStdin())
				return a.attachPoolConsole(cmd.Context(), projectID, poolID, line.command, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Opening an admin console on pool %s's host (%s to detach)\n", poolID, a.detachHint())
			fmt.Fprintln(cmd.ErrOrStderr(), "If the screen stays blank, press Enter to redraw the prompt.")
			return a.attachPoolConsole(cmd.Context(), projectID, poolID, nil, true, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	command.Flags().SetInterspersed(false)
	command.Flags().BoolVarP(&forceTTY, "tty", "t", false, "Allocate a TTY for a one-off command")
	return command
}

func (a *App) attachPoolConsole(ctx context.Context, projectID, poolID string, argv []string, tty bool, stdin io.Reader, stdout, stderr io.Writer) error {
	console := client.NewOSConsole(stdin, stdout)
	var cols, rows int
	if console != nil {
		cols, rows, _ = console.Size()
	}
	frames, err := a.openPoolConsoleAttach(ctx, projectID, poolID, argv, tty, rows, cols)
	if err != nil {
		return err
	}
	defer frames.Close()

	// One filter for the whole session: the detach chord is a pair of
	// keystrokes and can land in separate reads.
	chord := newDetachFilter(a.leader())
	session := client.New(client.Options{
		Conn:        frames,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		Console:     console,
		Kind:        "pool console",
		Action:      "open pool console",
		RequireExit: len(argv) > 0,
		ErrorFrame:  func(payload []byte) error { return fmt.Errorf("pool console: %s", terminalSafe(string(payload))) },
		RawMode:     tty,
		Terminal:    tty,
		CopyInput: func(ctx context.Context, s *client.Session) error {
			if len(argv) == 0 {
				return copyTerminalInput(ctx, s, chord)
			}
			return copySandboxExecInput(ctx, s, true)
		},
		InterruptNotice: interruptNotice(stderr, tty, "the pool host"),
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
	err = session.Run(ctx)
	if errors.Is(err, errTerminalDetached) {
		return nil
	}
	if errors.Is(err, client.ErrInterrupted) {
		return interruptedExit()
	}
	return err
}

// openPoolConsoleAttach dials the console websocket. The size travels as query
// parameters so the shell's first prompt is already drawn at the caller's size,
// before the session's own resize tracking has said anything.
func (a *App) openPoolConsoleAttach(ctx context.Context, projectID, poolID string, argv []string, tty bool, rows, cols int) (*directAttachFrames, error) {
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
		return nil, fmt.Errorf("open pool console: unsupported websocket base URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/pools/" + url.PathEscape(poolID) + "/console"
	query := url.Values{}
	for _, arg := range argv {
		query.Add("command", arg)
	}
	if len(argv) > 0 {
		query.Set("tty", strconv.FormatBool(tty))
	}
	if tty && rows > 0 && cols > 0 {
		query.Set("rows", strconv.Itoa(rows))
		query.Set("cols", strconv.Itoa(cols))
	}
	u.RawQuery = query.Encode()
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if message := attachErrorMessage(body); message != "" {
				return nil, fmt.Errorf("open pool console: %s", message)
			}
		}
		return nil, fmt.Errorf("open pool console: %w", err)
	}
	return &directAttachFrames{conn: websocket.NetConn(ctx, conn, websocket.MessageBinary)}, nil
}
