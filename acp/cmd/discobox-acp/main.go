package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	acp "github.com/obot-platform/discobox/acp"
	"github.com/obot-platform/discobox/acp/launcher"
	"github.com/obot-platform/discobox/acp/protocol"
	"github.com/obot-platform/discobox/acp/registry"
	"github.com/spf13/cobra"
)

type cliOptions struct {
	registryURL string
	output      string
	timeout     time.Duration
	stdout      io.Writer
	stderr      io.Writer
}

type app struct {
	opts cliOptions
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], cliOptions{stdout: os.Stdout, stderr: os.Stderr}))
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
	if opts.stdout == nil {
		opts.stdout = os.Stdout
	}
	if opts.stderr == nil {
		opts.stderr = os.Stderr
	}
	if opts.output == "" {
		opts.output = "table"
	}
	if opts.timeout == 0 {
		opts.timeout = 30 * time.Second
	}
	a := &app{opts: opts}
	cmd := &cobra.Command{
		Use:           "discobox-acp",
		Short:         "Launch and inspect ACP agent implementations",
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
	cmd.SetOut(opts.stdout)
	cmd.SetErr(opts.stderr)
	cmd.PersistentFlags().StringVar(&a.opts.registryURL, "registry-url", registry.DefaultURL, "ACP registry URL")
	cmd.PersistentFlags().StringVarP(&a.opts.output, "output", "o", a.opts.output, "Output format: table or json")
	cmd.PersistentFlags().DurationVar(&a.opts.timeout, "timeout", a.opts.timeout, "ACP request timeout")
	cmd.AddCommand(a.newImplementationsCommand())
	cmd.AddCommand(a.newStatusCommand())
	cmd.AddCommand(a.newLaunchCommand())
	cmd.AddCommand(a.newSessionsCommand())
	cmd.AddCommand(a.newSupervisorCommand())
	return cmd
}

func (a *app) newImplementationsCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "implementations",
		Aliases: []string{"impls"},
		Short:   "List supported ACP implementations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			agents := registry.ListSupported()
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"implementations": agents})
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tALIASES\tDESCRIPTION")
			for _, agent := range agents {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", agent.ID, strings.Join(agent.Aliases, ","), agent.Description)
			}
			return tw.Flush()
		},
	}
}

func (a *app) newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status [AGENT]",
		Short: "Show ACP implementation launch and runtime status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, err := a.registryAgent(cmd.Context(), agentArg(args))
			if err != nil {
				return err
			}
			resolved, resolveErr := launcher.Resolve(agent)
			runtime, running, stale, err := launcher.RuntimeStatus(agent.ID)
			if err != nil {
				return err
			}
			if running && runtime != nil {
				if runtime.Socket == "" {
					running = false
					stale = true
				} else if err := daemonPing(cmd.Context(), agent.ID); err != nil {
					running = false
					stale = true
				}
			}
			status := map[string]any{
				"agent":      agent.ID,
				"name":       agent.Name,
				"version":    agent.Version,
				"running":    running,
				"stale_pid":  stale,
				"runtime":    runtime,
				"launchable": resolveErr == nil,
			}
			if resolveErr == nil {
				status["method"] = resolved.Method
				status["command"] = resolved.Command
				status["args"] = resolved.Args
			} else {
				status["launch_error"] = resolveErr.Error()
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), status)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "agent: %s (%s)\n", agent.ID, agent.Name)
			fmt.Fprintf(cmd.OutOrStdout(), "version: %s\n", agent.Version)
			fmt.Fprintf(cmd.OutOrStdout(), "running: %t\n", running)
			if runtime != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "pid: %d\n", runtime.PID)
				if runtime.AgentPID != 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "agent pid: %d\n", runtime.AgentPID)
				}
				if runtime.Socket != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "socket: %s\n", runtime.Socket)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "started: %s\n", runtime.StartedAt.Format(time.RFC3339))
				if stale {
					fmt.Fprintln(cmd.OutOrStdout(), "runtime record: stale")
				}
			}
			if resolveErr == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "launch method: %s\n", resolved.Method)
				fmt.Fprintf(cmd.OutOrStdout(), "launch command: %s %s\n", resolved.Command, strings.Join(resolved.Args, " "))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "launchable: false (%s)\n", resolveErr)
			}
			return nil
		},
	}
}

func (a *app) newLaunchCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "launch [AGENT]",
		Short: "Launch an ACP implementation in the background",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, err := a.registryAgent(cmd.Context(), agentArg(args))
			if err != nil {
				return err
			}
			started, err := a.ensureSupervisor(cmd.Context(), agent)
			if err != nil {
				return err
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"agent": agent.ID, "started": started})
			}
			if started {
				fmt.Fprintf(cmd.OutOrStdout(), "launched %s\n", agent.ID)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s already running\n", agent.ID)
			}
			return nil
		},
	}
}

func (a *app) newSessionsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "sessions", Short: "Inspect ACP sessions"}
	var cwd string
	list := &cobra.Command{
		Use:   "list [AGENT]",
		Short: "List sessions from an ACP implementation",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, err := a.registryAgent(cmd.Context(), agentArg(args))
			if err != nil {
				return err
			}
			if _, err := a.ensureSupervisor(cmd.Context(), agent); err != nil {
				return err
			}
			resp, err := daemonListSessions(cmd.Context(), agent.ID, cwd)
			if err != nil {
				return err
			}
			if a.opts.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"agent": agent.ID, "sessions": resp.Sessions, "next_cursor": resp.NextCursor})
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SESSION ID\tCWD\tTITLE")
			for _, session := range resp.Sessions {
				title := ""
				if session.Title != nil {
					title = *session.Title
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", session.SessionID, session.CWD, title)
			}
			return tw.Flush()
		},
	}
	list.Flags().StringVar(&cwd, "cwd", "", "filter sessions by absolute working directory")
	cmd.AddCommand(list)
	return cmd
}

