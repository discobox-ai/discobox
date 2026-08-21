package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/execstream"
	"github.com/discobox-ai/discobox/execstream/client"
	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/execstream/resume"
)

// Open connects a terminal for one of this CLI's own commands — apply — drawn
// in the launcher's overlay. The discobox's terminals come through OpenExec
// and NewShell instead.
func (d *apiDataSource) Open(ctx context.Context, action tui.Interaction, sandboxID string, cols, rows int) (tui.Terminal, error) {
	switch action {
	case tui.InteractApply:
		// These run on this machine rather than in the discobox; see
		// tui_local.go.
		return d.openLocalCommand(ctx, action, sandboxID, cols, rows)
	default:
		return nil, fmt.Errorf("%s is not a terminal", action)
	}
}

// Execs is the sandbox's exec sessions as the workspace's tab strip needs
// them.
func (d *apiDataSource) Execs(ctx context.Context, sandboxID string) ([]tui.Exec, error) {
	execs, err := d.app.listSandboxExecs(ctx, d.projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]tui.Exec, 0, len(execs))
	for _, exec := range execs {
		out = append(out, tuiExec(exec))
	}
	return out, nil
}

// tuiExec maps one API exec record to the launcher's own type, keeping API
// types out of the tui package.
func tuiExec(exec apimodel.SandboxExec) tui.Exec {
	// The startup command is what a harness terminal is actually running in
	// the foreground; the argv is the shell it was typed into.
	command := exec.StartupCommand
	if len(command) == 0 {
		command = exec.Command
	}
	live := false
	switch exec.Status {
	case apiclientgen.SandboxExecStatusInstalling,
		apiclientgen.SandboxExecStatusStarting,
		apiclientgen.SandboxExecStatusRunning:
		live = true
	}
	// A service names itself in exec metadata, which is what lets the
	// workspace draw its tab from this one listing rather than a poll of its
	// own (ADR 0068 §7).
	metadata := map[string]string(exec.Metadata.Value)
	return tui.Exec{
		ID:          exec.ID,
		Command:     command,
		Harness:     exec.HarnessId.Value,
		Primary:     exec.Primary.Value,
		Service:     metadata[sandboxServiceIDMetadata],
		ServiceName: metadata[sandboxServiceNameMetadata],
		Tty:         exec.Tty,
		Live:        live,
		CreatedAt:   exec.CreatedAt,
	}
}

// The exec metadata keys the sandbox-agent tags a service's exec with. They
// must match services.MetadataServiceID/Name in the sandbox-agent; the control
// plane proxies exec metadata opaquely, so this is where the two agree.
const (
	sandboxServiceIDMetadata   = "serviceId"
	sandboxServiceNameMetadata = "serviceName"
)

// Services is the sandbox's declared services, running or not — what the
// workspace's services menu is drawn from.
func (d *apiDataSource) Services(ctx context.Context, sandboxID string) ([]tui.Service, error) {
	client, err := d.app.apiClient()
	if err != nil {
		return nil, err
	}
	services, err := listSandboxServices(ctx, client, d.projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]tui.Service, 0, len(services))
	for _, service := range services {
		mapped := tui.Service{
			ID:          service.ID,
			Name:        service.Name,
			Description: service.Description.Or(""),
			Status:      string(service.Status),
			Problem:     service.Problem.Or(""),
			FileName:    service.FileName.Or(""),
			ExecID:      service.ExecId.Or(""),
			StartedAt:   service.StartedAt.Or(time.Time{}),
			Error:       service.Error.Or(""),
		}
		if code, ok := service.ExitCode.Get(); ok {
			exit := int(code)
			mapped.ExitCode = &exit
		}
		out = append(out, mapped)
	}
	return out, nil
}

