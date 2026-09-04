package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// sshConnectDialer opens one byte stream to the server's sshd over the
// transport the API already answers on: a `GET /ssh/connect` websocket, whose
// stream the server hands to the same sshd its TCP listener feeds.
//
// It is the one piece both ways of reaching that sshd without a TCP port share
// — the loopback bridge below, and the `ProxyCommand` an emitted ssh_config
// names (ssh_proxy.go) — so neither owns the URL or the client.
type sshConnectDialer struct {
	url    string
	client *http.Client
}

// sshConnectDialer resolves the endpoint once, so a bridge serving many
// connections does not rebuild it per connection.
func (a *App) sshConnectDialer() (sshConnectDialer, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return sshConnectDialer{}, err
	}
	socketURL, err := sshConnectWebSocketURL(baseURL)
	if err != nil {
		return sshConnectDialer{}, err
	}
	return sshConnectDialer{url: socketURL, client: httpClient}, nil
}

// dial returns the websocket as a net.Conn. Closing it closes the websocket.
func (d sshConnectDialer) dial(ctx context.Context) (net.Conn, error) {
	wsConn, resp, err := websocket.Dial(ctx, d.url, &websocket.DialOptions{HTTPClient: d.client})
	if resp != nil && resp.Body != nil {
		// The handshake response body carries nothing once the connection is
		// upgraded, but it is still a body: leaving it open leaks the
		// underlying connection on every session.
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, wsConn, websocket.MessageBinary), nil
}

// spliceSSHConnect pumps bytes both ways until either side finishes or the
// context is canceled. Neither stream is closed here: the caller owns both.
func spliceSSHConnect(ctx context.Context, local io.ReadWriter, remote io.ReadWriter) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// sshBridge is a loopback TCP port that carries SSH to the server over the
// transport the API already answers on.
//
// `ssh` speaks TCP to a host and port and nothing else, and the local endpoint
// is usually a unix socket. endpoint.StartLoopbackProxy cannot serve here: it
// is an HTTP reverse proxy, and these are not HTTP bytes.
//
// The port exists for the life of one command. Nothing about it is written
// down, which is the point: connecting to a sandbox should not require the
// server to hold a machine-wide SSH port open. A persisted ssh_config cannot
// name a port that only exists while a command runs, so what `discobox admin
// ssh-config` writes reaches the same sshd through a `ProxyCommand` instead;
// see ssh_proxy.go.
type sshBridge struct {
	listener net.Listener
	cancel   context.CancelFunc
}

// port is the loopback port ssh is pointed at. The listener is always TCP —
// startSSHBridge asks for "tcp" — but the assertion is checked rather than
// assumed, so a future change to that call fails here instead of panicking
// inside a running session.
func (b *sshBridge) port() int {
	if addr, ok := b.listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

func (b *sshBridge) Close() error {
	b.cancel()
	return b.listener.Close()
}

// startSSHBridge opens the loopback port and serves it until the bridge is
// closed. Errors on individual connections surface to `ssh` as a closed
// connection, which is what it already knows how to report.
func (a *App) startSSHBridge(ctx context.Context) (*sshBridge, error) {
	dialer, err := a.sshConnectDialer()
	if err != nil {
		return nil, err
	}
	// Loopback only. This port is an unauthenticated door onto the SSH server
	// for anything that can reach it, so it must not be reachable off-host;
	// the SSH layer still authenticates, but the blast radius is the point.
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the local SSH bridge: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	bridge := &sshBridge{listener: listener, cancel: cancel}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go bridge.serve(ctx, conn, dialer)
		}
	}()
	return bridge, nil
}

func (b *sshBridge) serve(ctx context.Context, conn net.Conn, dialer sshConnectDialer) {
	defer conn.Close()
	remote, err := dialer.dial(ctx)
	if err != nil {
		return
	}
	defer remote.Close()
	spliceSSHConnect(ctx, conn, remote)
}

// sshConnectWebSocketURL turns the API base URL into the websocket URL for the
// SSH connect route. A unix-socket endpoint keeps its scheme-less host: the
// HTTP client dials the socket regardless of what the URL says, and the host is
// only there to make it a valid URL.
func sshConnectWebSocketURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", baseURL, err)
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	default:
		parsed.Scheme = "ws"
	}
	parsed.Path = "/ssh/connect"
	return parsed.String(), nil
}

