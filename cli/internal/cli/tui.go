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

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
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
			ID:           cfg.ID,
			Name:         cfg.Name,
			Slug:         cfg.Slug,
			Image:        cfg.Image.Or(""),
			DefinitionID: cfg.DefinitionId.Or(""),
			Default:      cfg.ID == defaultID,
			Created:      cfg.CreatedAt,
			Updated:      cfg.UpdatedAt,
		})
	}
	return out, nil
}

func (d *apiDataSource) ListHarnessDefinitions(ctx context.Context) ([]tui.HarnessDefinition, error) {
	res, err := d.client.ListHarnessDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListHarnessDefinitionsBody](res)
	if err != nil {
		return nil, err
	}
	defs := body.GetHarnessDefinitions()
	out := make([]tui.HarnessDefinition, 0, len(defs))
	for _, def := range defs {
		out = append(out, tui.HarnessDefinition{
			ID:          def.ID,
			Name:        def.Name,
			Description: def.Description.Or(""),
			Image:       def.Image.Or(""),
		})
	}
	return out, nil
}

// SaveHarness creates a harness config (from a definition or a custom image) or
// updates an existing config's name. Choosing the project default is a separate
// action (SetDefaultHarness) driven from the agents list. Definition-backed
// creation skips the interactive configure step; a definition with a configure
// flow should be enabled via `discobox debug harness enable` to answer its prompts.
func (d *apiDataSource) SaveHarness(ctx context.Context, req tui.SaveHarnessRequest) (tui.HarnessConfig, error) {
	var cfg *apimodel.HarnessConfig
	if req.ID == "" {
		body, err := createHarnessBody(harnessCreateOptions{
			name:         req.Name,
			slug:         req.Slug,
			definitionID: req.DefinitionID,
			image:        req.Image,
		})
		if err != nil {
			return tui.HarnessConfig{}, err
		}
		res, err := d.client.CreateHarnessConfig(ctx, body, apiclientgen.CreateHarnessConfigParams{ProjectId: d.projectID})
		if err != nil {
			return tui.HarnessConfig{}, err
		}
		if cfg, err = expectResponse[apimodel.HarnessConfig](res); err != nil {
			return tui.HarnessConfig{}, err
		}
	} else {
		body := &apimodel.UpdateHarnessConfigBody{}
		body.SetName(apiclientgen.NewOptString(req.Name))
		res, err := d.client.UpdateHarnessConfig(ctx, body, apiclientgen.UpdateHarnessConfigParams{ProjectId: d.projectID, HarnessConfigId: req.ID})
		if err != nil {
			return tui.HarnessConfig{}, err
		}
		if cfg, err = expectResponse[apimodel.HarnessConfig](res); err != nil {
			return tui.HarnessConfig{}, err
		}
	}
	return d.toTUIHarnessConfig(ctx, *cfg)
}

func (d *apiDataSource) DeleteHarness(ctx context.Context, id string) error {
	res, err := d.client.DeleteHarnessConfig(ctx, apiclientgen.DeleteHarnessConfigParams{ProjectId: d.projectID, HarnessConfigId: id})
	if err != nil {
		return err
	}
	return expectNoContent[apiclientgen.DeleteHarnessConfigNoContent](res)
}

func (d *apiDataSource) SetDefaultHarness(ctx context.Context, id string) error {
	return d.app.setDefaultHarnessConfig(ctx, d.client, d.projectID, id)
}

// ConfigureHarness runs the agent's interactive configure flow in the current
// terminal. It spins up a config-mode sandbox based on the agent config itself
// (its own image and ID), attaches the sandbox's primary terminal to the given
// streams so the user answers prompts directly, then applies the resulting files
// and secret bindings back onto the config.
func (d *apiDataSource) ConfigureHarness(ctx context.Context, harnessID string, stdin io.Reader, stdout, stderr io.Writer) error {
	res, err := d.client.GetHarnessConfig(ctx, apiclientgen.GetHarnessConfigParams{ProjectId: d.projectID, HarnessConfigId: harnessID})
	if err != nil {
		return err
	}
	cfg, err := expectResponse[apimodel.HarnessConfig](res)
	if err != nil {
		return err
	}
	config := apimodel.SandboxCreateConfig{
		HarnessConfigId: apiclientgen.NewOptString(cfg.ID),
		HarnessMode:     apiclientgen.NewOptSandboxCreateConfigHarnessMode(apiclientgen.SandboxCreateConfigHarnessModeConfig),
		Image:           cfg.Image,
	}
	out, err := d.app.runConfigureSandbox(ctx, d.client, d.projectID, config, stdin, stdout, stderr)
	if err != nil {
		return err
	}
	return d.app.applyHarnessConfigureOutput(ctx, d.client, d.projectID, cfg.ID, out)
}

