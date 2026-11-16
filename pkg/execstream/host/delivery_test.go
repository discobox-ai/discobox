package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/execstream/resume"
)

// Per-attacher delivery: each attacher's frames are written by its own writer
// from a bounded queue, so a viewer that stops reading holds up neither the
// producer nor the other viewers, and is detached once it is persistently
// behind. These tests stand a stalled viewer — a net.Pipe whose client end is
// never read, so every write to it blocks — next to healthy ones.

// serve attaches conn in the background and reports what Attach returned.
func serve(ctx context.Context, s *Stream, conn *pipeConn, opts AttachOptions) <-chan error {
	ended := make(chan error, 1)
	go func() { ended <- s.Attach(ctx, conn, opts) }()
	return ended
}

func waitForAttachers(t *testing.T, s *Stream, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(s.Attachers()) != n {
		if time.Now().After(deadline) {
			t.Fatalf("%d attachers, want %d", len(s.Attachers()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitEnd(t *testing.T, ended <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-ended:
		return err
	case <-time.After(within):
		t.Fatalf("Attach still serving after %s", within)
		return nil
	}
}

// produce broadcasts n chunks of size bytes, numbered from from, from a
// goroutine, and fails the test if the producer is held up for longer than
// within.
func produce(t *testing.T, s *Stream, from, n, size int, within time.Duration) []string {
	t.Helper()
	want := make([]string, n)
	for i := range n {
		want[i] = fmt.Sprintf("%0*d", size, from+i)
	}
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for _, chunk := range want {
			s.Broadcast(frame.Stdout, []byte(chunk))
		}
	}()
	select {
	case <-produced:
	case <-time.After(within):
		t.Fatalf("Broadcast held up for over %s: the producer cannot drain the process's output", within)
	}
	return want
}

// On a terminal stream, a stalled viewer that falls more than its budget
// behind is detached for it, and only it: the producer never waits, the
// healthy viewer receives every chunk in order, and the stalled viewer's queue
// is released.
func TestReplayableViewerOverBudgetIsDetachedAlone(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done, Replay: &fakeReplayer{}, MaxPendingBytes: 2048})

	stalled, _ := newPipe(t)
	healthy, healthyClient := newPipe(t)
	stalledEnded := serve(context.Background(), s, stalled, AttachOptions{})
	serve(context.Background(), s, healthy, AttachOptions{})
	waitForAttachers(t, s, 2)
	stalledAttacher := attacherFor(t, s, stalled)

	// Paced by the healthy viewer, so it never falls behind itself: each
	// chunk is read before the next is produced, while the stalled viewer's
	// queue only grows.
	for i := range 200 {
		chunk := produce(t, s, i, 1, 100, time.Second)[0]
		got := readFrames(t, healthyClient, 1)
		if string(got[0].Payload) != chunk {
			t.Fatalf("healthy viewer read %q at chunk %d, want %q", got[0].Payload, i, chunk)
		}
	}
	if err := awaitEnd(t, stalledEnded, 2*time.Second); !errors.Is(err, ErrFellBehind) {
		t.Fatalf("stalled Attach returned %v, want %v", err, ErrFellBehind)
	}
	waitForAttachers(t, s, 1)
	stalledAttacher.mu.Lock()
	pending, queued := stalledAttacher.pending, len(stalledAttacher.data)
	stalledAttacher.mu.Unlock()
	if pending != 0 || queued != 0 {
		t.Fatalf("detached viewer still holds %d bytes in %d frames", pending, queued)
	}
}

func attacherFor(t *testing.T, s *Stream, conn *pipeConn) *Attacher {
	t.Helper()
	for _, a := range s.Attachers() {
		if a.conn == conn {
			return a
		}
	}
	t.Fatal("no attacher for that connection")
	return nil
}

// On a pipe stream a slow viewer is the command's only reader of its output,
// and nothing could repaint what it missed, so a full queue holds the
// producer rather than dropping its bytes: a viewer that reads slowly but
// steadily receives every one of them, far more than its budget, in order.
func TestSlowPipeViewerReceivesEverythingThroughItsBudget(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done, MaxPendingBytes: 512})

	conn, client := newPipe(t)
	ended := serve(context.Background(), s, conn, AttachOptions{})
	waitForAttachers(t, s, 1)

	const chunks = 100
	got := make(chan []string, 1)
	go func() {
		var out []string
		for len(out) < chunks {
			f, err := frame.Read(client)
			if err != nil {
				break
			}
			out = append(out, string(f.Payload))
			time.Sleep(time.Millisecond)
		}
		got <- out
	}()
	want := produce(t, s, 0, chunks, 100, 5*time.Second)
	select {
	case read := <-got:
		if !slices.Equal(read, want) {
			t.Fatalf("slow viewer read %d chunks, want all %d in order", len(read), chunks)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow viewer did not receive its output")
	}
	select {
	case err := <-ended:
		t.Fatalf("slow but reading viewer was detached: %v", err)
	default:
	}
}

