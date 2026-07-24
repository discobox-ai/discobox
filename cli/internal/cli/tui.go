package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/execstream"
	"github.com/obot-platform/discobox/execstream/client"

	"github.com/obot-platform/discobox/execstream/frame"
	"github.com/obot-platform/discobox/execstream/resume"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
	"github.com/obot-platform/discobox/cli/internal/tui"
)

// newTUICommand launches the interactive terminal UI: a k9s-style, keyboard-driven
// dashboard whose first screen is a live table of the project's sandboxes.
func (a *App) newTUICommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive sandbox dashboard",
		Long: `Launch the interactive terminal UI.

The UI opens on a live table of the project's sandboxes with vim-style
navigation (j/k, g/G). Press up past the top to focus the tab bar, then use
h/l (or ←/→) to move between the sandboxes, agents, and secrets tabs and
enter/down to drop back into a tab. Mark rows with space, delete the marked
rows (or the highlighted one) with d, refresh with r, press enter to attach an
embedded terminal, or press f to attach fullscreen. Press q to quit.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			ds := &apiDataSource{app: a, client: client, projectID: projectID}
			return tui.Run(cmd.Context(), ds)
		},
	}
}

// apiDataSource adapts the CLI's API client to the tui.DataSource contract,
// reusing the existing sandbox and exec helpers so the UI shares one code path
// with the non-interactive commands.
type apiDataSource struct {
	app       *App
	client    *apiclientgen.Client
	projectID string
}

func (d *apiDataSource) ListSandboxes(ctx context.Context) ([]tui.Sandbox, error) {
	res, err := d.client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: d.projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](res)
	if err != nil {
		return nil, err
	}
	sandboxes := body.GetSandboxes()
	out := make([]tui.Sandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, toTUISandbox(sb))
	}
	return out, nil
}

func toTUISandbox(sb apimodel.Sandbox) tui.Sandbox {
	return tui.Sandbox{
		ID:      sb.ID,
		Name:    sb.Config.Name,
		State:   sandboxDisplayState(sb),
		Message: sandboxMessage(sb),
		Updated: sb.UpdatedAt,
		Created: sb.CreatedAt,
	}
}

func (d *apiDataSource) ListHarnesses(ctx context.Context) ([]tui.Harness, error) {
	configs, err := d.app.listHarnessConfigs(ctx, d.client, d.projectID)
	if err != nil {
		return nil, err
	}
	defaultID, _ := d.app.defaultHarnessConfigID(ctx, d.client, d.projectID)
	out := make([]tui.Harness, 0, len(configs))
	for _, cfg := range configs {
		name := cfg.Slug
		if name == "" {
			name = cfg.Name
		}
		label := cfg.Name
		if cfg.Slug != "" && !strings.EqualFold(cfg.Slug, cfg.Name) {
			label = fmt.Sprintf("%s (%s)", cfg.Name, cfg.Slug)
		}
		out = append(out, tui.Harness{Name: name, Label: label, Default: cfg.ID == defaultID})
	}
	return out, nil
}

func (d *apiDataSource) ListHarnessConfigs(ctx context.Context) ([]tui.HarnessConfig, error) {
	configs, err := d.app.listHarnessConfigs(ctx, d.client, d.projectID)
	if err != nil {
		return nil, err
	}
	defaultID, err := d.app.defaultHarnessConfigID(ctx, d.client, d.projectID)
	if err != nil {
		return nil, err
	}
	out := make([]tui.HarnessConfig, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, tui.HarnessConfig{
			ID:         cfg.ID,
			Name:       cfg.Name,
			Slug:       cfg.Slug,
			Image:      cfg.Image.Or(""),
			BuiltIn:    cfg.BuiltIn,
			Configured: cfg.Configured,
			Default:    cfg.ID == defaultID,
			Created:    cfg.CreatedAt,
			Updated:    cfg.UpdatedAt,
		})
	}
	return out, nil
}

func (d *apiDataSource) SetDefaultHarness(ctx context.Context, id string) error {
	return d.app.setDefaultHarnessConfig(ctx, d.client, d.projectID, id)
}

// ConfigureHarness runs the agent's interactive configure flow against the given
// streams, which the TUI supplies via tea.Exec as the restored real terminal so
// the user can answer prompts. The server owns applying the result.
func (d *apiDataSource) ConfigureHarness(ctx context.Context, harnessID string, stdin io.Reader, stdout, stderr io.Writer) error {
	_, err := d.app.runHarnessConfigure(ctx, d.client, d.projectID, harnessID, stdin, stdout, stderr)
	return err
}

// DeconfigureHarness undoes what the agent's configure flow created, leaving the
// agent in place but unrunnable until it is configured again.
func (d *apiDataSource) DeconfigureHarness(ctx context.Context, harnessID string) error {
	res, err := d.client.DeconfigureHarnessConfig(ctx, apiclientgen.DeconfigureHarnessConfigParams{
		ProjectId: d.projectID, HarnessConfigId: harnessID,
	})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.HarnessConfig](res)
	return err
}
func (d *apiDataSource) PathOptions(ctx context.Context) ([]string, error) {
	res, err := d.client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: d.projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](res)
	if err != nil {
		return nil, err
	}
	sandboxes := newestByCreatedAt(body.GetSandboxes(), func(s apimodel.Sandbox) time.Time { return s.CreatedAt }, -1)
	seen := map[string]struct{}{}
	var paths []string
	for _, sb := range sandboxes {
		source, ok := sb.Config.Source.Get()
		if !ok {
			continue
		}
		path := strings.TrimSpace(source.LocalDirectory.Or(""))
		if path == "" {
			if u, ok := source.URL.Get(); ok {
				path = strings.TrimSpace(u.String())
			}
		}
		if path == "" {
			continue
		}
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths, nil
}

func (d *apiDataSource) DefaultPath() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// CreateSession adapts TUI form values to the shared prompt sandbox creation
// flow used by the Cobra command.
func (d *apiDataSource) CreateSession(ctx context.Context, req tui.NewSessionRequest) (tui.Sandbox, error) {
	opts := sandboxcreate.PromptOptions{Source: strings.TrimSpace(req.Path), Harness: strings.TrimSpace(req.Harness)}
	if opts.Source == "" {
		opts.Source = d.DefaultPath()
	}
	if prompt := strings.TrimSpace(req.Prompt); prompt != "" {
		opts.Prompt = []string{prompt}
	}
	sandbox, err := sandboxcreate.CreatePromptSandbox(ctx, d.client, d.projectID, opts)
	if err != nil {
		return tui.Sandbox{}, err
	}
	// A server that cannot reach this directory waits for us to push it.
	if err := sandboxcreate.DeliverSource(ctx, d.client, d.projectID, sandbox, opts.Source, d.app.serverURL, d.app.token); err != nil {
		return tui.Sandbox{}, err
	}
	return toTUISandbox(*sandbox), nil
}

func (d *apiDataSource) DeleteSandbox(ctx context.Context, id string) error {
	sandboxID, err := d.app.resolveSandboxID(ctx, d.client, d.projectID, id)
	if err != nil {
		return err
	}
	res, err := d.client.DeleteSandbox(ctx, apiclientgen.DeleteSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	return expectNoContent[apiclientgen.DeleteSandboxAccepted](res)
}

// primaryExecID is the virtual exec id that resolves, on the sandbox-agent, to
// the sandbox's current primary (default) terminal and relaunches it with the
// harness's relaunch command when it has stopped. It must match
// terminal.PrimaryExecID in the sandbox-agent; the control plane proxies exec
// ids opaquely, so the client just uses this value in place of a real id.
const primaryExecID = "primary"

// OpenTerminal attaches to the sandbox's default (primary) terminal. It targets
// the virtual "primary" exec id, so the sandbox-agent resolves the current
// primary and relaunches (resumes) it when it has stopped — the client never
// creates a new exec, and a session that ended is revived on attach.
func (d *apiDataSource) OpenTerminal(ctx context.Context, sandboxID string, cols, rows int) (tui.Terminal, error) {
	events := make(chan tui.TerminalEvent, 4)
	frames, err := d.app.openReconnectingSandboxExecAttach(ctx, d.projectID, sandboxID, primaryExecID, execAttachOptions{replay: true, event: func(event resume.Event) {
		var state tui.TerminalConnectionState
		switch event.State {
		case resume.ConnectionReconnecting:
			state = tui.TerminalReconnecting
		case resume.ConnectionReconnected:
			state = tui.TerminalReconnected
		default:
			return
		}
		select {
		case events <- tui.TerminalEvent{State: state}:
		default:
		}
	}})
	if err != nil {
		return nil, err
	}
	term := &framedTerminal{frames: frames, events: events}
	if err := term.Resize(cols, rows); err != nil {
		_ = frames.Close()
		return nil, err
	}
	return term, nil
}

// AttachTerminal runs the same default-terminal attach flow as
// `disco box terminal attach`, using the real terminal streams supplied while
// Bubble Tea is suspended. It attaches the virtual primary terminal, so the
// agent resumes a stopped session on attach.
func (d *apiDataSource) AttachTerminal(ctx context.Context, sandboxID string, stdin io.Reader, stdout, stderr io.Writer) error {
	fmt.Fprintln(stderr, "Attaching to the sandbox terminal (Ctrl-P Ctrl-Q to detach)")
	return d.app.attachSandboxTerminal(ctx, d.projectID, sandboxID, primaryExecID, execAttachOptions{}, stdin, stdout, stderr)
}

// framedTerminal presents the framed attach connection as an io.ReadWriteCloser
// plus Resize: Read yields terminal output payloads, Write sends input frames,
// and Resize sends a resize frame.
type framedTerminal struct {
	frames  execstream.Conn
	events  <-chan tui.TerminalEvent
	readBuf []byte // leftover output bytes from a partially consumed frame

	writeMu sync.Mutex
	ready   sync.Once
}

func (t *framedTerminal) Read(p []byte) (int, error) {
	var readyErr error
	t.ready.Do(func() { readyErr = t.frames.WriteFrame(frame.Ready, nil) })
	if readyErr != nil {
		return 0, readyErr
	}
	if len(t.readBuf) > 0 {
		n := copy(p, t.readBuf)
		t.readBuf = t.readBuf[n:]
		return n, nil
	}
	for {
		f, err := t.frames.ReadFrame()
		if err != nil {
			return 0, err
		}
		switch f.Type {
		// A pane is one visual stream, so stderr is shown inline with stdout the
		// way a terminal shows it. (Terminals are TTY execs and never send it;
		// this keeps a pane over a pipe exec from dropping the stream.)
		case frame.Stdout, frame.Stderr:
			if len(f.Payload) == 0 {
				continue
			}
			n := copy(p, f.Payload)
			if n < len(f.Payload) {
				t.readBuf = f.Payload[n:]
			}
			return n, nil
		case frame.Exit:
			if err := client.ExitErrorFromPayload("terminal", f.Payload); err != nil {
				return 0, err
			}
			return 0, io.EOF
		case frame.Error:
			return 0, fmt.Errorf("terminal: %s", string(f.Payload))
		default:
			// Ignore frames the embedded pane does not consume (e.g. ready).
		}
	}
}

func (t *framedTerminal) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.frames.WriteFrame(frame.Input, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *framedTerminal) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	payload, err := json.Marshal(struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.frames.WriteFrame(frame.Resize, payload)
}

func (t *framedTerminal) Close() error {
	return t.frames.Close()
}

func (t *framedTerminal) Events() <-chan tui.TerminalEvent { return t.events }
