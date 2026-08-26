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
	// always Linux, and the sandbox-agent is built for the host that runs the
	// tests as well as the guest that runs the binary.
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
	reply, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("read hook reply: %w", err)
	}
	if strings.TrimSpace(string(reply)) != "ok" {
		return fmt.Errorf("publish hook: %s", strings.TrimSpace(string(reply)))
	}
	return nil
}

func handleConn(ctx context.Context, conn net.Conn, recorder Recorder) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !scanner.Scan() {
		_, _ = io.WriteString(conn, "empty hook message")
		return
	}
	var message Message
	if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
		_, _ = io.WriteString(conn, "invalid hook message: "+err.Error())
		return
	}
	payload := message.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if _, err := recorder.RecordHarnessHook(ctx, store.HarnessHookRecord{
		TerminalID: message.TerminalID,
		Provider:   message.Provider,
		Event:      message.Event,
		Payload:    payload,
	}); err != nil {
		_, _ = io.WriteString(conn, err.Error())
		return
	}
	_, _ = io.WriteString(conn, "ok")
}