// A pipe stream's viewer that stops reading — a `discobox exec | less` left on
// one page — holds the producer once its queue is full, for as long as it
// stays stopped, far past the write timeout, and is never detached: when it
// reads again it gets every byte, in order.
func TestPausedPipeViewerHoldsTheProducerAndLosesNothing(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done, MaxPendingBytes: 512, WriteTimeout: 20 * time.Millisecond})

	conn, client := newPipe(t)
	ended := serve(context.Background(), s, conn, AttachOptions{})
	waitForAttachers(t, s, 1)

	const chunks = 50
	want := make([]string, chunks)
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := range chunks {
			want[i] = fmt.Sprintf("%0100d", i)
			s.Broadcast(frame.Stdout, []byte(want[i]))
		}
	}()
	time.Sleep(300 * time.Millisecond) // many write timeouts
	select {
	case <-produced:
		t.Fatal("producer ran past a full queue: the paused viewer's output went somewhere it cannot read")
	case err := <-ended:
		t.Fatalf("paused pipe viewer was detached: %v", err)
	default:
	}

	got := framePayloads(readFrames(t, client, chunks))
	<-produced
	if !slices.Equal(got, want) {
		t.Fatalf("viewer read %d chunks, want all %d in order", len(got), chunks)
	}
}

// When the process exits, a pipe viewer that is behind still gets the rest of
// its output and then the exit frame, however long it takes to read them:
// closing an attach puts what is queued on the wire, with no deadline.
func TestSlowPipeViewerGetsItsTailAndExitAfterTheProcessExits(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done, MaxPendingBytes: 2048, WriteTimeout: 20 * time.Millisecond})

	conn, client := newPipe(t)
	ended := serve(context.Background(), s, conn, AttachOptions{})
	waitForAttachers(t, s, 1)
	want := produce(t, s, 0, 15, 100, time.Second) // within the budget, all unread

	exit, err := frame.EncodeExit("exited", ptr[int64](0), "")
	if err != nil {
		t.Fatal(err)
	}
	// What the shim does once the process has exited and its output drained.
	go func() {
		for _, a := range s.Attachers() {
			_ = a.WriteFrame(frame.Exit, exit)
			a.Close()
		}
	}()
	time.Sleep(200 * time.Millisecond) // many write timeouts

	got := readFrames(t, client, len(want)+1)
	if !slices.Equal(framePayloads(got[:len(want)]), want) || got[len(want)].Type != frame.Exit {
		t.Fatalf("viewer read %d frames, want its %d chunks and then the exit frame", len(got), len(want))
	}
	if err := awaitEnd(t, ended, 2*time.Second); err != nil {
		t.Fatalf("Attach returned %v, want nil", err)
	}
}

// A protocol error ends an attach behind the output it was already owed, not
// ahead of it: a client stops reading at an Error frame, so an Error that
// overtook queued output would lose it.
func TestErrorFrameFollowsQueuedOutput(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done})

	conn, client := newPipe(t)
	serve(context.Background(), s, conn, AttachOptions{})
	waitForAttachers(t, s, 1)
	// The first chunk is being written and the second is queued behind it,
	// both unread, when the client breaks the protocol.
	want := produce(t, s, 0, 2, 10, time.Second)
	if err := frame.Write(client, frame.Ack, nil); err != nil { // a host-only frame
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let readFrames queue its Error

	got := readFrames(t, client, 3)
	if !slices.Equal(framePayloads(got[:2]), want) || got[2].Type != frame.Error {
		t.Fatalf("client read %+v, want both chunks and then the Error", got)
	}
}

