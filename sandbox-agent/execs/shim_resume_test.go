package execs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream"
	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/execstream/resume"
	"github.com/discobox-ai/discobox/sandbox-agent/shimproxy"
)

type resumableShimConn struct {
	net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
}

func (c *resumableShimConn) ReadFrame() (frame.Frame, error) {
	return frame.Read(c.reader)
}

func (c *resumableShimConn) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return frame.Write(c.Conn, typ, payload)
}

func dialResumableShim(ctx context.Context, socketPath string) (*resumableShimConn, error) {
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		return nil, err
	}
	reqURL := &url.URL{Scheme: "http", Host: "unix", Path: "/attach", RawQuery: "replay=true"}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, fmt.Errorf("attach status = %s", resp.Status)
	}
	return &resumableShimConn{Conn: conn, reader: reader}, nil
}

// This is the process-level reconnect test. It crosses the shim's real Unix
// HTTP upgrade, host.Stream, PTY, and process stdin/stdout rather than replacing
// the application callback with a fake.
func TestRunShimResumesTerminalInputAcrossPhysicalReconnect(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	shimErr := make(chan error, 1)
	go func() {
		shimErr <- RunShim(ctx, ShimConfig{
			ExecID: "exec_resume",
			Command: []string{
				"sh",
				"-c",
				`while IFS= read -r line; do printf '<%s>\n' "$line"; [ "$line" = quit ] && exit 0; done`,
			},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
			Rows:        24,
			Cols:        80,
			TTY:         true,
		})
	}()

	allowReplacement := make(chan struct{})
	physical := make(chan *resumableShimConn, 2)
	var dialCount atomic.Int32
	dial := func(ctx context.Context) (execstream.Conn, error) {
		if dialCount.Add(1) > 1 {
			select {
			case <-allowReplacement:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		conn, err := dialResumableShim(ctx, socketPath)
		if err != nil {
			return nil, err
		}
		physical <- conn
		return conn, nil
	}
	initial, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan resume.Event, 4)
	resumed, err := resume.New(ctx, initial, resume.Options{
		Dial:  dial,
		Event: func(event resume.Event) { events <- event },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()

	resize, err := frame.EncodeResize(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.WriteFrame(frame.Resize, resize); err != nil {
		t.Fatal(err)
	}
	if err := resumed.WriteFrame(frame.Ready, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	if err := resumed.WriteFrame(frame.Input, []byte("before\n")); err != nil {
		t.Fatal(err)
	}
	before := readResumedOutputUntil(t, resumed, []byte("<before>"))
	if count := bytes.Count(before, []byte("<before>")); count != 1 {
		t.Fatalf("pre-reconnect response count = %d, output %q", count, before)
	}

	firstPhysical := <-physical
	if err := firstPhysical.Close(); err != nil {
		t.Fatal(err)
	}
	if err := resumed.WriteFrame(frame.Input, []byte("during\n")); err != nil {
		t.Fatalf("write during disconnect: %v", err)
	}
	select {
	case event := <-events:
		if event.State != resume.ConnectionReconnecting {
			t.Fatalf("first event = %q, want reconnecting", event.State)
		}
	case <-ctx.Done():
		t.Fatal("resume client did not detect the closed shim connection")
	}
	close(allowReplacement)

	during := readResumedOutputUntil(t, resumed, []byte("<during>"))
	if count := bytes.Count(during, []byte("<during>")); count != 1 {
		t.Fatalf("replayed response count = %d, output %q", count, during)
	}
	select {
	case event := <-events:
		if event.State != resume.ConnectionReconnected {
			t.Fatalf("second event = %q, want reconnected", event.State)
		}
	case <-ctx.Done():
		t.Fatal("resume client did not reconnect to the shim")
	}

	if err := resumed.WriteFrame(frame.Input, []byte("after\n")); err != nil {
		t.Fatal(err)
	}
	after := readResumedOutputUntil(t, resumed, []byte("<after>"))
	if count := bytes.Count(after, []byte("<after>")); count != 1 {
		t.Fatalf("post-reconnect response count = %d, output %q", count, after)
	}

	if err := resumed.WriteFrame(frame.Input, []byte("quit\n")); err != nil {
		t.Fatal(err)
	}
	readResumedExit(t, resumed)
	_ = resumed.Close()
	cancel()
	if err := <-shimErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

func readResumedOutputUntil(t *testing.T, conn execstream.Conn, needle []byte) []byte {
	t.Helper()
	var output []byte
	for !bytes.Contains(output, needle) {
		next, err := conn.ReadFrame()
		if err != nil {
			t.Fatalf("read output before %q: %v", needle, err)
		}
		switch next.Type {
		case frame.Stdout:
			output = append(output, next.Payload...)
		case frame.Exit:
			t.Fatalf("process exited before output %q; got %q", needle, output)
		default:
			t.Fatalf("unexpected frame type %d before output %q", next.Type, needle)
		}
	}
	return output
}

func readResumedExit(t *testing.T, conn execstream.Conn) {
	t.Helper()
	for {
		next, err := conn.ReadFrame()
		if err != nil {
			t.Fatalf("read exit frame: %v", err)
		}
		if next.Type == frame.Exit {
			exit, err := frame.DecodeExit(next.Payload)
			if err != nil {
				t.Fatalf("decode exit: %v", err)
			}
			if exit.ExitCode == nil || *exit.ExitCode != 0 {
				t.Fatalf("exit = %#v, want code 0", exit)
			}
			return
		}
	}
}
