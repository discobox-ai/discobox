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

	"github.com/discobox-ai/discobox/execstream/client"
)

// newPoolConsoleCommand implements `discobox admin pool console`: a root shell on
// the machine hosting a pool's runtime, for debugging the backend itself.
//
// The console is not a sandbox and not the pool agent. It is a privileged
// container in the pool host's own namespaces, opened straight against that
// host's Docker daemon, so it answers even when the pool agent never came up.
func (a *App) newPoolConsoleCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "console [POOL_ID]",
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

One console container is kept per pool host and reattached, so a capture or a
trace started in it survives a detach. The shell exits when you type exit, and
the next console starts a fresh one.

Without POOL_ID the project's default pool is used.`,
		Example: `  discobox admin pool console
  discobox admin pool console pool_01hq`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completePools,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			apiClient, err := a.apiClient()
			if err != nil {
				return err
			}
			var poolID string
			if len(args) > 0 {
				poolID, err = a.resolvePoolID(cmd.Context(), apiClient, projectID, args[0])
			} else {
				poolID, err = a.defaultPoolID(cmd.Context(), apiClient, projectID)
				if err == nil && strings.TrimSpace(poolID) == "" {
					err = errors.New("no default pool for this project; pass a pool ID")
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Opening an admin console on pool %s's host (%s to detach)\n", poolID, a.detachHint())
			return a.attachPoolConsole(cmd.Context(), projectID, poolID, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func (a *App) attachPoolConsole(ctx context.Context, projectID, poolID string, stdin io.Reader, stdout, stderr io.Writer) error {
	// A nil *OSConsole must not travel as a non-nil Console interface, so the
	// interface stays unset when stdin is not a terminal file.
	var console client.Console
	var cols, rows int
	if osConsole := client.NewOSConsole(stdin); osConsole != nil {
		console = osConsole
		cols, rows, _ = osConsole.Size()
	}
	frames, err := a.openPoolConsoleAttach(ctx, projectID, poolID, rows, cols)
	if err != nil {
		return err
	}
	defer frames.Close()

	// One filter for the whole session: the detach chord is a pair of
	// keystrokes and can land in separate reads.
	chord := newDetachFilter(a.leader())
	session := client.New(client.Options{
		Conn:    frames,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		Console: console,
		Kind:    "pool console",
		Action:  "open pool console",
		RawMode: true,
		Resize:  true,
		CopyInput: func(ctx context.Context, s *client.Session) error {
			return copyTerminalInput(ctx, s, chord)
		},
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
	return err
}

// openPoolConsoleAttach dials the console websocket. The size travels as query
// parameters so the shell's first prompt is already drawn at the caller's size,
// before the session's own resize tracking has said anything.
func (a *App) openPoolConsoleAttach(ctx context.Context, projectID, poolID string, rows, cols int) (*directAttachFrames, error) {
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
	if rows > 0 && cols > 0 {
		query := url.Values{}
		query.Set("rows", strconv.Itoa(rows))
		query.Set("cols", strconv.Itoa(cols))
		u.RawQuery = query.Encode()
	}
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
