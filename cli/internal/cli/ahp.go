package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/ahphost"
	"github.com/discobox-ai/discobox/pkg/version"
)

// ahpDefaultPort is where `discobox admin ahp` listens unless told
// otherwise, on loopback: a port that stays the same from run to run, so the
// entry VS Code was given once goes on working.
//
// No forward Discobox makes of a discobox's port takes it (reservedLocalPorts),
// and the help tells the user to keep VS Code's own forwarding off it. The
// entry VS Code keeps carries the token, and a discobox that listened on this
// number while the host was not running would otherwise be forwarded onto it
// and be handed the token by the next connection VS Code makes.
const ahpDefaultPort = 7591

var ahpDefaultListen = net.JoinHostPort("127.0.0.1", strconv.Itoa(ahpDefaultPort))

// reservedLocalPorts are the local ports no forward of a discobox's port
// takes. See ahpDefaultPort.
func reservedLocalPorts() []int { return []int{ahpDefaultPort} }

func (a *App) newAHPCommand() *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "ahp",
		Short: "Serve your discoboxes to VS Code's Agent Sessions",
		Long: `Run an Agent Host Protocol host that VS Code's Agents window connects to, and
serve it every discobox on every server this client knows (ADR 26-10-17-354).

Each discobox is a session. Each of its harness terminals is a chat, whose
screen is the live terminal — what "discobox attach --raw" shows — and a
message sent in the chat is typed into it. A terminal VS Code opens for a
session is a shell in that discobox. Archiving or deleting a session archives
the discobox. Looking at a discobox that is stopped does not start it; sending
it a message or typing into it does.

It prints the entry to add to VS Code's "chat.remoteAgentHosts" setting. The
connection token in it is kept with this client's state, so the entry stays
good from one run to the next.

The entry dials 127.0.0.1:7591 whether or not this command is running, so that
port must not be one a discobox can listen behind. Discobox's own port
forwards never take it. VS Code forwards a discobox it has open over
Remote-SSH by itself, though: keep it off this port, for example with
"remote.portsAttributes": {"7591": {"onAutoForward": "ignore"}} in your user
settings.

To open one discobox in a full editor, a discobox serves itself too
(ADR 26-10-22-853): set "chat.sshRemoteAgentHostCommand" to
"discobox-sandbox-agent ahp" and run VS Code's "Agents: Connect to Remote Agent
Host via SSH" on the discobox's SSH host. Its session has the discobox's files
and changes, and Open in Editor opens it over Remote-SSH.`,
		Example: `  discobox admin ahp
  discobox admin ahp --listen 127.0.0.1:9000`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runAHP(cmd, listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ahpDefaultListen, "Loopback address to listen on; only the default port is kept from discobox port forwards")
	return cmd
}

// ahpSSHCommandSetting is the VS Code setting naming the command VS Code runs
// over SSH to start an agent host, and ahpSSHCommand the sandbox agent's
// (ADR 26-10-22-853 §3).
const (
	ahpSSHCommandSetting = "chat.sshRemoteAgentHostCommand"
	ahpSSHCommand        = "discobox-sandbox-agent ahp"
)

// ahpHostEntry is one entry of VS Code's chat.remoteAgentHosts setting.
type ahpHostEntry struct {
	Name            string `json:"name"`
	Address         string `json:"address"`
	ConnectionToken string `json:"connectionToken"`
}

func (a *App) runAHP(cmd *cobra.Command, listen string) error {
	ctx := cmd.Context()
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
	// A create pushes its source in the background (App.waitForPushes), and
	// leaving without it would cut a push off mid-lease.
	defer ds.waitForPushes(cmd.ErrOrStderr())
	ln, err := listenLoopback(ctx, listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	token, err := ahpToken()
	if err != nil {
		return err
	}
	// A session VS Code creates without a folder on this machine is cut from
	// the directory the host runs in, as `discobox new` would be here.
	source, err := os.Getwd()
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	host := ahphost.New(ahphost.Config{
		Boxes:   ahpBoxes{ds: ds},
		Token:   token,
		Source:  source,
		Version: version.String(),
		Logf: func(format string, args ...any) {
			_, _ = fmt.Fprintln(stderr, terminalSafe(fmt.Sprintf(format, args...)))
		},
	})
	entry, err := json.MarshalIndent(ahpHostEntry{Name: "discobox", Address: "ws://" + ln.Addr().String(), ConnectionToken: token}, "", "  ")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "Serving the Agent Host Protocol on ws://%s. Add this to VS Code's chat.remoteAgentHosts:\n", ln.Addr())
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(entry)); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "To open one discobox in an editor, set %q to %q and connect to that discobox with \"Agents: Connect to Remote Agent Host via SSH\".\n", ahpSSHCommandSetting, ahpSSHCommand)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: host, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	go func() { _ = host.Run(ctx) }()
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listenLoopback listens on address, which must be a loopback one: the host
// serves plain ws:// with the token in the URL, so anywhere else it would hand
// every discobox to whoever can read the traffic (ADR 26-10-17-354 §1).
func listenLoopback(ctx context.Context, address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("--listen %s: the host listens on loopback only", address)
	}
	var config net.ListenConfig
	return config.Listen(ctx, "tcp", address)
}

