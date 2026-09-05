package execs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/sandbox-agent/shimproxy"
)

// Two clients, one terminal: the PTY is the size whichever of them asked last,
// so the other is reading a screen laid out for a window it does not have. This
// is what Ctrl-L in a pane does about it — re-send my size, then ask for the
// screen — and it crosses the whole of the real thing: the Unix HTTP upgrade,
// host.Stream, the screen emulator, the PTY, and a program answering SIGWINCH.
//
// The program records the size it is told about on every SIGWINCH, so the file
// it writes is the record of which client's window it was laying out for.
func TestRunShimRepaintPutsTheAskingClientsSizeBack(t *testing.T) {
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	sizePath := filepath.Join(dir, "sizes")
	socketPath := filepath.Join(dir, "shim.sock")
	shimErr := make(chan error, 1)
	go func() {
		shimErr <- RunShim(ctx, ShimConfig{
			ExecID: "exec_repaint",
			Command: []string{
				"sh", "-c",
				`trap 'stty size >> ` + sizePath + `' WINCH; printf hello; while :; do sleep 0.05; done`,
			},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
			Rows:        30,
			Cols:        90,
			TTY:         true,
		})
	}()

	asking := attachShimReader(ctx, t, socketPath)
	other := attachShimReader(ctx, t, socketPath)
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	asking.wait(t, "hello")
	other.wait(t, "hello")

	// Each client's size is waited for in turn, because two clients writing to
	// two connections have no order of their own — and which of them asked last
	// is the whole premise. The other one does, so 100x40 is what the program is
	// laying out for and this client's window is the wrong one.
	writeResize(t, asking, 80, 24)
	waitForSize(t, sizePath, "24 80")
	writeResize(t, other, 100, 40)
	waitForSize(t, sizePath, "40 100")

	// From here, everything the asking client receives is the answer to what it
	// asks next.
	asking.reset()
	other.reset()

	writeResize(t, asking, 80, 24)
	if err := asking.conn.WriteFrame(frame.Repaint, nil); err != nil {
		t.Fatalf("write repaint: %v", err)
	}

	// The program is laying out for the asking client's window again. The last
	// line is what counts: the redraw nudge behind the repaint shrinks the pty a
	// row and restores it, so the program is told twice and settles on the size
	// that was asked for.
	waitForSize(t, sizePath, "24 80")
	// And the screen came back with it, rather than the client waiting for the
	// program to print something next.
	asking.wait(t, "hello")
	// The snapshot went to the client that asked and to nobody else. That is
	// the only part of a repaint that is private, and this program is written
	// so that it is the only part under test: it answers SIGWINCH by writing to
	// a file and never prints again, where a program that repainted on SIGWINCH
	// would broadcast that redraw — laid out for the asking client's size — to
	// this one too.
	if got := other.text(); strings.Contains(got, "hello") {
		t.Fatalf("the other client received %q, want the snapshot to reach only the client that asked", got)
	}

	cancel()
	<-shimErr
}

// shimReader is one attached client, drained continuously: the shim writes to
// every attacher, so a client that is not reading holds up the process.
type shimReader struct {
	conn *resumableShimConn
	mu   sync.Mutex
	out  []byte
}

func attachShimReader(ctx context.Context, t *testing.T, socketPath string) *shimReader {
	t.Helper()
	conn, err := dialResumableShim(ctx, socketPath)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// The attach asked for a replay, which is held until the client says it is
	// reading. It is, below.
	if err := conn.WriteFrame(frame.Ready, nil); err != nil {
		t.Fatalf("write ready: %v", err)
	}
	r := &shimReader{conn: conn}
	go func() {
		for {
			next, err := conn.ReadFrame()
			if err != nil {
				return
			}
			if next.Type != frame.Stdout {
				continue
			}
			r.mu.Lock()
			r.out = append(r.out, next.Payload...)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *shimReader) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.out)
}

func (r *shimReader) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out = nil
}

func (r *shimReader) wait(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if strings.Contains(r.text(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("client received %q, want it to contain %q", r.text(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeResize(t *testing.T, r *shimReader, cols, rows uint16) {
	t.Helper()
	payload, err := frame.EncodeResize(cols, rows)
	if err != nil {
		t.Fatalf("encode resize: %v", err)
	}
	if err := r.conn.WriteFrame(frame.Resize, payload); err != nil {
		t.Fatalf("write resize: %v", err)
	}
}

// waitForSize waits for want to be the last size the program was told about.
func waitForSize(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			if lines := strings.Fields(strings.TrimSpace(string(data))); len(lines) >= 2 {
				last = strings.Join(lines[len(lines)-2:], " ")
			}
			if last == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the program was last told it is %q, want %q (whole record: %q)", last, want, readFileString(path))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readFileString(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
