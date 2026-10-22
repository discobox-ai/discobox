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

	"github.com/discobox-ai/discobox/cli/internal/ahphost"
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
settings.`,
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
		Boxes:   ds,
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
