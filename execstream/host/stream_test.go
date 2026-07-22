package host

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/obot-platform/discobox/execstream/frame"
)

// pipeConn is an execstream.Conn over an io pipe, which is all the transport
// this package needs to be exercised.
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

func newPipe(t *testing.T) (*pipeConn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	return &pipeConn{conn: server}, client
}

// collect starts reading the client end immediately and reports the first n
// frames. It must be started before anything is broadcast: net.Pipe has no
// buffering, and an attacher write blocks until the client reads it.
func collect(client net.Conn, n int) <-chan []frame.Frame {
	ch := make(chan []frame.Frame, 1)
	go func() {
		out := make([]frame.Frame, 0, n)
		for len(out) < n {
			f, err := frame.Read(client)
			if err != nil {
				break
			}
			out = append(out, f)
		}
		ch <- out
	}()
	return ch
}

func await(t *testing.T, ch <-chan []frame.Frame) []frame.Frame {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for frames")
		return nil
	}
}

// readFrames is the start-then-wait pair for tests that broadcast nothing until
// after the attach is serving.
func readFrames(t *testing.T, client net.Conn, n int) []frame.Frame {
	t.Helper()
	return await(t, collect(client, n))
}

// The window this closes: a client that sees the announcement may start the
// process at once, so anything broadcast between the announcement and
// registration would be lost. Ready runs after registration by construction, so
// output produced from inside it still reaches this attacher.
func TestAttachRegistersBeforeReady(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done})
	conn, client := newPipe(t)

	go func() {
		_ = s.Attach(context.Background(), conn, AttachOptions{
			Ready: func() error {
				// Stand in for a process that starts the instant the client is
				// told the stream is up.
				s.Broadcast(frame.Stdout, []byte("first"))
				return nil
			},
		})
	}()

	got := readFrames(t, client, 1)
	if len(got) != 1 || got[0].Type != frame.Stdout || string(got[0].Payload) != "first" {
		t.Fatalf("frames = %+v, want the output broadcast during Ready", got)
	}
}

// A Ready failure is the transport refusing the attach; it must surface rather
// than leaving a registered attacher behind.
func TestAttachReadyErrorLeavesNoAttacher(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done})
	conn, _ := newPipe(t)

	wantErr := context.Canceled
	if err := s.Attach(context.Background(), conn, AttachOptions{
		Ready: func() error { return wantErr },
	}); !errors.Is(err, wantErr) {
		t.Fatalf("Attach err = %v, want %v", err, wantErr)
	}
	if s.HasAttachers() {
		t.Fatal("attacher still registered after a failed Ready")
	}
}

type fakeReplayer struct {
	mu       sync.Mutex
	observed []byte
	snapshot []byte
	after    int
}

func (r *fakeReplayer) Observe(payload []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, payload...)
}
func (r *fakeReplayer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot
}
func (r *fakeReplayer) AfterReplay() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.after++
}

// The snapshot must reach the wire before the live frames buffered behind it,
// so the client sees history then live output, contiguous and in order.
func TestAttachReplaySnapshotPrecedesBufferedLiveOutput(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{snapshot: []byte("HISTORY")}
	s := New(Options{Done: done, Replay: replay})
	conn, client := newPipe(t)

	go func() {
		_ = s.Attach(context.Background(), conn, AttachOptions{
			Replay: true,
			Ready: func() error {
				// Broadcast while the attacher is registered but still buffering.
				s.Broadcast(frame.Stdout, []byte("live"))
				return nil
			},
		})
	}()
	// Unblock the replay wait the way a real client does.
	if err := frame.Write(client, frame.Ready, nil); err != nil {
		t.Fatalf("write ready: %v", err)
	}

	got := readFrames(t, client, 2)
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	if string(got[0].Payload) != "HISTORY" {
		t.Fatalf("first frame = %q, want the snapshot", got[0].Payload)
	}
	if string(got[1].Payload) != "live" {
		t.Fatalf("second frame = %q, want the buffered live output", got[1].Payload)
	}
	// AfterReplay runs once the flush has reached the wire, so it trails the
	// frames the client just read.
	deadline := time.Now().Add(2 * time.Second)
	for {
		replay.mu.Lock()
		after := replay.after
		replay.mu.Unlock()
		if after == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("AfterReplay called %d times, want 1", after)
		}
		time.Sleep(time.Millisecond)
	}
}

