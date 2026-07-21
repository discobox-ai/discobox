package shimruntime

import (
	"bytes"
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/obot-platform/discobox/execstream/frame"
	"github.com/obot-platform/discobox/execstream/host"
)

// screenPipe returns a pipe standing in for the PTY master: EnableScreen
// writes emulator query answers to the returned write end, and the test reads
// them from the read end.
func screenPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

func readWithTimeout(f *os.File, d time.Duration) []byte {
	ch := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 256)
		n, err := f.Read(buf)
		if err != nil {
			ch <- nil
			return
		}
		ch <- buf[:n]
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(d):
		return nil
	}
}

// pipeConn is an execstream.Conn over an io pipe: enough transport to attach a
// client to the runtime without HTTP or a socket.
type pipeConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (c *pipeConn) ReadFrame() (frame.Frame, error) { return frame.Read(c.conn) }
func (c *pipeConn) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return frame.Write(c.conn, typ, payload)
}
func (c *pipeConn) Close() error { return c.conn.Close() }

// attachPipe attaches a client and drains whatever the runtime sends it,
// returning a func reporting the bytes received so far. Draining matters: an
// attacher write blocks until the client reads.
func attachPipe(t *testing.T, r *Runtime) func() []byte {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	go func() { _ = r.stream.Attach(context.Background(), &pipeConn{conn: server}, host.AttachOptions{}) }()

	var mu sync.Mutex
	var got []byte
	go func() {
		for {
			f, err := frame.Read(client)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, f.Payload...)
			mu.Unlock()
		}
	}()
	for !r.HasAttachers() {
		time.Sleep(time.Millisecond)
	}
	return func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), got...)
	}
}

// A program's terminal query (here DA1, which Claude Code blocks on at
// startup) makes the screen emulator write a response to its internal
// unbuffered pipe. Without the response pump this blocks Broadcast while it
// holds the runtime mutex and deadlocks the entire shim.
func TestBroadcastTerminalQueryDoesNotDeadlock(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	r := New("test", done, nil)
	input, tty := screenPipe(t)
	r.EnableScreen(24, 80, DefaultScrollbackLines, tty)

	finished := make(chan struct{})
	go func() {
		r.Broadcast(frame.Stdout, []byte("hello\x1b[>0q\x1b[c"))
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Broadcast deadlocked on a terminal query")
	}

	// With no client attached the emulator's DA1 answer must be delivered to
	// the PTY input, like a real terminal would, so a headless TUI waiting on
	// the response can start.
	got := readWithTimeout(input, 5*time.Second)
	if !bytes.Contains(got, []byte("\x1b[?")) || !bytes.Contains(got, []byte("c")) {
		t.Fatalf("DA1 response never reached the PTY input, got %q", got)
	}
}

// While a client is attached its real terminal sees the query in the raw
// output stream and answers it; the emulator's answer must be dropped so the
// program does not receive duplicate responses.
func TestScreenQueryResponsesDroppedWhileAttached(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	r := New("test", done, nil)
	input, tty := screenPipe(t)
	r.EnableScreen(24, 80, DefaultScrollbackLines, tty)

	_ = attachPipe(t, r)

	r.Broadcast(frame.Stdout, []byte("hello\x1b[c"))

	// Delivery is asynchronous; give a misrouted response time to show up.
	if got := readWithTimeout(input, 300*time.Millisecond); got != nil {
		t.Fatalf("query response was written to the PTY while attached: %q", got)
	}
}

// An emulator panic must drop the screen (disabling repaint-on-attach) but
// leave raw live streaming intact, instead of killing the exec.
func TestScreenPanicDropsScreenAndKeepsStreaming(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	r := New("test", done, nil)
	_, tty := screenPipe(t)
	r.EnableScreen(24, 80, DefaultScrollbackLines, tty)

	r.mu.Lock()
	r.runScreenLocked(func(*screenBuffer) { panic("emulator bug") })
	r.mu.Unlock()

	if r.hasScreen() {
		t.Fatal("screen should be dropped after an emulator panic")
	}
	if !r.hasTTY() {
		t.Fatal("tty must survive a dropped screen so redrawAfterReplay still works")
	}

	received := attachPipe(t, r)
	r.Broadcast(frame.Stdout, []byte("still alive"))
	deadline := time.Now().Add(2 * time.Second)
	for !bytes.Contains(received(), []byte("still alive")) {
		if time.Now().After(deadline) {
			t.Fatalf("live streaming broken after screen drop, wire = %q", received())
		}
		time.Sleep(time.Millisecond)
	}
}

// The post-replay jiggle must leave the PTY at its original size.
func TestRedrawAfterReplayRestoresSize(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	done := make(chan struct{})
	defer close(done)
	r := New("test", done, nil)
	r.EnableScreen(40, 120, DefaultScrollbackLines, master)
	if err := pty.Setsize(master, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
		t.Fatal(err)
	}

	r.AfterReplay()

	size, err := pty.GetsizeFull(master)
	if err != nil {
		t.Fatal(err)
	}
	if size.Rows != 40 || size.Cols != 120 {
		t.Fatalf("size not restored after jiggle: got %dx%d", size.Rows, size.Cols)
	}
}