// The process exiting while a viewer is stalled: the healthy viewer gets its
// output and then the exit frame, as the shim sends it, and the stalled one's
// attach ends within the write timeout instead of holding a writer forever.
func TestExitReachesHealthyViewersWhileOneIsStalled(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done, Replay: &fakeReplayer{}, WriteTimeout: 200 * time.Millisecond})

	stalled, _ := newPipe(t)
	healthy, healthyClient := newPipe(t)
	stalledEnded := serve(context.Background(), s, stalled, AttachOptions{})
	healthyEnded := serve(context.Background(), s, healthy, AttachOptions{})
	waitForAttachers(t, s, 2)

	healthyFrames := collect(healthyClient, 3)
	produce(t, s, 0, 2, 10, 2*time.Second)
	exit, err := frame.EncodeExit("exited", ptr[int64](0), "")
	if err != nil {
		t.Fatal(err)
	}
	// What the shim does once the process has exited and its output drained.
	s.MarkExited(exit)
	for _, a := range s.Attachers() {
		_ = a.WriteFrame(frame.Exit, exit)
		a.Close()
	}

	got := await(t, healthyFrames)
	if len(got) != 3 || got[2].Type != frame.Exit {
		t.Fatalf("healthy viewer read %+v, want its output then the exit frame", got)
	}
	if err := awaitEnd(t, healthyEnded, 2*time.Second); err != nil {
		t.Fatalf("healthy Attach returned %v, want nil", err)
	}
	if _, err := frame.Read(healthyClient); !errors.Is(err, io.EOF) {
		t.Fatalf("healthy client read %v after the exit frame, want EOF: the attach closes its connection", err)
	}
	if err := awaitEnd(t, stalledEnded, 2*time.Second); !errors.Is(err, ErrStalled) {
		t.Fatalf("stalled Attach returned %v, want %v", err, ErrStalled)
	}
}

