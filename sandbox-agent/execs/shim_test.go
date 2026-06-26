package execs

import (
	"bufio"
	"context"
	"io"
	"net/http"
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