// ServiceLogs is a service's transcript as the bytes it wrote, for the pane
// that draws a service with no running process. The two streams are merged
// here — a pane is one screen, and that is what a terminal does with them.
func (d *apiDataSource) ServiceLogs(ctx context.Context, sandboxID, serviceID string) ([]byte, error) {
	client, err := d.app.apiClient()
	if err != nil {
		return nil, err
	}
	res, err := client.ListSandboxServiceLogs(ctx, apiclientgen.ListSandboxServiceLogsParams{
		ProjectId: d.projectID,
		SandboxId: sandboxID,
		ServiceId: serviceID,
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.SandboxExecLogsResponse](res)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, entry := range body.GetEntries() {
		if entry.Stream == apiclientgen.SandboxExecLogEntryStreamInput {
			continue
		}
		out = append(out, entry.Data...)
	}
	return out, nil
}

// DoService runs one lifecycle verb against one declared service.
func (d *apiDataSource) DoService(ctx context.Context, verb tui.ServiceVerb, sandboxID, serviceID string) error {
	client, err := d.app.apiClient()
	if err != nil {
		return err
	}
	_, err = actOnSandboxService(ctx, client, string(verb), d.projectID, sandboxID, serviceID)
	return err
}

// OpenExec attaches to one existing exec session. tui.ExecPrimary is spelled
// the same as the wire's virtual primary id, and the sandbox resolves it —
// relaunching a primary terminal that has stopped — so nothing is started
// here.
func (d *apiDataSource) OpenExec(ctx context.Context, sandboxID, execID string, cols, rows int) (tui.Terminal, error) {
	return d.openFramedTerminal(ctx, sandboxID, execID, cols, rows)
}

// NewShell creates, attaches and starts a fresh interactive shell exec — what
// `discobox shell` with no command runs. Only the sandbox can say which shell its
// user has, so the request asks for one rather than naming it.
func (d *apiDataSource) NewShell(ctx context.Context, sandboxID string, cols, rows int) (tui.Exec, tui.Terminal, error) {
	return d.newSandboxSession(ctx, sandboxID, sandboxExecCreateOptions{
		interactive: true, tty: true, shell: true, env: paneTerminalEnv(),
	}, cols, rows)
}

// NewTerminal creates, attaches and starts another of the sandbox's own
// harness terminals — a second session of whatever harness it already runs.
// Which harness that is, is the sandbox's answer and not this machine's, so
// the request names none: an exec created with no command, no shell and no
// harness is a terminal on the sandbox's configured harness.
func (d *apiDataSource) NewTerminal(ctx context.Context, sandboxID string, cols, rows int) (tui.Exec, tui.Terminal, error) {
	return d.newSandboxSession(ctx, sandboxID, sandboxExecCreateOptions{
		interactive: true, tty: true, terminal: true, env: paneTerminalEnv(),
	}, cols, rows)
}

// newSandboxSession is what both of them are: create the exec, attach to it,
// and only then start it.
//
// A created exec is not a running one. It is started once the attach is up and
// its size is known, which is the order attachSandboxExec uses: started first,
// its opening output would go out before anything was listening, and it would
// draw itself at whatever size the sandbox guessed.
func (d *apiDataSource) newSandboxSession(ctx context.Context, sandboxID string, opts sandboxExecCreateOptions, cols, rows int) (tui.Exec, tui.Terminal, error) {
	body, err := createSandboxExecBody(opts, nil)
	if err != nil {
		return tui.Exec{}, nil, err
	}
	exec, err := d.app.createSandboxExec(ctx, d.projectID, sandboxID, body)
	if err != nil {
		return tui.Exec{}, nil, err
	}
	term, err := d.openFramedTerminal(ctx, sandboxID, exec.ID, cols, rows)
	if err != nil {
		return tui.Exec{}, nil, err
	}
	started, err := d.app.startSandboxExec(ctx, d.projectID, sandboxID, exec.ID)
	if err != nil {
		_ = term.Close()
		return tui.Exec{}, nil, err
	}
	return tuiExec(started), term, nil
}

func (d *apiDataSource) openFramedTerminal(ctx context.Context, sandboxID, execID string, cols, rows int) (tui.Terminal, error) {
	// Buffered and dropped rather than blocked on: a connection event nobody is
	// waiting for must not hold up the transport that produced it.
	events := make(chan tui.TerminalEvent, 4)
	frames, err := d.app.openReconnectingSandboxExecAttach(ctx, d.projectID, sandboxID, execID, execAttachOptions{
		replay: true,
		event: func(event resume.Event) {
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
		},
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

// paneTerminalEnv is what a pane's shell should be told about the terminal it is
// drawing into, taken from the terminal this window is running in.
//
// A bare name means "whatever this shell has", and one that is unset is not
// sent at all, so a sandbox is only ever told about a setting somebody made.
//
// TERM is deliberately not among them. The terminal on this side of a pane is
// not yours — it is an emulator, and the sandbox's own default of
// xterm-256color describes it accurately. Forwarding a TERM of xterm-kitty or
// alacritty would name a terminal the sandbox has no terminfo for and is not
// talking to anyway, which is how "unknown terminal type" happens.
//
// COLORTERM and NO_COLOR carry no such baggage: they are advisory, need no
// terminfo, and are exactly the two that say how much color to use. Passing
// them on costs nothing even when this window renders to less than the sandbox
// was told — whatever the sandbox emits is kept in full by the emulator and
// brought down to what your terminal can show once, at the edge.
func paneTerminalEnv() []string {
	return []string{"COLORTERM", "NO_COLOR"}
}

// framedTerminal presents the framed attach connection as the byte stream a
// pane draws: Read yields terminal output payloads, Write sends input frames,
// and Resize sends a resize frame.
type framedTerminal struct {
	frames  execstream.Conn
	events  <-chan tui.TerminalEvent
	readBuf []byte // leftover output bytes from a partially consumed frame

	writeMu sync.Mutex
	ready   sync.Once
}

func (t *framedTerminal) Read(p []byte) (int, error) {
	// The ready frame pairs with the replay this attach asked for: the remote
	// must not stream the scrollback until something is reading it. It is sent
	// on the first read rather than at open, because that is the moment there
	// demonstrably is one.
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
				t.readBuf = append([]byte(nil), f.Payload[n:]...)
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
			// Ignore frames the pane does not consume (e.g. ready).
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

func (t *framedTerminal) Close() error { return t.frames.Close() }

func (t *framedTerminal) Events() <-chan tui.TerminalEvent { return t.events }

// ensure the adapter satisfies what the launcher asks of a terminal.
var _ tui.Terminal = (*framedTerminal)(nil)