// A client joining with a replay, over a resumable session, while another
// viewer is stalled: its session is acknowledged, its screen arrives ahead of
// the live output, and its input is acknowledged — none of it waits on the
// stalled viewer, whose writer is blocked the whole time.
func TestJoinReplayAndAcksProceedWhileAViewerIsStalled(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{snapshot: []byte("HISTORY")}
	s := New(Options{Done: done, Replay: replay, OnFrame: func(frame.Frame) error { return nil }})

	stalled, _ := newPipe(t)
	serve(context.Background(), s, stalled, AttachOptions{})
	waitForAttachers(t, s, 1)
	produce(t, s, 0, 1, 10, time.Second) // the stalled writer is now blocked

	joining, client := newPipe(t)
	serve(context.Background(), s, joining, AttachOptions{Replay: true})
	waitForAttachers(t, s, 2)
	produce(t, s, 1, 1, 10, time.Second) // live, behind the snapshot

	session, err := resume.EncodeNewSession(bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.Write(client, frame.Session, session); err != nil {
		t.Fatal(err)
	}
	established := readFrames(t, client, 1)
	if established[0].Type != frame.SessionOK {
		t.Fatalf("first frame type %d, want SessionOK ahead of the held replay", established[0].Type)
	}
	if err := frame.Write(client, frame.Ready, nil); err != nil {
		t.Fatal(err)
	}
	got := readFrames(t, client, 2)
	if want := []string{"HISTORY", "0000000001"}; !slices.Equal(framePayloads(got), want) {
		t.Fatalf("joining viewer read %q, want %q", framePayloads(got), want)
	}

	action, err := resume.EncodeAction(1, frame.Input, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := frame.Write(client, frame.Action, action); err != nil {
		t.Fatal(err)
	}
	ack := readFrames(t, client, 1)
	if ack[0].Type != frame.Ack {
		t.Fatalf("frame type %d, want Ack", ack[0].Type)
	}
}

// Canceling an attach whose writer is blocked on a viewer that stopped reading
// ends it at once: closing the connection is what returns the blocked write,
// and Attach does not return until its writer has.
func TestCancelReleasesABlockedWriter(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	s := New(Options{Done: done}) // the default write timeout: far longer than this test

	stalled, _ := newPipe(t)
	ctx, cancel := context.WithCancel(context.Background())
	ended := serve(ctx, s, stalled, AttachOptions{})
	waitForAttachers(t, s, 1)
	stalledAttacher := attacherFor(t, s, stalled)
	produce(t, s, 0, 1, 10, time.Second)

	cancel()
	if err := awaitEnd(t, ended, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Attach returned %v, want %v", err, context.Canceled)
	}
	select {
	case <-stalledAttacher.writerDone:
	default:
		t.Fatal("Attach returned with its writer still running")
	}
	if err := stalled.conn.SetDeadline(time.Time{}); !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("connection still open after Attach returned (SetDeadline: %v)", err)
	}
	waitForAttachers(t, s, 0)
}

// A terminal viewer detached for falling behind comes back the way any
// reconnecting terminal does, with a replay: the screen, then live output from
// exactly where it stands, with nothing the screen already holds repeated.
func TestDetachedViewerRecoversByReplay(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{snapshot: []byte("SCREEN")}
	s := New(Options{Done: done, Replay: replay, MaxPendingBytes: 1024})

	first, _ := newPipe(t)
	ended := serve(context.Background(), s, first, AttachOptions{})
	waitForAttachers(t, s, 1)
	produce(t, s, 0, 50, 100, 2*time.Second)
	if err := awaitEnd(t, ended, 2*time.Second); !errors.Is(err, ErrFellBehind) {
		t.Fatalf("Attach returned %v, want %v", err, ErrFellBehind)
	}

	again, client := newPipe(t)
	serve(context.Background(), s, again, AttachOptions{Replay: true})
	waitForAttachers(t, s, 1)
	frames := collect(client, 2)
	if err := frame.Write(client, frame.Ready, nil); err != nil {
		t.Fatal(err)
	}
	s.Broadcast(frame.Stdout, []byte("after"))
	if got, want := framePayloads(await(t, frames)), []string{"SCREEN", "after"}; !slices.Equal(got, want) {
		t.Fatalf("reconnected viewer read %q, want %q", got, want)
	}
	select {
	case extra := <-collect(client, 1):
		t.Fatalf("reconnected viewer also read %q, want nothing more", framePayloads(extra))
	case <-time.After(100 * time.Millisecond):
	}
}

// A viewer reconnecting with a repaint larger than its whole budget, while
// output keeps arriving behind it, takes the repaint and the output: the
// snapshot is not counted, so a viewer that fell behind once is not detached
// again by its own repaint.
func TestLargeRepaintDoesNotSpendTheBudget(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{snapshot: bytes.Repeat([]byte("S"), 8192)}
	s := New(Options{Done: done, Replay: replay, MaxPendingBytes: 1024})

	conn, client := newPipe(t)
	ended := serve(context.Background(), s, conn, AttachOptions{Replay: true})
	waitForAttachers(t, s, 1)
	want := produce(t, s, 0, 5, 100, time.Second) // queued behind the snapshot while joining

	if err := frame.Write(client, frame.Ready, nil); err != nil {
		t.Fatal(err)
	}
	got := readFrames(t, client, 6)
	if len(got[0].Payload) != 8192 || !slices.Equal(framePayloads(got[1:]), want) {
		t.Fatalf("viewer read %d frames, want the repaint and then the %d chunks behind it", len(got), len(want))
	}
	select {
	case err := <-ended:
		t.Fatalf("reconnected viewer was detached: %v", err)
	default:
	}
}

// Only one repaint at a time rides outside the budget. A viewer that keeps
// asking for the screen without reading what it was sent pays for every one
// after the first, and is detached once they do not fit, rather than queueing
// a screen per request.
func TestRepeatedRepaintsCountAgainstTheBudget(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	replay := &fakeReplayer{snapshot: bytes.Repeat([]byte("S"), 8192)}
	s := New(Options{Done: done, Replay: replay, MaxPendingBytes: 1024})

	conn, client := newPipe(t)
	ended := serve(context.Background(), s, conn, AttachOptions{})
	waitForAttachers(t, s, 1)
	for range 3 {
		if err := frame.Write(client, frame.Repaint, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := awaitEnd(t, ended, 2*time.Second); !errors.Is(err, ErrFellBehind) {
		t.Fatalf("Attach returned %v, want %v", err, ErrFellBehind)
	}
}
