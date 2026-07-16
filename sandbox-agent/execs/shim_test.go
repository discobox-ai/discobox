package execs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/shimproxy"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"
)

func TestRunShimSendsOutputBeforeExitFrame(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_test",
			Command:     []string{"sh", "-c", "printf hi; exit 7"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			LogDir:      filepath.Join(dir, "logs"),
		})
	}()

	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	defer conn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}

	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	var output []byte
	var exit frame.ExitPayload
	for {
		next, err := frame.Read(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("attach closed before exit frame")
			}
			t.Fatalf("read frame: %v", err)
		}
		switch next.Type {
		case frame.Output:
			output = append(output, next.Payload...)
		case frame.Exit:
			var decodeErr error
			exit, decodeErr = frame.DecodeExit(next.Payload)
			if decodeErr != nil {
				t.Fatalf("decode exit: %v", decodeErr)
			}
			goto done
		default:
			t.Fatalf("unexpected frame type %d", next.Type)
		}
	}

done:
	if string(output) != "hi" {
		t.Fatalf("output = %q, want hi", string(output))
	}
	if exit.ExitCode == nil || *exit.ExitCode != 7 {
		if exit.ExitCode == nil {
			t.Fatalf("exit code was not sent")
		}
		t.Fatalf("exit code = %d, want 7", *exit.ExitCode)
	}
	// Logs are flushed by the time the exit frame arrives, so read them before
	// tearing down.
	logs, err := ReadLogs(ctx, filepath.Join(dir, "logs"), "exec_test")
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	var logged []byte
	for _, entry := range logs {
		if entry.Stream == LogStreamStdout {
			logged = append(logged, entry.Data...)
		}
	}
	if string(logged) != "hi" {
		t.Fatalf("logged output = %q, want hi", string(logged))
	}
	// The shim lingers after exit so a late attacher can still replay + read the
	// exit code; cancel to end the linger and shut it down.
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

func TestRunShimUsesResizeFrameBeforeStart(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sizePath := filepath.Join(dir, "size.txt")
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_test",
			Command:     []string{"sh", "-c", "sleep 0.2; stty size > " + sizePath},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			LogDir:      filepath.Join(dir, "logs"),
			Rows:        24,
			Cols:        80,
			TTY:         true,
		})
	}()

	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	defer conn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}

	payload, err := frame.EncodeResize(101, 33)
	if err != nil {
		t.Fatalf("encode resize: %v", err)
	}
	if err := frame.Write(conn, frame.Resize, payload); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	// The child writes the size file and exits; wait for that (the shim lingers
	// after exit), then cancel to end the linger.
	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		var readErr error
		if data, readErr = os.ReadFile(sizePath); readErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("size file not written: %v", readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
	if got := string(data); got != "33 101\n" {
		t.Fatalf("size = %q, want %q", got, "33 101\n")
	}
}

// attachReadExit opens an attach to the shim, optionally starts the process, and
// reads frames until the exit frame, returning it.
func attachReadExit(ctx context.Context, t *testing.T, socketPath string, start bool) frame.ExitPayload {
	t.Helper()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	_ = resp.Body.Close()
	if start {
		if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
			t.Fatalf("start shim: %v", err)
		}
	}
	for {
		next, err := frame.Read(reader)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if next.Type == frame.Exit {
			exit, err := frame.DecodeExit(next.Payload)
			if err != nil {
				t.Fatalf("decode exit: %v", err)
			}
			return exit
		}
	}
}

// A client that attaches after the process has already exited still receives the
// exit frame (and code) rather than a bare disconnect, because the shim lingers.
func TestRunShimLingersForLateAttach(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_late",
			Command:     []string{"sh", "-c", "printf bye; exit 3"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			LogDir:      filepath.Join(dir, "logs"),
		})
	}()

	// A current attacher observes the live exit (proving the process has ended).
	if exit := attachReadExit(ctx, t, socketPath, true); exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("first attach exit = %v, want 3", exit.ExitCode)
	}
	// A second attacher connects after the exit; the lingering shim still delivers
	// the exit frame + code.
	if exit := attachReadExit(ctx, t, socketPath, false); exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("late attach exit = %v, want 3", exit.ExitCode)
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}
