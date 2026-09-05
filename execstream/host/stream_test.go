package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/execstream/resume"
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
	s := New(Options{Done: done, OnFrame: func(f frame.Frame) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, f.Type)
		return nil
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

func TestReadFramesDeduplicatesResumedActions(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var mu sync.Mutex
	var input []byte
	s := New(Options{Done: done, OnFrame: func(next frame.Frame) error {
		if next.Type == frame.Input {
			mu.Lock()
			input = append(input, next.Payload...)
			mu.Unlock()
		}
		return nil
	}})

	token := bytes.Repeat([]byte{0x55}, 32)
	sessionPayload, err := resume.EncodeSession(token, 1)
	if err != nil {
		t.Fatal(err)
	}
	actionPayload, err := resume.EncodeAction(1, frame.Input, []byte("once"))
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		conn, client := newPipe(t)
		go func() { _ = s.Attach(context.Background(), conn, AttachOptions{}) }()
		if err := frame.Write(client, frame.Session, sessionPayload); err != nil {
			t.Fatal(err)
		}
		established, err := frame.Read(client)
		if err != nil {
			t.Fatal(err)
		}
		position, err := resume.DecodePosition(established.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if position != uint64(attempt) {
			t.Fatalf("attempt %d host position = %d, want %d", attempt, position, attempt)
		}
		if err := frame.Write(client, frame.Action, actionPayload); err != nil {
			t.Fatal(err)
		}
		ack, err := frame.Read(client)
		if err != nil {
			t.Fatal(err)
		}
		position, err = resume.DecodePosition(ack.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if position != 1 {
			t.Fatalf("attempt %d ack = %d, want 1", attempt, position)
		}
		_ = client.Close()
	}
	mu.Lock()
	got := string(input)
	mu.Unlock()
	if got != "once" {
		t.Fatalf("applied input = %q, want exactly one copy", got)
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

// A repaint asked for mid-stream is the replay half of an attach, at a later
// moment: the client that asked gets the snapshot, ahead of whatever is
// broadcast next, and the program is nudged into confirming it.
//
// Every other attacher gets nothing. What they are showing arrived because they
// asked for it, and a screen redrawn under somebody who pressed nothing is a
// screen that flickers for no reason they can see.
func TestRepaintReachesOnlyTheClientThatAskedForIt(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{snapshot: []byte("HISTORY")}
	s := New(Options{Done: done, Replay: replay})

	asking, askingClient := newPipe(t)
	quiet, quietClient := newPipe(t)
	go func() { _ = s.Attach(context.Background(), asking, AttachOptions{}) }()
	go func() { _ = s.Attach(context.Background(), quiet, AttachOptions{}) }()
	for len(s.Attachers()) < 2 {
		time.Sleep(time.Millisecond)
	}

	// Two frames on the asking side — the repaint, then the live output behind
	// it — and one on the other, which sees only the live output.
	askingFrames := collect(askingClient, 2)
	quietFrames := collect(quietClient, 1)

	if err := frame.Write(askingClient, frame.Repaint, nil); err != nil {
		t.Fatalf("write repaint: %v", err)
	}
	// The nudge trails the snapshot reaching the wire, so waiting for it is
	// what makes the broadcast below land after the repaint rather than racing
	// it.
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
	s.Broadcast(frame.Stdout, []byte("live"))

	got := await(t, askingFrames)
	if len(got) != 2 || string(got[0].Payload) != "HISTORY" || string(got[1].Payload) != "live" {
		t.Fatalf("asking client read %q, want the snapshot then the live output", framePayloads(got))
	}
	other := await(t, quietFrames)
	if len(other) != 1 || string(other[0].Payload) != "live" {
		t.Fatalf("other client read %q, want only the live output", framePayloads(other))
	}
}

// A stream with no screen — a pipe exec — has nothing to repaint from. The
// frame is ignored rather than answered with an empty screen or an error, the
// same answer an attach that asks for a replay gets.
func TestRepaintWithoutAReplayerIsIgnored(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var mu sync.Mutex
	var seen []byte
	s := New(Options{Done: done, OnFrame: func(f frame.Frame) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, f.Type)
		return nil
	}})
	conn, client := newPipe(t)
	go func() { _ = s.Attach(context.Background(), conn, AttachOptions{}) }()
	for !s.HasAttachers() {
		time.Sleep(time.Millisecond)
	}

	if err := frame.Write(client, frame.Repaint, nil); err != nil {
		t.Fatalf("write repaint: %v", err)
	}
	// The input behind it is what proves the repaint was consumed rather than
	// routed to the process: it arrives, and nothing else does.
	if err := frame.Write(client, frame.Input, []byte("x")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := append([]byte(nil), seen...)
	mu.Unlock()
	if !bytes.Equal(got, []byte{frame.Input}) {
		t.Fatalf("routed frames = %v, want the input only", got)
	}
}

func framePayloads(frames []frame.Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, string(f.Payload))
	}
	return out
}

// The fence a snapshot leaves behind: a chunk the Replayer had already absorbed
// when the snapshot was taken is not delivered behind it.
//
// The interleave it stands in for has no seam to pause it — Broadcast counts a
// chunk under the stream lock and delivers it after releasing that lock, so a
// repaint can take its snapshot in between — and what closes that window is the
// count, not the write order. A client that received the chunk twice would show
// the second copy under the repaint it asked for.
func TestReplayFenceDropsWhatTheSnapshotAlreadyHolds(t *testing.T) {
	conn, client := newPipe(t)
	attach := &Attacher{conn: conn, done: make(chan struct{}), ready: make(chan struct{})}

	// A snapshot taken when the Replayer had absorbed seven chunks.
	attach.beginReplay(7)
	if err := attach.flushBuffer(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Drained continuously, and broadcast from a goroutine: a chunk that should
	// have been dropped has to have somewhere to go, or this fails as a
	// deadlock instead of as the extra frame it is.
	received := make(chan string, 8)
	go func() {
		for {
			next, err := frame.Read(client)
			if err != nil {
				return
			}
			received <- string(next.Payload)
		}
	}()
	go func() {
		for _, chunk := range []struct {
			text string
			at   uint64
		}{
			{"seventh", 7}, // in the snapshot: already on the client's screen
			{"sixth", 6},   // older still, and in it too
			{"eighth", 8},  // counted after it: the client has not seen this
			{"stderr", 0},  // no snapshot can hold it, so it is never dropped
		} {
			_ = attach.broadcast(frame.Stdout, []byte(chunk.text), chunk.at)
		}
	}()

	var got []string
	for len(got) < 2 {
		select {
		case text := <-received:
			got = append(got, text)
		case <-time.After(3 * time.Second):
			t.Fatalf("client read %q, want the two chunks the snapshot does not hold", got)
		}
	}
	if want := []string{"eighth", "stderr"}; !slices.Equal(got, want) {
		t.Fatalf("client read %q, want %q — everything in the snapshot dropped", got, want)
	}
	select {
	case extra := <-received:
		t.Fatalf("client also read %q, want nothing the snapshot already holds", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// A repaint asked for before this attacher's own attach replay has happened
// does nothing, and the attach's replay is untouched by it.
//
// Both would paint a screen, and the attach's is the older of the two: served
// second it leaves the client looking at a screen from before it pressed
// anything. The window is the whole of waitForReady, which a resume reconnect
// passes through — and reconnecting is exactly when somebody reaches for
// Ctrl-L.
func TestRepaintWhileTheAttachReplayIsStillOwedDoesNothing(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	// Numbered, so a second snapshot cannot be mistaken for the first: the
	// attach holds snapshot 1 from registration, and a repaint served in this
	// window would take and send snapshot 2 ahead of it.
	replay := &countingReplayer{}
	s := New(Options{Done: done, Replay: replay})
	conn, client := newPipe(t)

	go func() { _ = s.Attach(context.Background(), conn, AttachOptions{Replay: true}) }()
	for !s.HasAttachers() {
		time.Sleep(time.Millisecond)
	}

	// Drained from the start: a frame written in the window below must have
	// somewhere to land, or its absence proves nothing.
	received := make(chan string, 8)
	go func() {
		for {
			next, err := frame.Read(client)
			if err != nil {
				return
			}
			received <- string(next.Payload)
		}
	}()

	// The replay is still owed: no frame.Ready has been sent.
	if err := frame.Write(client, frame.Repaint, nil); err != nil {
		t.Fatalf("write repaint: %v", err)
	}
	select {
	case early := <-received:
		t.Fatalf("client read %q before its own attach replay, want the repaint to stand aside", early)
	case <-time.After(200 * time.Millisecond):
	}
	replay.mu.Lock()
	after := replay.after
	replay.mu.Unlock()
	if after != 0 {
		t.Fatalf("AfterReplay ran %d times before the attach replay, want none", after)
	}

	// The attach's own replay then happens, exactly once and with the snapshot
	// it registered with.
	if err := frame.Write(client, frame.Ready, nil); err != nil {
		t.Fatalf("write ready: %v", err)
	}
	select {
	case got := <-received:
		if got != "SNAPSHOT1" {
			t.Fatalf("client read %q, want the snapshot the attach registered with", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the attach replay")
	}
	select {
	case extra := <-received:
		t.Fatalf("client also read %q, want one screen and not two", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// countingReplayer numbers its snapshots, so a test can tell which call
// produced the screen that reached the wire.
type countingReplayer struct {
	mu    sync.Mutex
	taken int
	after int
}

func (r *countingReplayer) Observe([]byte) {}

func (r *countingReplayer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taken++
	return fmt.Appendf(nil, "SNAPSHOT%d", r.taken)
}

func (r *countingReplayer) AfterReplay() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.after++
}

// A repaint with no screen to send sets no fence, and still asks for the
// redraw.
//
// The Replayer fails open: an emulator dropped after a panic answers Snapshot
// with nothing, and plain live streaming carries on. A fence recorded for a
// screen that was never written would turn that into live output silently
// discarded — dropped as "already painted" by a repaint that painted nothing.
// The program's own redraw is then the whole of the repaint, which is what
// failing open means here.
func TestRepaintWithNoScreenSetsNoFence(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{} // no snapshot: the screen has been dropped
	s := New(Options{Done: done, Replay: replay})
	conn, _ := newPipe(t)

	attach := &Attacher{conn: conn, done: make(chan struct{}), ready: make(chan struct{})}
	s.mu.Lock()
	s.attachers[attach] = struct{}{}
	s.observed = 5
	s.mu.Unlock()

	s.repaint(attach)

	attach.mu.Lock()
	fence := attach.replayedThrough
	buffering := attach.buffering
	attach.mu.Unlock()
	if fence != 0 {
		t.Fatalf("fence moved to %d for a screen that was never sent, want it left alone", fence)
	}
	if buffering {
		t.Fatal("attacher left buffering with no snapshot coming to flush it behind")
	}
	replay.mu.Lock()
	after := replay.after
	replay.mu.Unlock()
	if after != 1 {
		t.Fatalf("AfterReplay ran %d times, want the program nudged into redrawing anyway", after)
	}
}
