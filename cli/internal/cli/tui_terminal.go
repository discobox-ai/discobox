package cli

import (
	"bytes"
	"context"
	"fmt"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/execstream/client"
	"github.com/discobox-ai/discobox/pkg/execstream/resume"
	"github.com/discobox-ai/discobox/pkg/tools"
)

// Open connects a terminal for one of this CLI's own commands — apply — drawn
// in the launcher's overlay. The discobox's terminals come through OpenExec
// and NewShell instead.
func (d *apiDataSource) Open(ctx context.Context, action tui.Interaction, sandboxID string, cols, rows int) (tui.Terminal, error) {
	d = d.at(sandboxID)
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
	d = d.at(sandboxID)
	execs, observedAt, err := d.app.listSandboxExecs(ctx, d.projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	if !observedAt.IsZero() {
		return nil, tui.ErrNotRunning
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
	// A service names itself in exec metadata, which is how the workspace
	// tells a service's exec from a terminal or a shell in this listing and
	// pairs it with the services listing it polls beside it (ADR 0070 §2, §7).
	metadata := map[string]string(exec.Metadata.Value)
	return tui.Exec{
		ID:          exec.ID,
		Command:     command,
		Harness:     exec.HarnessId.Value,
		Primary:     exec.Primary.Value,
		Service:     metadata[sandboxServiceIDMetadata],
		Tool:        execToolID(metadata[execToolMetadataKey]),
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

// execToolMetadataKey is the exec metadata key a tool session carries, and its
// value is the tool's id. It is the only thing that tells a tool session from
// a shell in the listing, so every window that draws a workspace has to read
// the same key — which is why it is spelled once, here, at the wire. Unlike the
// service keys it is this client's own: the sandbox knows nothing about tools.
// See ADR 26-08-27-302 on tool sessions.
const execToolMetadataKey = "tool"

// legacyDiffToolID is the label a diff session carries when a launcher from
// before the diff tool had a stable id opened it. The label is stored in the
// sandbox with the exec, so such a session is still running in boxes this
// launcher attaches to.
const legacyDiffToolID = "diff"

// execToolID is a tool session's label as this launcher names that tool: the
// stored label, except the diff's old one, which is read as tools.DiffID so the
// running diff is picked back up as the diff rather than stranded while `d`
// starts a second one (ADR 0125 §2).
func execToolID(label string) string {
	if label == legacyDiffToolID {
		return tools.DiffID
	}
	return label
}

// Services is the sandbox's declared services, running or not — what the
// workspace's services menu is drawn from.
func (d *apiDataSource) Services(ctx context.Context, sandboxID string) ([]tui.Service, error) {
	d = d.at(sandboxID)
	client, err := d.app.apiClient()
	if err != nil {
		return nil, err
	}
	services, observedAt, err := listSandboxServices(ctx, client, d.projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	if !observedAt.IsZero() {
		return nil, tui.ErrNotRunning
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
	d = d.at(sandboxID)
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
	d = d.at(sandboxID)
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
	d = d.at(sandboxID)
	return d.openFramedTerminal(ctx, sandboxID, execID, cols, rows)
}

// NewShell creates, attaches and starts a fresh interactive shell exec — what
// `discobox shell` with no command runs. Only the sandbox can say which shell its
// user has, so the request asks for one rather than naming it — along with the
// shell this machine's user prefers (preferredShellEnv), which the sandbox runs
// instead when it has it.
func (d *apiDataSource) NewShell(ctx context.Context, sandboxID string, cols, rows int) (tui.Exec, tui.Terminal, error) {
	d = d.at(sandboxID)
	return d.newSandboxSession(ctx, sandboxID, sandboxExecCreateOptions{
		interactive: true, tty: true, shell: true, env: append(paneTerminalEnv(), preferredShellEnv()...),
	}, cols, rows)
}

// NewTerminal creates, attaches and starts another of the sandbox's own
// harness terminals — a second session of whatever harness it already runs.
// Which harness that is, is the sandbox's answer and not this machine's, so
// the request names none: an exec created with no command, no shell and no
// harness is a terminal on the sandbox's configured harness.
func (d *apiDataSource) NewTerminal(ctx context.Context, sandboxID string, cols, rows int) (tui.Exec, tui.Terminal, error) {
	d = d.at(sandboxID)
	return d.newSandboxSession(ctx, sandboxID, sandboxExecCreateOptions{
		interactive: true, tty: true, terminal: true, env: paneTerminalEnv(),
	}, cols, rows)
}

// NewTool creates, attaches and starts a tool session: the tool's command, run
// in the sandbox's default working directory — which is its primary source —
// and labeled with the tool it is.
//
// No workdir is sent for the same reason `discobox tools git` sends none: an
// exec with no workdir already lands in the sandbox's primary source directory,
// and naming it here would be this machine guessing at a path only the sandbox
// knows.
func (d *apiDataSource) NewTool(ctx context.Context, sandboxID, toolID string, cols, rows int) (tui.Exec, tui.Terminal, error) {
	d = d.at(sandboxID)
	catalog, _, err := d.app.toolCatalog(ctx, d.client, d.projectID, sandboxID)
	if err != nil {
		return tui.Exec{}, nil, err
	}
	def, err := findTool(catalog, toolID)
	if err != nil {
		return tui.Exec{}, nil, err
	}
	if def.Runs != tools.RunsSandbox {
		return tui.Exec{}, nil, fmt.Errorf("tool %s runs here, not in the discobox", toolID)
	}
	// Before the session, not alongside it: the tool reads its configuration
	// when it starts, so a file that lands a moment later is a file this run
	// never saw. See tui_tools.go.
	if err := d.app.installToolFiles(ctx, d.projectID, sandboxID, def); err != nil {
		return tui.Exec{}, nil, err
	}
	command, err := d.app.sandboxToolCommand(ctx, d.projectID, sandboxID, def, nil)
	if err != nil {
		return tui.Exec{}, nil, err
	}
	return d.newSandboxSession(ctx, sandboxID, sandboxExecCreateOptions{
		interactive: true, tty: true, env: append(paneTerminalEnv(), def.Env...),
		metadata: map[string]string{execToolMetadataKey: def.ID},
	}, cols, rows, command...)
}

// EndExec ends one exec session in the sandbox, killing what is running in it.
func (d *apiDataSource) EndExec(ctx context.Context, sandboxID, execID string) error {
	d = d.at(sandboxID)
	return d.app.deleteSandboxExec(ctx, d.projectID, sandboxID, execID)
}

func (d *apiDataSource) PutImage(ctx context.Context, sandboxID, name string, data []byte) (string, error) {
	d = d.at(sandboxID)
	stat, err := d.app.writeSandboxFile(ctx, apiclientgen.WriteSandboxFileParams{
		ProjectId: d.projectID,
		SandboxId: sandboxID,
		Path:      pastedImageDir + "/" + name,
		Parents:   apiclientgen.NewOptBool(true),
	}, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("copy %s into the discobox: %w", name, err)
	}
	return stat.Path, nil
}

// SendInput types text into an exec and presses Enter — a message to the
// harness running there, which is how an AHP client's message reaches it
// (ADR 26-10-17-354 §3).
func (d *apiDataSource) SendInput(ctx context.Context, sandboxID, execID, text string) error {
	d = d.at(sandboxID)
	_, err := sendSandboxExecInput(ctx, d.client, d.projectID, sandboxID, execID, []apimodel.SandboxExecInputPart{
		{Text: apiclientgen.NewOptString(text)},
		{Key: apiclientgen.NewOptString("Enter")},
	})
	return err
}

// pastedImageDir is the directory of pasted images in the discobox, where
// PutImage writes each through the files API as the discobox's user (ADR
// 26-10-22-499 §4). The name is the console's own — a timestamp and a base
// name already stripped to a safe alphabet — and the path the write answers
// with is the path a program in the discobox opens.
const pastedImageDir = "/tmp/discobox-paste"

// newSandboxSession is what all of them are: create the exec, attach to it,
// and only then start it.
//
// A created exec is not a running one. It is started once the attach is up and
// its size is known, which is the order attachSandboxExec uses: started first,
// its opening output would go out before anything was listening, and it would
// draw itself at whatever size the sandbox guessed.
func (d *apiDataSource) newSandboxSession(ctx context.Context, sandboxID string, opts sandboxExecCreateOptions, cols, rows int, command ...string) (tui.Exec, tui.Terminal, error) {
	body, err := createSandboxExecBody(opts, command)
	if err != nil {
		return tui.Exec{}, nil, err
	}
	exec, err := d.app.createSandboxExec(ctx, d.projectID, sandboxID, body, true)
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
	term := &framedTerminal{Terminal: client.NewTerminal(frames), events: events}
	if err := term.Resize(cols, rows); err != nil {
		_ = term.Close()
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

// framedTerminal is the shared frames-to-bytes terminal (client.Terminal),
// plus the connection events a pane draws and that never appear in the output.
type framedTerminal struct {
	*client.Terminal
	events <-chan tui.TerminalEvent
}

func (t *framedTerminal) Events() <-chan tui.TerminalEvent { return t.events }

// ensure the adapter satisfies what the launcher asks of a terminal.
var _ tui.Terminal = (*framedTerminal)(nil)