func (a *app) newSupervisorCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "supervise AGENT",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, err := a.registryAgent(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.runSupervisor(cmd.Context(), agent)
		},
	}
}

func (a *app) registryAgent(ctx context.Context, id string) (registry.Agent, error) {
	resolved, err := registry.ResolveSupportedID(id)
	if err != nil {
		return registry.Agent{}, err
	}
	reg, err := registry.Fetch(ctx, a.opts.registryURL)
	if err != nil {
		return registry.Agent{}, err
	}
	return reg.FindAgent(resolved)
}

func agentArg(args []string) string {
	if len(args) == 0 {
		return acp.DefaultAgentID
	}
	return args[0]
}

func (a *app) ensureSupervisor(ctx context.Context, agent registry.Agent) (bool, error) {
	if runtime, running, _, err := launcher.RuntimeStatus(agent.ID); err != nil {
		return false, err
	} else if running && runtime != nil && runtime.Socket != "" {
		if err := daemonPing(ctx, agent.ID); err == nil {
			return false, nil
		}
		_ = launcher.ClearRuntime(agent.ID)
	}

	if _, err := launcher.Resolve(agent); err != nil {
		return false, err
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	args := []string{
		"--registry-url", a.opts.registryURL,
		"--timeout", a.opts.timeout.String(),
		"supervise", agent.ID,
	}
	process := exec.CommandContext(context.Background(), exe, args...)
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer null.Close()
	process.Stdin = null
	process.Stdout = null
	process.Stderr = null
	if err := process.Start(); err != nil {
		return false, err
	}
	_ = process.Process.Release()

	deadline := time.Now().Add(a.opts.timeout)
	for {
		if err := daemonPing(ctx, agent.ID); err == nil {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("timed out waiting for %s ACP supervisor readiness", agent.ID)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (a *app) runSupervisor(ctx context.Context, agent registry.Agent) error {
	resolved, err := launcher.Resolve(agent)
	if err != nil {
		return err
	}
	socket := launcher.SocketPath(agent.ID)
	_ = os.Remove(socket)
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(socket)

	process := launcher.ExecCommand(ctx, resolved)
	client, err := protocol.New(ctx, &mcp.CommandTransport{Command: process})
	if err != nil {
		return err
	}
	defer client.Close()
	defer launcher.ClearRuntime(agent.ID)

	init, err := client.Initialize(ctx)
	if err != nil {
		return err
	}
	if err := launcher.RecordRuntime(agent.ID, os.Getpid(), process, socket); err != nil {
		return err
	}

	state := &supervisorState{agentID: agent.ID, client: client, supportsSessionList: init.SupportsSessionList()}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go state.serveConn(conn)
	}
}

type supervisorState struct {
	agentID             string
	client              *protocol.Client
	supportsSessionList bool
}

type daemonRequest struct {
	Method string `json:"method"`
	CWD    string `json:"cwd,omitempty"`
}

type daemonResponse struct {
	OK         bool                   `json:"ok"`
	Error      string                 `json:"error,omitempty"`
	Sessions   []protocol.SessionInfo `json:"sessions,omitempty"`
	NextCursor *string                `json:"next_cursor,omitempty"`
}

func (s *supervisorState) serveConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)
	for scanner.Scan() {
		var req daemonRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = enc.Encode(daemonResponse{OK: false, Error: err.Error()})
			continue
		}
		switch req.Method {
		case "ping":
			_ = enc.Encode(daemonResponse{OK: true})
		case "sessions/list":
			if !s.supportsSessionList {
				_ = enc.Encode(daemonResponse{OK: false, Error: fmt.Sprintf("agent %q does not advertise session/list support", s.agentID)})
				continue
			}
			resp, err := s.client.ListSessions(context.Background(), req.CWD, "")
			if err != nil {
				_ = enc.Encode(daemonResponse{OK: false, Error: err.Error()})
				continue
			}
			_ = enc.Encode(daemonResponse{OK: true, Sessions: resp.Sessions, NextCursor: resp.NextCursor})
		default:
			_ = enc.Encode(daemonResponse{OK: false, Error: "unknown method"})
		}
	}
}

func daemonPing(ctx context.Context, agentID string) error {
	var resp daemonResponse
	return daemonCall(ctx, agentID, daemonRequest{Method: "ping"}, &resp)
}

func daemonListSessions(ctx context.Context, agentID, cwd string) (*protocol.ListSessionsResponse, error) {
	var resp daemonResponse
	if err := daemonCall(ctx, agentID, daemonRequest{Method: "sessions/list", CWD: cwd}, &resp); err != nil {
		return nil, err
	}
	return &protocol.ListSessionsResponse{Sessions: resp.Sessions, NextCursor: resp.NextCursor}, nil
}

func daemonCall(ctx context.Context, agentID string, req daemonRequest, resp *daemonResponse) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", launcher.SocketPath(agentID))
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	if err := json.NewDecoder(conn).Decode(resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
