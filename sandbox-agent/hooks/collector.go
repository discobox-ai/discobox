package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/store"
)

const (
	TerminalIDEnv = "DISCOBOX_TERMINAL_ID"
	SocketEnv     = "DISCOBOX_HOOK_SOCKET"
	socketDirMode = 0o711
	socketMode    = 0o666
	replyOK       = "ok"
)

type Recorder interface {
	RecordHarnessHook(context.Context, store.HarnessHookRecord) (store.HarnessHookRecord, error)
}

type Message struct {
	TerminalID string          `json:"terminalId,omitempty"`
	Provider   string          `json:"provider"`
	Event      string          `json:"event"`
	Payload    json.RawMessage `json:"payload"`
}

func SocketPath(runtimeDir string) string {
	if strings.TrimSpace(runtimeDir) == "" {
		runtimeDir = "/run/discobox/harness-terminals"
	}
	// path, not filepath: this names a location inside the sandbox, which is
	// POSIX (Linux or macOS), and the sandbox-agent is built for the host that
	// runs the tests, Windows included, as well as the guest that runs the
	// binary.
	return path.Join(path.Dir(path.Clean(runtimeDir)), "harness-hooks", "hooks.sock")
}

func Serve(ctx context.Context, socketPath string, recorder Recorder) error {
	if recorder == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	if strings.TrimSpace(socketPath) == "" {
		return errors.New("hook socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), socketDirMode); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(socketPath), socketDirMode); err != nil {
		return err
	}
	_ = os.Remove(socketPath)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen hook socket: %w", err)
	}
	if err := os.Chmod(socketPath, socketMode); err != nil {
		listener.Close()
		return fmt.Errorf("set hook socket permissions: %w", err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)
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
		go handleConn(ctx, conn, recorder)
	}
}

func Publish(ctx context.Context, socketPath string, message Message) error {
	if strings.TrimSpace(socketPath) == "" {
		return fmt.Errorf("%s is required", SocketEnv)
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("connect hook socket: %w", err)
	}
	defer conn.Close()
	if len(message.Payload) == 0 {
		message.Payload = json.RawMessage(`{}`)
	}
	if err := json.NewEncoder(conn).Encode(message); err != nil {
		return fmt.Errorf("write hook message: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// The reply is one line, read up to its newline and never to EOF: on
	// Windows a read pending on a Unix socket while its peer closes it can
	// stall instead of returning EOF (go.dev/issue/49352), which timed this
	// out with the reply already sent (#475).
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read hook reply: %w", err)
	}
	if reply = strings.TrimSpace(reply); reply != replyOK {
		return fmt.Errorf("publish hook: %s", reply)
	}
	return nil
}

// writeReply answers a publisher with one line, the protocol's whole reply, so a
// newline inside an error cannot end it early.
func writeReply(conn net.Conn, text string) {
	_, _ = io.WriteString(conn, strings.ReplaceAll(text, "\n", " ")+"\n")
}

func handleConn(ctx context.Context, conn net.Conn, recorder Recorder) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !scanner.Scan() {
		writeReply(conn, "empty hook message")
		return
	}
	var message Message
	if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
		writeReply(conn, "invalid hook message: "+err.Error())
		return
	}
	payload := message.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	// The hook is recorded under the name its harness used. Its canonical name
	// is the store's to derive, on the way in, and is not passed from here:
	// one owner, so no writer can store a row whose two names disagree
	// (ADR 0146 §4).
	if _, err := recorder.RecordHarnessHook(ctx, store.HarnessHookRecord{
		TerminalID: message.TerminalID,
		Provider:   message.Provider,
		Event:      message.Event,
		Payload:    payload,
	}); err != nil {
		writeReply(conn, err.Error())
		return
	}
	writeReply(conn, replyOK)
}
