package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// `discobox desktop` opens the console as a native app: the same console as
// `discobox console`, drawn by a desktop binary (desktop/, a Rust app on Rune)
// that this process starts and serves. The desktop app holds no credentials
// and knows no servers. It asks this process, over its stdin and stdout, for
// everything the console's DataSource answers, so every transport, login and
// reconnect is the CLI's own.
//
// The design is ADR 26-11-07-876, -983 and -289; desktop/README.md says what
// is built so far.

// desktopBinaryEnv names a desktop binary to run in place of the one this CLI
// would run — for developing the desktop app against a released CLI.
const desktopBinaryEnv = "DISCOBOX_DESKTOP_BINARY"

func (a *App) newDesktopCommand() *cobra.Command {
	var (
		binary    string
		printSpec bool
	)
	cmd := &cobra.Command{
		Use:   "desktop",
		Short: "Open the discobox console as a desktop app",
		Long: `Open the console as a native desktop app.

It is the same console as "discobox console": the discoboxes, and a box's
terminals beside them. It talks to the server through this command, so
--server, --token and every configured server work as they do for the
console.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if printSpec {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(console.NewSpec())
			}
			return a.runDesktop(cmd, binary)
		},
	}
	cmd.Flags().BoolVar(&printSpec, "print-spec", false, "print the console's UX spec the desktop app is handed, and exit")
	_ = cmd.Flags().MarkHidden("print-spec")
	cmd.Flags().StringVar(&binary, "binary", "", "desktop app to run instead of the one this CLI would find (also "+desktopBinaryEnv+")")
	_ = cmd.Flags().MarkHidden("binary")
	return cmd
}

func (a *App) runDesktop(cmd *cobra.Command, binary string) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	path, err := desktopBinary(binary)
	if err != nil {
		return err
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	ds, err := newAPIDataSource(ctx, a, client, projectID)
	if err != nil {
		return err
	}
	defer ds.waitForPushes(cmd.ErrOrStderr())

	child := exec.CommandContext(ctx, path, "--bridge")
	child.Env = desktopEnv(os.Environ())
	// The app logs what the bridge carried, and that is text a box wrote.
	stderr := &terminalSafeLines{w: cmd.ErrOrStderr()}
	child.Stderr = stderr
	toChild, err := child.StdinPipe()
	if err != nil {
		return err
	}
	fromChild, err := child.StdoutPipe()
	if err != nil {
		return err
	}
	if err := child.Start(); err != nil {
		return fmt.Errorf("start the desktop app: %w", err)
	}
	b := &desktopBridge{ds: ds, out: toChild, streams: map[uint32]*desktopStream{}, boxes: map[string]desktopBoxRef{}}
	go b.serve(ctx, fromChild)
	err = child.Wait()
	stderr.flush()
	cancel()
	b.closeAll()
	return err
}

// desktopEnv is the app's environment: this process's, less every discobox
// variable but the app's own (DISCOBOX_DESKTOP_*). The app holds no
// credentials and knows no servers (ADR 26-11-07-983 §1), so neither
// DISCOBOX_TOKEN nor a server's address reaches it.
func desktopEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "DISCOBOX_") && !strings.HasPrefix(strings.ToUpper(name), "DISCOBOX_DESKTOP_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// terminalSafeLines writes the app's stderr to this terminal a line at a
// time, each through terminalSafeMultiline: the app logs requests and answers
// that carry what boxes wrote (names, messages, errors folded from a box's
// stderr), and a control sequence in them is display data, not an
// instruction to the user's terminal.
type terminalSafeLines struct {
	mu      sync.Mutex
	w       io.Writer
	pending []byte
}

func (t *terminalSafeLines) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = append(t.pending, p...)
	for {
		i := bytes.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		line := string(t.pending[:i+1])
		t.pending = t.pending[i+1:]
		if _, err := io.WriteString(t.w, terminalSafeMultiline(line)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// flush writes what is left after the app's last newline.
func (t *terminalSafeLines) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) > 0 {
		_, _ = io.WriteString(t.w, terminalSafeMultiline(string(t.pending))+"\n")
		t.pending = nil
	}
}

// desktopBinary finds the desktop app: the flag, then the environment, then one
// beside this executable (where `go tool task build:desktop` puts it), then one
// on PATH. A released CLI downloads the app its manifest pins instead (ADR
// 26-11-07-983 §2); until that lands, this is the only way it is found.
func desktopBinary(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(desktopBinaryEnv); env != "" {
		return env, nil
	}
	name := "discobox-desktop"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(beside); err == nil {
			return beside, nil
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	return "", errors.New("no desktop app found: build it with `go tool task build:desktop`, or name one with --binary or " + desktopBinaryEnv)
}

// ---------------------------------------------------------------------------
// The bridge
//
// One stream of frames each way: kind (1 byte), stream (u32), length (u32),
// payload. Kind 255 is a JSON message on stream 0: a request from the app
// ({"id","op",...}), its answer ({"id","result"} or {"id","error"}), or an
// event this side pushes ({"event",...}). Every other kind is an execstream
// frame type for one open terminal: Stdout and Exit from here, Input, Resize
// and Repaint from the app.

const bridgeJSON byte = 255

type desktopBridge struct {
	ds *apiDataSource

	writeMu sync.Mutex
	out     io.Writer

	mu      sync.Mutex
	streams map[uint32]*desktopStream
	// boxes is every box of the last listing the app was sent, by the key the
	// app knows it by: what routes a request about a box to its server, as
	// the window's ForServer(box.Server) does.
	boxes map[string]desktopBoxRef
}

// desktopBoxRef is a box as the bridge routes it: the server it is on, as the
// listing names it ("" when only one server is configured), and its ID there.
// Two servers can list the same ID, so the ID alone names no box
// (cli/internal/tui/REVIEW.md).
type desktopBoxRef struct{ server, id string }

// desktopBoxKey is the key the app knows a box by: its ID when the listing
// names no server (one is configured), and the server's name and the ID
// otherwise. The app treats it as opaque and shows the box's sandboxId.
func desktopBoxKey(server, id string) string {
	if server == "" {
		return id
	}
	return server + "/" + id
}

// desktopStream is one terminal the app has open, and what ends the
// goroutines that serve it.
type desktopStream struct {
	term console.Terminal
	done chan struct{}
}

type bridgeRequest struct {
	ID      int64    `json:"id"`
	Op      string   `json:"op"`
	Sandbox string   `json:"sandbox,omitempty"`
	Exec    string   `json:"exec,omitempty"`
	Stream  uint32   `json:"stream,omitempty"`
	Cols    int      `json:"cols,omitempty"`
	Rows    int      `json:"rows,omitempty"`
	Verb    string   `json:"verb,omitempty"`
	Targets []string `json:"targets,omitempty"`
}

func (b *desktopBridge) write(kind byte, stream uint32, payload []byte) error {
	var head [9]byte
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:5], stream)
	binary.BigEndian.PutUint32(head[5:9], uint32(len(payload)))
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if _, err := b.out.Write(head[:]); err != nil {
		return err
	}
	_, err := b.out.Write(payload)
	return err
}

func (b *desktopBridge) send(v any) {
	data, err := json.Marshal(v)
	if err == nil {
		_ = b.write(bridgeJSON, 0, data)
	}
}

func (b *desktopBridge) serve(ctx context.Context, in io.Reader) {
	r := bufio.NewReaderSize(in, 64*1024)
	go b.pollListing(ctx)
	for {
		var head [9]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return
		}
		kind, stream, size := head[0], binary.BigEndian.Uint32(head[1:5]), binary.BigEndian.Uint32(head[5:9])
		if size > 16<<20 {
			return
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return
		}
		if kind == bridgeJSON {
			var req bridgeRequest
			if err := json.Unmarshal(payload, &req); err == nil {
				go b.handle(ctx, req)
			}
			continue
		}
		b.mu.Lock()
		st := b.streams[stream]
		b.mu.Unlock()
		if st == nil {
			continue
		}
		term := st.term
		switch kind {
		case frame.Input:
			_, _ = term.Write(payload)
		case frame.Resize:
			var size frame.ResizePayload
			if json.Unmarshal(payload, &size) == nil {
				_ = term.Resize(int(size.Cols), int(size.Rows))
			}
		case frame.Repaint:
			_ = term.Repaint()
		case frame.CloseInput:
			b.closeStream(stream)
		}
	}
}

func (b *desktopBridge) reply(id int64, result any, err error) {
	if err != nil {
		b.send(map[string]any{"id": id, "error": err.Error()})
		return
	}
	b.send(map[string]any{"id": id, "result": result})
}

func (b *desktopBridge) handle(ctx context.Context, req bridgeRequest) {
	switch req.Op {
	case "hello":
		b.reply(req.ID, map[string]any{"spec": console.NewSpec()}, nil)
	case "list":
		listing, err := b.list(ctx)
		b.reply(req.ID, desktopListing(listing), err)
	case "actions":
		listing, err := b.list(ctx)
		if err != nil {
			b.reply(req.ID, nil, err)
			return
		}
		var targets []console.Sandbox
		for _, s := range listing.Sandboxes {
			for _, key := range req.Targets {
				if desktopBoxKey(s.Server, s.ID) == key {
					targets = append(targets, s)
				}
			}
		}
		b.reply(req.ID, desktopActions(console.Lifecycle(targets)), nil)
	case "execs", "open", "newShell", "newTerminal", "endExec", "do":
		ds, sandboxID, err := b.source(ctx, req.Sandbox)
		if err != nil {
			if req.Op == "endExec" {
				b.closeStream(req.Stream)
			}
			b.reply(req.ID, nil, err)
			return
		}
		req.Sandbox = sandboxID
		b.handleBox(ctx, ds, req)
	default:
		b.reply(req.ID, nil, fmt.Errorf("unknown op %q", req.Op))
	}
}

// list is the listing, noting each box in it by the key the app knows it by.
func (b *desktopBridge) list(ctx context.Context) (console.Listing, error) {
	listing, err := b.ds.List(ctx)
	if err != nil {
		return listing, err
	}
	boxes := make(map[string]desktopBoxRef, len(listing.Sandboxes))
	for _, s := range listing.Sandboxes {
		boxes[desktopBoxKey(s.Server, s.ID)] = desktopBoxRef{server: s.Server, id: s.ID}
	}
	b.mu.Lock()
	b.boxes = boxes
	b.mu.Unlock()
	return listing, nil
}

// source is the data source for the server a box is on, and the box's ID
// there, from the key the app named it by. A key this side has not listed is
// listed again first, and one no server lists is an error, never a request to
// the primary server about another server's box.
func (b *desktopBridge) source(ctx context.Context, key string) (console.DataSource, string, error) {
	b.mu.Lock()
	ref, ok := b.boxes[key]
	b.mu.Unlock()
	if !ok {
		if _, err := b.list(ctx); err != nil {
			return nil, "", err
		}
		b.mu.Lock()
		ref, ok = b.boxes[key]
		b.mu.Unlock()
		if !ok {
			return nil, "", fmt.Errorf("no discobox %s is listed", key)
		}
	}
	ds := b.ds.ForServer(ref.server)
	if ds == nil {
		return nil, "", fmt.Errorf("the server %s is not answering", ref.server)
	}
	return ds, ref.id, nil
}

// handleBox answers a request about one box, from the source for its server.
func (b *desktopBridge) handleBox(ctx context.Context, ds console.DataSource, req bridgeRequest) {
	switch req.Op {
	case "execs":
		execs, err := ds.Execs(ctx, req.Sandbox)
		if errors.Is(err, console.ErrNotRunning) {
			execs, err = nil, nil
		}
		b.reply(req.ID, desktopExecs(execs), err)
	case "open":
		term, err := ds.OpenExec(ctx, req.Sandbox, req.Exec, req.Cols, req.Rows)
		if err == nil {
			b.attach(req.Stream, term)
		}
		b.reply(req.ID, map[string]any{"stream": req.Stream}, err)
	case "newShell", "newTerminal":
		var (
			ex   console.Exec
			term console.Terminal
			err  error
		)
		if req.Op == "newShell" {
			ex, term, err = ds.NewShell(ctx, req.Sandbox, req.Cols, req.Rows)
		} else {
			ex, term, err = ds.NewTerminal(ctx, req.Sandbox, req.Cols, req.Rows)
		}
		if err == nil {
			b.attach(req.Stream, term)
		}
		b.reply(req.ID, desktopExec(ex), err)
	case "endExec":
		b.closeStream(req.Stream)
		b.reply(req.ID, nil, ds.EndExec(ctx, req.Sandbox, req.Exec))
	case "do":
		note, err := ds.Do(ctx, console.Verb(req.Verb), req.Sandbox, desktopCredentials())
		b.reply(req.ID, map[string]any{"note": note}, err)
	}
}

// desktopCredentials answers a verb's credential question while the desktop
// app has no dialog for one: it declines, and says so.
func desktopCredentials() console.CredentialUI {
	return console.CredentialUI{
		Report: func(string) {},
		Confirm: func(context.Context, string) (bool, error) {
			return false, errors.New("the desktop app cannot answer a credential question yet")
		},
		Run: func(context.Context, *exec.Cmd) error {
			return errors.New("the desktop app cannot hand over the terminal")
		},
	}
}

func (b *desktopBridge) pollListing(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		if listing, err := b.list(ctx); err == nil {
			b.send(map[string]any{"event": "listing", "listing": desktopListing(listing)})
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *desktopBridge) attach(stream uint32, term console.Terminal) {
	st := &desktopStream{term: term, done: make(chan struct{})}
	b.mu.Lock()
	b.streams[stream] = st
	b.mu.Unlock()
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := term.Read(buf)
			if n > 0 {
				_ = b.write(frame.Stdout, stream, append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				msg := ""
				if !errors.Is(err, io.EOF) {
					msg = err.Error()
				}
				_ = b.write(frame.Exit, stream, []byte(msg))
				b.closeStream(stream)
				return
			}
		}
	}()
	// Events is never closed while the terminal is open, and need not be
	// after: this ends when the stream does.
	go func() {
		for {
			select {
			case <-st.done:
				return
			case ev := <-term.Events():
				state := "reconnected"
				if ev.State == console.TerminalReconnecting {
					state = "reconnecting"
				}
				b.send(map[string]any{"event": "stream", "stream": stream, "state": state})
			}
		}
	}()
}

func (b *desktopBridge) closeStream(stream uint32) {
	b.mu.Lock()
	st := b.streams[stream]
	delete(b.streams, stream)
	b.mu.Unlock()
	if st != nil {
		close(st.done)
		_ = st.term.Close()
	}
}

func (b *desktopBridge) closeAll() {
	b.mu.Lock()
	streams := b.streams
	b.streams = map[uint32]*desktopStream{}
	b.mu.Unlock()
	for _, st := range streams {
		close(st.done)
		_ = st.term.Close()
	}
}

// The wire's shapes: what the desktop app draws a row and a tab from.

type desktopSandbox struct {
	// ID is the key the app knows the box by (desktopBoxKey), SandboxID its
	// ID on its server, which is what the app shows.
	ID           string    `json:"id"`
	SandboxID    string    `json:"sandboxId"`
	Server       string    `json:"server,omitempty"`
	Name         string    `json:"name"`
	State        string    `json:"state"`
	Agent        string    `json:"agent,omitempty"`
	AgentMsg     string    `json:"agentMsg,omitempty"`
	Harness      string    `json:"harness"`
	Source       string    `json:"source"`
	Tags         []string  `json:"tags,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	Commit       string    `json:"commit,omitempty"`
	Dirty        bool      `json:"dirty,omitempty"`
	Added        int       `json:"added,omitempty"`
	Deleted      int       `json:"deleted,omitempty"`
	CPU          int       `json:"cpu,omitempty"`
	MemoryBytes  int64     `json:"memoryBytes,omitempty"`
	Created      time.Time `json:"created"`
	Parent       string    `json:"parent,omitempty"`
	Message      string    `json:"message,omitempty"`
	Provisioning string    `json:"provisioning,omitempty"`
}

// desktopParent is the key of the box that made this one, which is on the
// same server.
func desktopParent(s console.Sandbox) string {
	if s.CreatedBy == "" {
		return ""
	}
	return desktopBoxKey(s.Server, s.CreatedBy)
}

func desktopListing(l console.Listing) []desktopSandbox {
	out := make([]desktopSandbox, 0, len(l.Sandboxes))
	for _, s := range l.Sandboxes {
		d := desktopSandbox{
			ID: desktopBoxKey(s.Server, s.ID), SandboxID: s.ID, Server: s.Server, Name: s.Name, State: string(s.State), Agent: string(s.Agent.State), AgentMsg: s.Agent.Msg,
			Harness: s.HarnessName, Source: s.Source, Tags: s.Tags, Branch: s.Branch, Commit: s.Commit, Dirty: s.Dirty,
			Created: s.Created, Parent: desktopParent(s), Message: s.Message, Provisioning: s.Provisioning,
		}
		if d.Harness == "" {
			d.Harness = s.Harness
		}
		if s.Git.Known {
			d.Branch, d.Commit, d.Dirty = s.Git.Branch, s.Git.Commit, s.Git.Dirty
		}
		if s.Diff.Known {
			d.Added, d.Deleted = s.Diff.Added, s.Diff.Deleted
		}
		if s.Usage.Known {
			d.CPU, d.MemoryBytes = s.Usage.CPUPercent, s.Usage.MemoryBytes
		}
		out = append(out, d)
	}
	return out
}

// desktopAction is one entry of a box's action menu: the command it runs,
// whether it can now, and why not when it cannot. The app shows its own
// binding for the command beside it.
type desktopAction struct {
	Command string `json:"command"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Enabled bool   `json:"enabled"`
	Why     string `json:"why,omitempty"`
}

func desktopActions(entries []console.Entry) []desktopAction {
	out := make([]desktopAction, 0, len(entries))
	for _, e := range entries {
		a := desktopAction{Command: e.ID, Label: e.Label, Detail: e.Detail, Enabled: e.Enabled}
		if !e.Enabled {
			a.Why = e.Why
		}
		out = append(out, a)
	}
	return out
}

type desktopExecInfo struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Label string   `json:"label"`
	Live  bool     `json:"live"`
	Argv  []string `json:"argv,omitempty"`
}

// desktopExecs is what a workspace opens, as the console's does: the primary,
// and every other live terminal; services and files-changed sessions are drawn
// by the console in other ways, which the desktop app does not have yet.
func desktopExecs(execs []console.Exec) []desktopExecInfo {
	out := []desktopExecInfo{{ID: console.ExecPrimary, Kind: "primary", Live: true}}
	for _, e := range execs {
		if e.Primary || e.Service != "" || e.FilesChanged || !e.Live || !e.Tty {
			continue
		}
		out = append(out, desktopExec(e))
	}
	return out
}

func desktopExec(e console.Exec) desktopExecInfo {
	kind := "shell"
	switch {
	case e.Primary:
		kind = "primary"
	case e.Harness != "":
		kind = "terminal"
	case e.Tool != "":
		kind = "tool"
	}
	label := e.Harness
	if e.Tool != "" {
		label = e.Tool
	} else if label == "" && len(e.Command) > 0 {
		label = filepath.Base(e.Command[0])
	}
	return desktopExecInfo{ID: e.ID, Kind: kind, Label: label, Live: e.Live, Argv: e.Command}
}