// Only stdout is replayable state. A stderr chunk must reach attachers without
// being folded into the screen a repaint would reproduce.
func TestBroadcastFeedsReplayerStdoutOnly(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{}
	s := New(Options{Done: done, Replay: replay})
	conn, client := newPipe(t)
	go func() { _ = s.Attach(context.Background(), conn, AttachOptions{}) }()
	// Start reading first: an attacher write blocks until the client reads, so
	// broadcasting into an unread pipe would stall the broadcaster.
	frames := collect(client, 2)
	for !s.HasAttachers() {
		time.Sleep(time.Millisecond)
	}

	s.Broadcast(frame.Stdout, []byte("out"))
	s.Broadcast(frame.Stderr, []byte("err"))

	got := await(t, frames)
	if len(got) != 2 || got[0].Type != frame.Stdout || got[1].Type != frame.Stderr {
		t.Fatalf("frames = %+v, want stdout then stderr", got)
	}
	replay.mu.Lock()
	observed := string(replay.observed)
	replay.mu.Unlock()
	if observed != "out" {
		t.Fatalf("replayer observed %q, want stdout only", observed)
	}
}

// A client that arrives after the process exited still gets the exit frame,
// rather than a bare disconnect it cannot distinguish from a crash.
func TestAttachAfterExitDeliversRetainedExitFrame(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done})
	payload, err := frame.EncodeExit("exited", ptr(int64(7)), "")
	if err != nil {
		t.Fatalf("encode exit: %v", err)
	}
	s.MarkExited(payload)

	conn, client := newPipe(t)
	go func() { _ = s.Attach(context.Background(), conn, AttachOptions{}) }()

	got := readFrames(t, client, 1)
	if len(got) != 1 || got[0].Type != frame.Exit {
		t.Fatalf("frames = %+v, want an exit frame", got)
	}
	exit, err := frame.DecodeExit(got[0].Payload)
	if err != nil {
		t.Fatalf("decode exit: %v", err)
	}
	if exit.ExitCode == nil || *exit.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", exit.ExitCode)
	}
}

// Client control frames reach the process; Ready is consumed here and must not.
func TestReadFramesRoutesControlFramesAndConsumesReady(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var mu sync.Mutex
	var seen []byte
	s := New(Options{Done: done, OnFrame: func(_ *Attacher, f frame.Frame) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, f.Type)
	}})
	conn, client := newPipe(t)
	go func() { _ = s.Attach(context.Background(), conn, AttachOptions{}) }()
	for !s.HasAttachers() {
		time.Sleep(time.Millisecond)
	}

	for _, typ := range []byte{frame.Ready, frame.Input, frame.Signal} {
		if err := frame.Write(client, typ, []byte("x")); err != nil {
			t.Fatalf("write frame %d: %v", typ, err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := append([]byte(nil), seen...)
	mu.Unlock()
	if !bytes.Equal(got, []byte{frame.Input, frame.Signal}) {
		t.Fatalf("routed frames = %v, want input and signal only (ready is consumed)", got)
	}
}

// The size a client sent before the process started must survive to launch time.
func TestResizeIsRetainedForLaunch(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done})
	if _, ok := s.PendingResize(); ok {
		t.Fatal("a fresh stream must have no pending resize")
	}

	waited := make(chan struct{})
	go func() { s.WaitForResize(context.Background()); close(waited) }()
	s.ApplyResize(frame.ResizePayload{Cols: 101, Rows: 33})

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForResize did not return after a resize")
	}
	got, ok := s.PendingResize()
	if !ok || got.Cols != 101 || got.Rows != 33 {
		t.Fatalf("pending resize = %+v, %v, want 101x33", got, ok)
	}
}

func ptr[T any](v T) *T { return &v }
