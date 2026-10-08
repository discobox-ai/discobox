package sourceconverge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The credential helper is how a source's origin token reaches git without
// being written into the checkout: each origin is configured with
// `credential.<originUrl>.helper` naming this binary's git-credential mode,
// which asks the agent over a local socket for the token of the origin git is
// fetching from and answers git with it as a bearer credential. The agent's
// own clone and a user's later `git fetch origin` go through the same helper,
// and a new document's token is the next fetch's without touching the
// repository.
//
// The socket answers anyone in the sandbox. A token fetches this sandbox's own
// origins and nothing else, which anyone in the sandbox can already do with
// the checkout's remote; the socket adds no reach.

// SocketPath is the credential socket beside the agent's runtime directory.
func SocketPath(runtimeDir string) string {
	if strings.TrimSpace(runtimeDir) == "" {
		runtimeDir = "/run/discobox/harness-terminals"
	}
	// filepath: the socket is on this agent's own filesystem, and the agent
	// runs inside the sandbox, so the host's rules are the sandbox's.
	return filepath.Join(filepath.Dir(filepath.Clean(runtimeDir)), "git-credential", "credential.sock")
}

// HelperCommand is the credential.helper value that runs executable's
// git-credential mode against socketPath. The leading "!" has git run it
// through the shell, appending the operation, which is what lets both paths be
// quoted; without it git reads a quoted path as a helper name.
func HelperCommand(executable, socketPath string) string {
	return "!" + shellQuote(executable) + " git-credential --socket " + shellQuote(socketPath)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// credentialRequest is what the helper asks the agent: the request git
// described, by scheme, host (with any port) and path.
type credentialRequest struct {
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Path     string `json:"path"`
}

type credentialResponse struct {
	Token string `json:"token,omitempty"`
}

const (
	socketDirMode = 0o755
	// socketMode lets every account in the sandbox connect; see above.
	socketMode = 0o666
)

// ServeCredentials answers the helper with c's tokens until ctx ends.
func (c *Converger) ServeCredentials(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), socketDirMode); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(socketPath), socketDirMode); err != nil {
		return err
	}
	_ = os.Remove(socketPath)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen git credential socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)
	if err := os.Chmod(socketPath, socketMode); err != nil {
		return fmt.Errorf("set git credential socket permissions: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go c.answerCredential(conn)
	}
}

func (c *Converger) answerCredential(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	var request credentialRequest
	if err := json.NewDecoder(io.LimitReader(conn, 64*1024)).Decode(&request); err != nil {
		return
	}
	// A helper that hung up gets nothing either way, and git goes on as if
	// this helper had no answer.
	if err := json.NewEncoder(conn).Encode(credentialResponse{Token: c.Token(request.Protocol, request.Host, request.Path)}); err != nil {
		return
	}
}

// RunHelper is the git-credential mode: git's credential helper protocol on
// stdin and stdout, answered from the agent at the socket args name. It answers
// only `get`, and only a git that can take a bearer credential (the authtype
// capability, git 2.46 and later); anything else is answered with nothing, so
// git goes on as if the helper had none.
func RunHelper(args []string, stdin io.Reader, stdout io.Writer) error {
	var socketPath, operation string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--socket" && i+1 < len(args):
			socketPath = args[i+1]
			i++
		case operation == "":
			operation = args[i]
		}
	}
	if socketPath == "" {
		return errors.New("git-credential: --socket is required")
	}
	if operation != "get" {
		// store and erase: the token is the agent's to keep, not git's.
		_, _ = io.Copy(io.Discard, stdin)
		return nil
	}
	request, bearer, err := readCredentialRequest(stdin)
	if err != nil {
		return err
	}
	if !bearer {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("git-credential: connect to the sandbox agent: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}
	var response credentialResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return fmt.Errorf("git-credential: read the sandbox agent's answer: %w", err)
	}
	if response.Token == "" {
		return nil
	}
	// ephemeral: no other helper is to store it; the agent has the current
	// one, and a stored copy would outlive it.
	_, err = fmt.Fprintf(stdout, "capability[]=authtype\nauthtype=Bearer\ncredential=%s\nephemeral=1\n", response.Token)
	return err
}

// readCredentialRequest reads git's description of the request, and whether
// git can take a bearer credential at all.
func readCredentialRequest(stdin io.Reader) (credentialRequest, bool, error) {
	var request credentialRequest
	var bearer bool
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "protocol":
			request.Protocol = value
		case "host":
			request.Host = value
		case "path":
			request.Path = value
		case "capability[]":
			if value == "authtype" {
				bearer = true
			}
		}
	}
	return request, bearer, scanner.Err()
}