// sshBridgeArgs are the ssh(1) arguments that point at the bridge: everything
// about where and as whom to connect, so nothing has to be written to disk.
func sshBridgeArgs(port int, sandboxID, identityFile, knownHostsFile string) []string {
	return append([]string{
		"-p", strconv.Itoa(port),
		"-l", sandboxID,
	}, sshBridgeOptions(identityFile, knownHostsFile)...)
}

// scpBridgeArgs are the same for scp(1), which spells the port -P and names no
// user of its own: `discobox cp` puts the sandbox in each remote path instead,
// so one command can name more than one discobox.
func scpBridgeArgs(port int, identityFile, knownHostsFile string) []string {
	return append([]string{
		"-P", strconv.Itoa(port),
	}, sshBridgeOptions(identityFile, knownHostsFile)...)
}

// sshBridgeOptions are what every OpenSSH client pointed at the bridge needs
// beyond the address: which key to offer, which host key to accept, and that
// the user's own ssh_config has nothing to say about a port this process
// invented. Where the two clients differ — the port flag, and how the sandbox
// is named — stays with the caller.
func sshBridgeOptions(identityFile, knownHostsFile string) []string {
	return []string{
		"-i", identityFile,
		// Without IdentitiesOnly, ssh offers every agent key before this one
		// and can exhaust MaxAuthTries before reaching it.
		"-o", "IdentitiesOnly=yes",
		// The host key is pinned to a file written for this command alone, so
		// verification is real without touching the user's known_hosts — and
		// without the loopback port's reused number ever meaning anything.
		// Spelled for the config parser, not for the shell: ssh reads a -o
		// argument as a config line, so this value is percent-expanded and
		// split on whitespace, and a temp directory under "C:\Users\Ada
		// Lovelace" would arrive as two filenames neither of which exists. The
		// -i above is neither, and is passed as it is.
		"-o", "UserKnownHostsFile=" + sshConfigPath(knownHostsFile),
		"-o", "StrictHostKeyChecking=yes",
		// The bridge is this process; the user's ssh_config has nothing to say
		// about it, and a stray `Host *` block there could otherwise override
		// the identity or user this command just resolved.
		"-F", "none",
	}
}

// sshBridgeHost is the host argument, which must follow every option and
// precede any remote command (`ssh [options] host [command]`).
const sshBridgeHost = "127.0.0.1"

// sshBridgeSession is a live bridge plus the two files an OpenSSH client needs
// to authenticate over it: the enrolled private key, and a known_hosts pinning
// this server's host key against the port this run happens to have.
//
// The three exist together or not at all — a bridge with no pinned host key is
// a connection nothing can verify — so they are opened together and closed
// together, by `tools ssh` and by `cp` alike.
type sshBridgeSession struct {
	bridge     *sshBridge
	identity   string
	knownHosts string
	cleanup    func()
}

// port is the loopback port the client is pointed at.
func (s *sshBridgeSession) port() int { return s.bridge.port() }

func (s *sshBridgeSession) close() {
	_ = s.bridge.Close()
	s.cleanup()
}

// startSSHBridgeSession resolves the identity and host key, opens the bridge,
// and pins the key to it. The identity is enrolled in the project if it is not
// already, which is why this takes a client and a project rather than deriving
// them.
func (a *App) startSSHBridgeSession(cmd *cobra.Command, client *apiclientgen.Client, projectID string) (*sshBridgeSession, error) {
	// `tools ssh` and `cp` are commands the user typed, and a key generated or
	// enrolled for them is theirs to know about, so the notes are printed where
	// the command's own reporting goes.
	identityFile, err := a.resolveSSHIdentity(cmd.Context(), client, projectID, "", printedNotes(cmd.ErrOrStderr()))
	if err != nil {
		return nil, err
	}
	hostKey, err := a.sshHostKey(cmd.Context(), client)
	if err != nil {
		return nil, err
	}
	bridge, err := a.startSSHBridge(cmd.Context())
	if err != nil {
		return nil, err
	}
	knownHosts, cleanup, err := writeTemporaryKnownHosts(bridge.port(), hostKey)
	if err != nil {
		_ = bridge.Close()
		return nil, err
	}
	return &sshBridgeSession{bridge: bridge, identity: identityFile, knownHosts: knownHosts, cleanup: cleanup}, nil
}