// ahpToken is the connection token VS Code presents, made once and kept in
// <state>/ahp-token: the setting entry holding it is pasted once, and a token
// that changed every run would break it every run.
func ahpToken() (string, error) {
	path := filepath.Join(cliStateDir(), "ahp-token")
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := ensureStateDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := writePrivateFile(path, []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}

// ahpBoxes is the console's data source as the AHP host reads it: the same
// calls, every server's, with the host's own types at the boundary — the host
// is shared with the sandbox agent (ADR 26-10-22-853) and cannot import the
// console's.
type ahpBoxes struct{ ds *apiDataSource }

func (b ahpBoxes) List(ctx context.Context) (ahphost.Listing, error) {
	listing, err := b.ds.List(ctx)
	if err != nil {
		return ahphost.Listing{}, err
	}
	out := ahphost.Listing{Quiet: append(append([]string(nil), listing.Unreachable...), listing.Waiting...)}
	for _, box := range listing.Sandboxes {
		out.Boxes = append(out.Boxes, ahpBox(box))
	}
	return out, nil
}

func ahpBox(box tui.Sandbox) ahphost.Box {
	return ahphost.Box{
		ID: box.ID, Name: box.Name, Server: box.Server, State: ahphost.BoxState(box.State), Agent: box.Agent,
		Harness: box.Harness, HarnessName: box.HarnessName, Source: box.Source, SourceRemote: box.SourceRemote, Workdir: box.Workdir, Platform: box.Platform, Created: box.Created,
	}
}

func ahpExec(exec tui.Exec) ahphost.Exec {
	return ahphost.Exec{ID: exec.ID, Harness: exec.Harness, Tool: exec.Tool, Service: exec.Service, Primary: exec.Primary, Live: exec.Live}
}

// Harnesses is the primary's enabled harnesses: where a create goes.
func (b ahpBoxes) Harnesses(ctx context.Context) ([]ahphost.Harness, error) {
	harnesses, err := b.ds.Harnesses(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []ahphost.Harness
	for _, harness := range harnesses {
		if harness.State != tui.HarnessDisabled {
			out = append(out, ahphost.Harness{Slug: harness.Slug, Name: harness.Name})
		}
	}
	return out, nil
}

// Create is `discobox new` in the background: detached, and with uncommitted
// work left out, the console's answer when nobody is asked (Enter means no).
func (b ahpBoxes) Create(ctx context.Context, req ahphost.CreateRequest, report func(string)) (ahphost.Box, error) {
	run := tui.RunRequest{Harness: req.Harness, Source: req.Source, IncludeDirty: "false", Detach: true}
	if req.Prompt != "" {
		run.Prompt = []string{req.Prompt}
	}
	box, err := b.ds.Run(ctx, run, report, nil)
	if err != nil {
		return ahphost.Box{}, err
	}
	return ahpBox(box), nil
}

func (b ahpBoxes) Do(ctx context.Context, verb ahphost.Verb, sandboxID string) error {
	return b.ds.Do(ctx, tui.Verb(verb), sandboxID)
}

func (b ahpBoxes) Rename(ctx context.Context, sandboxID, name string) error {
	return b.ds.Rename(ctx, sandboxID, name)
}

func (b ahpBoxes) Execs(ctx context.Context, sandboxID string) ([]ahphost.Exec, error) {
	execs, err := b.ds.Execs(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]ahphost.Exec, 0, len(execs))
	for _, exec := range execs {
		out = append(out, ahpExec(exec))
	}
	return out, nil
}

func (b ahpBoxes) OpenExec(ctx context.Context, sandboxID, execID string, cols, rows int) (ahphost.Stream, error) {
	return b.ds.OpenExec(ctx, sandboxID, execID, cols, rows)
}

func (b ahpBoxes) NewShell(ctx context.Context, sandboxID string, cols, rows int) (ahphost.Exec, ahphost.Stream, error) {
	exec, term, err := b.ds.NewShell(ctx, sandboxID, cols, rows)
	return ahpExec(exec), term, err
}

func (b ahpBoxes) NewTerminal(ctx context.Context, sandboxID string, cols, rows int) (ahphost.Exec, ahphost.Stream, error) {
	exec, term, err := b.ds.NewTerminal(ctx, sandboxID, cols, rows)
	return ahpExec(exec), term, err
}

func (b ahpBoxes) EndExec(ctx context.Context, sandboxID, execID string) error {
	return b.ds.EndExec(ctx, sandboxID, execID)
}

func (b ahpBoxes) SendInput(ctx context.Context, sandboxID, execID, text string) error {
	return b.ds.SendInput(ctx, sandboxID, execID, text)
}

func (b ahpBoxes) PutImage(ctx context.Context, sandboxID, name string, data []byte) (string, error) {
	return b.ds.PutImage(ctx, sandboxID, name, data)
}
