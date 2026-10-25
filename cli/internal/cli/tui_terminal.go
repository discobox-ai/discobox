package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/execstream"
	"github.com/discobox-ai/discobox/pkg/execstream/client"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
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
		ID:           exec.ID,
		Command:      command,
		Harness:      exec.CurrentHarnessId.Or(exec.HarnessId.Value),
		Primary:      exec.Primary.Value,
		Service:      metadata[sandboxServiceIDMetadata],
		Tool:         execToolID(metadata[execToolMetadataKey]),
		FilesChanged: metadata[execFilesChangedMetadataKey] != "",
		ServiceName:  metadata[sandboxServiceNameMetadata],
		Tty:          exec.Tty,
		Live:         live,
		CreatedAt:    exec.CreatedAt,
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

// execFilesChangedMetadataKey is the exec metadata key a pull request's files
// changed carry (PullReview), and its value the head commit they were started
// over. Like the tool key it is this client's own, and it is what keeps the
// workspace from opening the session as a tab: the issue tab that started it
// draws it.
const execFilesChangedMetadataKey = "filesChanged"

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

// PullReview creates, attaches and starts discobox-review in the sandbox over a
// pull request's files changed: `discobox-review -base <BaseSHA> -head
// <HeadSHA>`, run in the sandbox's default working directory — its primary
// source, whose git directory keeps the review — as the program the diff tool
// runs, from the image, whatever tool a repository has declared in the diff
// tool's place. Comments written in it are signed with git's own user.name, as
// the diff tool's are.
//
// The sandbox has to have the pull request's branch checked out, whose review
// is the one shown, and its head, and is asked about both first:
// discobox-review would show another branch's review, or exit at once, and the
// tab could not say why. The base need
// not be: GitHub's is its base branch's tip as it is now, which the sandbox has
// not fetched once the branch has moved, so the base branch is named as well
// (-fallback-base), which measures the change from the same merge base.
//
// The two commits are GitHub's word, and go on a command line: each has to be
// a commit hash, so neither can be read as a flag.
func (d *apiDataSource) PullReview(ctx context.Context, sandboxID string, pull tui.PullRequest, cols, rows int) (tui.Exec, tui.Terminal, error) {
	d = d.at(sandboxID)
	if !commitHash(pull.BaseSHA) || !commitHash(pull.HeadSHA) {
		return tui.Exec{}, nil, fmt.Errorf("the pull request has not said which commits it is between")
	}
	// The review shown is the one kept for the branch checked out, so that
	// has to be the pull request's: another branch's card and approvals over
	// this diff would be read as this one's, and written to as well.
	branch, _, code, err := d.app.sandboxCommandOutput(ctx, d.projectID, sandboxID, "", []string{"git", "symbolic-ref", "--short", "-q", "HEAD"})
	switch branch = strings.TrimSpace(branch); {
	case err != nil:
		return tui.Exec{}, nil, fmt.Errorf("reading the discobox's branch: %w", err)
	case code != 0 || branch == "":
		return tui.Exec{}, nil, fmt.Errorf("this discobox has no branch checked out, so it keeps no review of %s", pull.Head)
	case terminalSafe(branch) != pull.HeadRef:
		// Both as the screen has them: the pull request's came made safe.
		// A fork's is its branch alone, as the discobox working on it
		// names it.
		return tui.Exec{}, nil, fmt.Errorf("this discobox has %s checked out, not the pull request's %s", terminalSafe(branch), pull.Head)
	}
	_, _, code, err = d.app.sandboxCommandOutput(ctx, d.projectID, sandboxID, "", []string{"git", "cat-file", "-e", pull.HeadSHA + "^{commit}"})
	switch {
	case err != nil:
		return tui.Exec{}, nil, fmt.Errorf("looking for its head in the discobox: %w", err)
	case code != 0:
		return tui.Exec{}, nil, fmt.Errorf("its head %s is not in this discobox — it was pushed from somewhere else", pull.HeadSHA[:12])
	}
	command := []string{"discobox-review", "-by", "git-user", "-base", pull.BaseSHA, "-head", pull.HeadSHA}
	if fallback := pullBaseFallback(pull.Base); fallback != "" {
		command = append(command, "-fallback-base", fallback)
	}
	return d.newSandboxSession(ctx, sandboxID, sandboxExecCreateOptions{
		interactive: true, tty: true, env: paneTerminalEnv(),
		metadata: map[string]string{execFilesChangedMetadataKey: pull.HeadSHA},
	}, cols, rows, command...)
}

// pullBaseFallback is the pull request's base branch as the sandbox's clone
// names it, or nothing when the name could be read as a flag or is not a
// branch name at all: the base commit is then all there is to go by.
func pullBaseFallback(branch string) string {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " ~^:?*[\\") || strings.Contains(branch, "..") {
		return ""
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return "origin/" + branch
}

// commitHash reports whether s is a whole commit hash: SHA-1's forty hex
// digits, or SHA-256's sixty-four.
func commitHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
	// A session created and then not handed back — the attach or the start
	// failed, or ctx was canceled between them — is nobody's: it is deleted
	// here, on a context of its own, since ctx may be what failed.
	abandon := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = d.app.deleteSandboxExec(ctx, d.projectID, sandboxID, exec.ID)
	}
	term, err := d.openFramedTerminal(ctx, sandboxID, exec.ID, cols, rows)
	if err != nil {
		abandon()
		return tui.Exec{}, nil, err
	}
	started, err := d.app.startSandboxExec(ctx, d.projectID, sandboxID, exec.ID)
	if err != nil {
		_ = term.Close()
		abandon()
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

// Repaint asks the sandbox to send this attacher the screen again, at the size
// the resize before it just re-asserted.
//
// The two frames are one thought and go out in order on one connection: the
// resize is what the exec's terminal is laid out to, and the repaint is what
// makes the answer arrive without waiting for the program to type something.
// An exec with no screen to repaint from — a pipe exec, a service's log —
// ignores the frame.
func (t *framedTerminal) Repaint() error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.frames.WriteFrame(frame.Repaint, nil)
}

func (t *framedTerminal) Close() error { return t.frames.Close() }

func (t *framedTerminal) Events() <-chan tui.TerminalEvent { return t.events }

// ensure the adapter satisfies what the launcher asks of a terminal.
var _ tui.Terminal = (*framedTerminal)(nil)
