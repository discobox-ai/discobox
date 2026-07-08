package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/shimproxy"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
	"github.com/obot-platform/discobox/sandbox-agent/terminal/frame"
)

func TestRunRecordsExitStatus(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			TerminalID:  "agt_test",
			AgentID:     "test",
			Command:     []string{"sh", "-c", "trap '' HUP; exit 7"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Rows:        24,
			Cols:        80,
		})
	}()

	if _, err := shimproxy.StartJSON[terminal.Terminal](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run shim: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	var status terminal.Terminal
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatalf("parse runtime: %v", err)
	}
	if status.Status != terminal.StatusFailed {
		t.Fatalf("status = %q", status.Status)
	}
	if status.ExitCode == nil || *status.ExitCode == 0 {
		if status.ExitCode == nil {
			t.Fatalf("exit code was not recorded")
		}
		t.Fatalf("exit code = %d", *status.ExitCode)
	}
	if status.ExitedAt == nil {
		t.Fatalf("exitedAt was not recorded")
	}
}

func TestRunSendsOutputBeforeExitFrame(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			TerminalID:  "agt_test",
			AgentID:     "test",
			Command:     []string{"sh", "-c", "printf hi; exit 7"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Rows:        24,
			Cols:        80,
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
	req.Header.Set("Upgrade", "discobox-agent-terminal")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}

	if _, err := shimproxy.StartJSON[terminal.Terminal](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	var output []byte
	var exit frame.ExitPayload
	for {
		next, err := frame.Read(reader)
		if err != nil {
			if err == io.EOF {
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
	if err := <-errCh; err != nil {
		t.Fatalf("run shim: %v", err)
	}
}

func TestReplayStreamsHistoryThenLive(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			TerminalID:  "agt_test",
			AgentID:     "test",
			Command:     []string{"sh", "-c", "printf AAA; sleep 1; printf BBB"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			LogDir:      filepath.Join(dir, "logs"),
			Rows:        24,
			Cols:        80,
		})
	}()

	// A plain attacher lets us observe when the historical "AAA" has been
	// produced, so the replay attach below starts strictly after it and strictly
	// before the live "BBB".
	plainConn, plainReader := attachTerminal(t, ctx, socketPath, false)
	defer plainConn.Close()
	if _, err := shimproxy.StartJSON[terminal.Terminal](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	if got := readOutputBytes(t, plainReader, 3); got != "AAA" {
		t.Fatalf("plain output = %q, want AAA", got)
	}

	// Attach with replay while the command sleeps: "AAA" must be replayed from
	// disk and "BBB" must arrive live, with no gap and no duplication.
	replayConn, replayReader := attachTerminal(t, ctx, socketPath, true)
	defer replayConn.Close()

	output := readOutputUntilExit(t, replayReader)
	if output != "AAABBB" {
		t.Fatalf("replay output = %q, want AAABBB", output)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run shim: %v", err)
	}
}

func attachTerminal(t *testing.T, ctx context.Context, socketPath string, replay bool) (io.ReadWriteCloser, *bufio.Reader) {
	t.Helper()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	attachURL := "http://unix/attach"
	if replay {
		attachURL += "?replay=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, attachURL, nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-agent-terminal")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}
	if replay {
		// Signal readiness so the shim starts streaming history immediately
		// instead of falling back on the ready timeout.
		if err := frame.Write(conn, frame.Ready, nil); err != nil {
			t.Fatalf("write ready frame: %v", err)
		}
	}
	return conn, reader
}

func readOutputBytes(t *testing.T, reader *bufio.Reader, n int) string {
	t.Helper()
	var out []byte
	for len(out) < n {
		next, err := frame.Read(reader)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if next.Type == frame.Output {
			out = append(out, next.Payload...)
		}
	}
	return string(out)
}

func readOutputUntilExit(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var out []byte
	for {
		next, err := frame.Read(reader)
		if err != nil {
			if err == io.EOF {
				t.Fatal("attach closed before exit frame")
			}
			t.Fatalf("read frame: %v", err)
		}
		switch next.Type {
		case frame.Output:
			out = append(out, next.Payload...)
		case frame.Exit:
			return string(out)
		}
	}
}

func TestReplayStreamsLargeHistoryFromStart(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	// Emit a large, deterministic blob (many PTY reads / many log entries), then
	// hold the terminal open so a replay attach lands after the whole blob is
	// recorded. seq 1..2000 prints ~10 KB, well past a single bufio buffer.
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			TerminalID:  "agt_test",
			AgentID:     "test",
			Command:     []string{"sh", "-c", "seq 1 2000; sleep 5"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			LogDir:      filepath.Join(dir, "logs"),
			Rows:        24,
			Cols:        80,
		})
	}()

	// Observe the whole blob on a plain attacher so the replay below starts
	// strictly after every byte has been produced and recorded.
	plainConn, plainReader := attachTerminal(t, ctx, socketPath, false)
	defer plainConn.Close()
	if _, err := shimproxy.StartJSON[terminal.Terminal](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	want := expectedSeqOutput(2000)
	live := readOutputBytes(t, plainReader, len(want))
	if live != want {
		t.Fatalf("plain output mismatch: got %d bytes, want %d", len(live), len(want))
	}

	replayConn, replayReader := attachTerminal(t, ctx, socketPath, true)
	defer replayConn.Close()
	// The replay must reproduce the recorded history from the very first byte.
	got := readOutputBytes(t, replayReader, len(want))
	if got != want {
		t.Fatalf("replay did not stream history from the start:\n got[:80]=%q\nwant[:80]=%q\n(len got=%d want=%d)",
			truncate(got, 80), truncate(want, 80), len(got), len(want))
	}
}

func expectedSeqOutput(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\r\n", i) // the PTY translates \n to \r\n
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func TestRunUsesResizeFrameBeforeStart(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sizePath := filepath.Join(dir, "size.txt")
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			TerminalID:  "agt_test",
			AgentID:     "test",
			Command:     []string{"sh", "-c", "sleep 0.2; stty size > " + sizePath},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Rows:        24,
			Cols:        80,
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
	req.Header.Set("Upgrade", "discobox-agent-terminal")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
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
	if _, err := shimproxy.StartJSON[terminal.Terminal](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("run shim: %v", err)
	}
	data, err := os.ReadFile(sizePath)
	if err != nil {
		t.Fatalf("read size: %v", err)
	}
	if got := string(data); got != "33 101\n" {
		t.Fatalf("size = %q, want %q", got, "33 101\n")
	}
}