// toTUIHarnessConfig maps an API harness config to the UI view, resolving the
// project default so the saved row can render its marker.
func (d *apiDataSource) toTUIHarnessConfig(ctx context.Context, cfg apimodel.HarnessConfig) (tui.HarnessConfig, error) {
	defaultID, err := d.app.defaultHarnessConfigID(ctx, d.client, d.projectID)
	if err != nil {
		return tui.HarnessConfig{}, err
	}
	return tui.HarnessConfig{
		ID:           cfg.ID,
		Name:         cfg.Name,
		Slug:         cfg.Slug,
		Image:        cfg.Image.Or(""),
		DefinitionID: cfg.DefinitionId.Or(""),
		Default:      cfg.ID == defaultID,
		Created:      cfg.CreatedAt,
		Updated:      cfg.UpdatedAt,
	}, nil
}

// PathOptions returns the distinct local directories and remote URLs that the
// project's existing sandboxes were created from, most-recent first.
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

// CreateSession builds the same run request the `run` command sends: it resolves
// the source path (local Git repo or remote URL), attaches the harness and
// prompt, and creates the sandbox.
func (d *apiDataSource) CreateSession(ctx context.Context, req tui.NewSessionRequest) (tui.Sandbox, error) {
	opts := runOptions{source: strings.TrimSpace(req.Path), harness: strings.TrimSpace(req.Harness)}
	if opts.source == "" {
		opts.source = d.DefaultPath()
	}
	if source, ref, ok := splitRunSourceRef(opts.source); ok {
		opts.source, opts.ref = source, ref
	}
	if prompt := strings.TrimSpace(req.Prompt); prompt != "" {
		opts.prompt = []string{prompt}
	}
	body, err := createRunSandboxBody(ctx, opts)
	if err != nil {
		return tui.Sandbox{}, err
	}
	res, err := d.client.CreateSandbox(ctx, body, apiclientgen.CreateSandboxParams{ProjectId: d.projectID})
	if err != nil {
		return tui.Sandbox{}, err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
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
	frames, err := d.app.openReconnectingSandboxExecAttach(ctx, d.projectID, sandboxID, primaryExecID, true, func(event attachConnectionEvent) {
		var state tui.TerminalConnectionState
		switch event.State {
		case attachConnectionReconnecting:
			state = tui.TerminalReconnecting
		case attachConnectionReconnected:
			state = tui.TerminalReconnected
		default:
			return
		}
		select {
		case events <- tui.TerminalEvent{State: state}:
		default:
		}
	})
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
// `discobox debug terminal attach`, using the real terminal streams supplied while
// Bubble Tea is suspended. It attaches the virtual primary terminal, so the
// agent resumes a stopped session on attach.
func (d *apiDataSource) AttachTerminal(ctx context.Context, sandboxID string, stdin io.Reader, stdout, stderr io.Writer) error {
	fmt.Fprintln(stderr, "Attaching to the sandbox terminal (Ctrl-P Ctrl-Q to detach)")
	return d.app.attachSandboxTerminal(ctx, d.projectID, sandboxID, primaryExecID, stdin, stdout, stderr)
}

// framedTerminal presents the framed attach connection as an io.ReadWriteCloser
// plus Resize: Read yields terminal output payloads, Write sends input frames,
// and Resize sends a resize frame.
type framedTerminal struct {
	frames  attachFrameTransport
	events  <-chan tui.TerminalEvent
	readBuf []byte // leftover output bytes from a partially consumed frame

	writeMu sync.Mutex
	ready   sync.Once
}

func (t *framedTerminal) Read(p []byte) (int, error) {
	var readyErr error
	t.ready.Do(func() { readyErr = t.frames.WriteFrame(attachFrameReady, nil) })
	if readyErr != nil {
		return 0, readyErr
	}
	if len(t.readBuf) > 0 {
		n := copy(p, t.readBuf)
		t.readBuf = t.readBuf[n:]
		return n, nil
	}
	for {
		frame, err := t.frames.ReadFrame()
		if err != nil {
			return 0, err
		}
		switch frame.typ {
		case attachFrameOutput:
			if len(frame.payload) == 0 {
				continue
			}
			n := copy(p, frame.payload)
			if n < len(frame.payload) {
				t.readBuf = frame.payload[n:]
			}
			return n, nil
		case attachFrameExit:
			if err := attachExitErrorFromPayload("terminal", frame.payload); err != nil {
				return 0, err
			}
			return 0, io.EOF
		case attachFrameError:
			return 0, fmt.Errorf("terminal: %s", string(frame.payload))
		default:
			// Ignore frames the embedded pane does not consume (e.g. ready).
		}
	}
}

func (t *framedTerminal) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.frames.WriteFrame(attachFrameInput, p); err != nil {
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
	return t.frames.WriteFrame(attachFrameResize, payload)
}

func (t *framedTerminal) Close() error {
	return t.frames.Close()
}

func (t *framedTerminal) Events() <-chan tui.TerminalEvent { return t.events }
