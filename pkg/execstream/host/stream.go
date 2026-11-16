// Package host serves one process's output to any number of attached clients.
//
// It owns the attacher set, the ordering guarantees around joining and leaving
// it, and the retained exit frame. It owns no process, no terminal, and no
// transport: the process lives behind Broadcast and the OnFrame callback, the
// terminal (if any) lives behind Replayer, and the transport is an
// execstream.Conn. That is what makes the ordering rules here testable over
// net.Pipe, with no sockets and no PTY.
//
// Delivery is per attacher. Broadcast never writes to a connection: it queues
// the chunk for each attacher, and each attacher's own writer puts its queue on
// the wire. On a terminal stream (one with a Replayer) a viewer that reads
// slowly or not at all therefore holds up only itself — not the process's
// output readers, and not the other viewers — and is detached once it is
// persistently behind. A pipe stream cannot repaint a viewer it detached, so
// there a viewer's full queue holds the producer instead, losslessly, as a
// pipe does. See Options.MaxPendingBytes and Options.WriteTimeout.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/execstream/resume"
)

// Replayer reproduces what a client that attaches mid-stream should see before
// live output resumes. A TTY stream has one — an emulator tracking the screen —
// and a pipe stream does not, which is the whole of the difference as far as
// this package is concerned.
//
// Observe is called with every stdout chunk while the stream lock is held, in
// broadcast order, and must not block: a Replayer that blocks stalls the
// process's output. Snapshot is called under the same lock when an attacher
// registers, and again whenever one asks for a repaint, so the snapshot and the
// frames buffered after it are exactly contiguous — no chunk is duplicated or
// lost across the join.
//
// AfterReplay runs once the snapshot and the frames buffered behind it have
// reached the wire. It exists for implementations whose real state lives
// somewhere more authoritative than the snapshot: a TTY implementation nudges
// the program into repainting itself, so any imperfection in the snapshot
// converges to what the program actually draws.
type Replayer interface {
	Observe(payload []byte)
	Snapshot() []byte
	AfterReplay()
}

// Options configures a Stream.
type Options struct {
	// Done is closed when the stream is finished with: it detaches every
	// attach still serving, dropping what is queued for it. Its owner closes
	// it once nothing is attached (HasAttachers) or it is shutting down
	// anyway; closing it under an attach still taking its output cuts that
	// output short.
	Done <-chan struct{}
	// Replay, when set, enables repaint-on-attach for clients that ask for it.
	Replay Replayer
	// OnFrame applies the control frames a client sends — input, resize, signal,
	// close-input. Ready and resumable-transport frames are consumed by this
	// package and never reach it.
	OnFrame func(frame.Frame) error
	// MaxPendingBytes bounds the frames queued for one attacher that its
	// connection has not yet taken, including the output held while it joins.
	// What happens when
	// a frame would not fit depends on whether a detached viewer can recover:
	//
	//   - A stream with a Replayer (a terminal) detaches the attacher with
	//     ErrFellBehind. Its client reconnects and is repainted from the
	//     screen, so it loses nothing it can still see, and nobody else waits.
	//   - A stream without one (a pipe exec) cannot repaint, and its viewer is
	//     usually the command's only consumer, so dropping its bytes would
	//     truncate the output. The frame waits for room instead, holding up
	//     the producer for as long as the viewer takes, with no deadline: the
	//     back-pressure of an ordinary pipe. A second viewer of the same pipe
	//     exec waits with it.
	//
	// The snapshot a replay queues is not counted: the attach is owed it
	// whatever its size, and a viewer that reconnected after falling behind
	// must be able to take its repaint and the output behind it. Only one
	// snapshot at a time is exempt; a repaint asked for while one is still
	// queued counts like output.
	//
	// Zero means DefaultMaxPendingBytes.
	MaxPendingBytes int
	// WriteTimeout bounds, on a stream with a Replayer, how long one frame
	// write to an attacher may go without completing. An attacher over it is
	// detached with ErrStalled and its connection closed, which releases the
	// blocked write; its client reconnects and is repainted. A pipe stream's
	// writes have no deadline, for the reason MaxPendingBytes gives.
	// Zero means DefaultWriteTimeout.
	WriteTimeout time.Duration
}

const (
	// DefaultMaxPendingBytes is Options.MaxPendingBytes when unset: a few
	// full screens of a repaint's scrollback and well over a hundred of the
	// shim's 32 KiB output reads.
	DefaultMaxPendingBytes = 4 << 20
	// DefaultWriteTimeout is Options.WriteTimeout when unset. It is the
	// deadline for a viewer that has stopped reading, not a latency budget: a
	// healthy client over a slow link completes a frame write far sooner.
	DefaultWriteTimeout = 30 * time.Second
)

var (
	// ErrFellBehind is why an attacher of a replayable stream was detached
	// when its queue filled. Attach returns it, wrapped with the byte count.
	ErrFellBehind = errors.New("viewer fell too far behind the output")
	// ErrStalled is why an attacher of a replayable stream was detached when
	// a write to it stopped making progress. Attach returns it, wrapped.
	ErrStalled = errors.New("viewer stopped reading")
	// errEnded refuses a frame for an attacher that is closing or detached.
	errEnded = errors.New("attach ended")
	// errStreamDone ends the attaches still serving once their owner is
	// finished with the stream (Options.Done).
	errStreamDone = errors.New("stream done")
)

// Stream is the fan-out for one process.
type Stream struct {
	done         <-chan struct{}
	onFrame      func(frame.Frame) error
	resumes      *resume.Server
	maxPending   int
	writeTimeout time.Duration

	mu        sync.Mutex
	attachers map[*Attacher]struct{}
	// exitPayload holds the encoded exit frame once the process has exited, so a
	// client attaching afterwards still receives it rather than a bare
	// disconnect. nil while the process runs.
	exitPayload []byte
	// replay backs repaint-on-attach; nil until SetReplayer, and nil forever
	// for a stream with no terminal.
	replay Replayer
	// observed counts the chunks the Replayer has absorbed. It is the fence
	// between a snapshot and the frames behind it: a snapshot taken at count N
	// contains every chunk up to N, so an attacher told to replay from N drops
	// those rather than painting them a second time underneath it. Only
	// meaningful with a Replayer, and only stdout is counted, because only
	// stdout is replayable state.
	observed uint64
	// resize is the most recent size a client asked for, retained so a process
	// that has not started yet can be launched at the right size.
	resize      *frame.ResizePayload
	resizeReady chan struct{}
	resizeOnce  sync.Once
}

// SetReplayer installs repaint-on-attach after construction. A stream whose
// process turns out to have a terminal enables it once the PTY exists; a stream
// without one never calls this, and its clients never wait on a repaint
// handshake that would not be answered.
func (s *Stream) SetReplayer(r Replayer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replay = r
}

func (s *Stream) replayer() Replayer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replay
}

func New(opts Options) *Stream {
	s := &Stream{
		done:         opts.Done,
		replay:       opts.Replay,
		onFrame:      opts.OnFrame,
		resumes:      resume.NewServer(),
		maxPending:   opts.MaxPendingBytes,
		writeTimeout: opts.WriteTimeout,
		attachers:    map[*Attacher]struct{}{},
		resizeReady:  make(chan struct{}),
	}
	if s.maxPending <= 0 {
		s.maxPending = DefaultMaxPendingBytes
	}
	if s.writeTimeout <= 0 {
		s.writeTimeout = DefaultWriteTimeout
	}
	return s
}

// AttachOptions configures one attach.
type AttachOptions struct {
	// Replay asks for the current state to be repainted before live output. It
	// is honored only when the Stream has a Replayer.
	Replay bool
	// Ready is called once this attacher has joined the broadcast set and is
	// holding its output, and before any frame reaches conn. A transport that must
	// announce itself — writing an HTTP 101, say — does it here.
	//
	// This ordering is the point of the callback. A client that sees the
	// announcement may start the process immediately, so an attacher that joined
	// after announcing would miss everything written in between; for a fast
	// command that is its entire output. Registration happens first and cannot
	// be reordered by a caller.
	Ready func() error
}

// Attach joins conn to the broadcast set and serves it until the process exits,
// the client disconnects, or ctx is canceled.
//
// Attach owns conn while it serves and closes it before returning, which is
// what releases this attach's reader and a writer blocked on a viewer that
// stopped reading. It returns once the writer has stopped. An attach that is
// closed (Attacher.Close) first writes what is queued for it, however long its
// client takes, unless it is detached meanwhile or Options.Done closes. The
// error is nil for an attach that ended normally — the frames queued for it
// were written — and otherwise says why it did not: Ready's error, ctx's, the
// process being gone, or the reason it was detached (ErrFellBehind,
// ErrStalled, or the write that failed).
func (s *Stream) Attach(ctx context.Context, conn execstream.Conn, opts AttachOptions) error {
	attach := s.newAttacher(conn)

	// Register before announcing: see AttachOptions.Ready. A joining attacher
	// queues live frames but does not write them, so they cannot race the
	// announcement onto the wire.
	replay := s.replayer()
	wantReplay := opts.Replay && replay != nil
	s.addAttacher(attach, wantReplay)
	defer s.removeAttacher(attach)
	go attach.write()

	if opts.Ready != nil {
		if err := opts.Ready(); err != nil {
			attach.detach(err)
			_ = attach.finish(nil)
			return err
		}
	}
	go s.readFrames(attach)

	if wantReplay {
		// Hold the repaint until the client says it is attached and reading, so
		// nothing is written into the announcement window where an intermediate
		// proxy hop may drop buffered bytes. A timeout still repaints, best
		// effort, for a client that never says so.
		s.waitForReady(attach)
	}
	// Live streaming begins here for every attach: everything queued since
	// registration goes out in order, behind the snapshot when there was one,
	// and a repaint asked for from here on has a screen of its own to send.
	attach.joined()
	if wantReplay {
		replay.AfterReplay()
	}

	// A client that arrived after a fast command finished still gets the exit
	// frame, after any replay, instead of a bare disconnect.
	if payload, ok := s.exitFrame(); ok {
		_ = attach.WriteFrame(frame.Exit, payload)
		attach.Close()
	}
	select {
	case <-attach.ended:
	case <-s.done:
		attach.detach(errStreamDone)
	case <-ctx.Done():
		attach.detach(ctx.Err())
	}
	return attach.finish(s.done)
}

// repaint replays the current screen to one client that is already attached.
//
// It is the replay half of Attach, at a later moment: snapshot under the lock
// and queue it, so the repaint lands behind what this client was already sent
// and ahead of the live frames after it, and let the Replayer nudge the program
// into confirming it. A stream with no Replayer has no screen to repaint and
// does nothing, which is the same answer it gives an attach that asks for one.
//
// The snapshot goes to the client that asked and to nobody else. What the
// Replayer does afterwards does not: nudging the program into redrawing is a
// program writing to its terminal, which every attacher sees, and the client
// asking has usually just re-sent its own size, which the terminal takes. That
// is not this call's to avoid — one terminal has one size, and the last client
// to name it wins — but it is why only the snapshot is described as private.
//
// A repaint asked for while this attacher is still joining does nothing. Attach
// is holding everything broadcast since registration, and is about to put it on
// the wire — behind the screen it registered with, when it asked for one. A
// second screen queued behind those frames would paint them twice, since it
// already contains them.
//
// No fence is set without a snapshot. A Replayer that has dropped its emulator
// — the screen fails open, so a panic in it degrades to plain live streaming —
// returns nothing, and a fence set for a screen that was never sent would drop
// live output that nothing has painted. Then the redraw below is the whole of
// the repaint, which is what failing open means.
func (s *Stream) repaint(attach *Attacher) {
	s.mu.Lock()
	replay := s.replay
	if replay == nil || attach.isJoining() {
		s.mu.Unlock()
		return
	}
	if snapshot := replay.Snapshot(); len(snapshot) > 0 {
		attach.beginReplay(s.observed, snapshot)
	}
	s.mu.Unlock()
	replay.AfterReplay()
}

// Broadcast delivers one chunk of the process's output to every attacher as a
// frame of typ, which is frame.Stdout or frame.Stderr.
//
// It queues the chunk for each attacher and returns; no connection is written
// on the caller's goroutine. The callers are the process's output readers (one
// per output stream), so that is what keeps a viewer that has stopped reading
// from stopping the process's output being drained, and from holding up the
// viewers queued after it. The exception is a stream with no Replayer, where a
// viewer whose queue is full holds the chunk until it has room: see
// Options.MaxPendingBytes.
func (s *Stream) Broadcast(typ byte, payload []byte) {
	// Feed the Replayer, count the chunk, and snapshot the attacher set under
	// one lock, so an attacher registering or asking for a repaint falls
	// cleanly on one side of it: either the Replayer absorbed it before the
	// snapshot was taken — and the count says so, so the attacher drops it
	// rather than painting it under the snapshot that already holds it — or it
	// is queued behind that snapshot. The queueing happens outside the lock,
	// which is why the count, and not the order of the queues, is what decides.
	s.mu.Lock()
	// Only stdout is replayable state; a stream with a Replayer is a TTY stream,
	// which never produces stderr frames anyway. Everything else is stamped
	// zero, which no snapshot can contain, so it is always delivered.
	var at uint64
	replayable := s.replay != nil
	if replayable && typ == frame.Stdout {
		s.replay.Observe(payload)
		s.observed++
		at = s.observed
	}
	attachers := s.snapshotAttachersLocked()
	s.mu.Unlock()
	if len(attachers) == 0 {
		return
	}
	// One copy, shared by every queue: a queued frame is never modified.
	payload = append([]byte(nil), payload...)
	for _, attach := range attachers {
		if err := attach.broadcast(typ, payload, at, replayable); err != nil {
			s.removeAttacher(attach)
		}
	}
}

// MarkExited retains the encoded exit frame for clients that attach later. Call
// it once the process has exited and its output has fully drained.
func (s *Stream) MarkExited(payload []byte) {
	s.mu.Lock()
	s.exitPayload = payload
	s.mu.Unlock()
}

func (s *Stream) exitFrame() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitPayload, s.exitPayload != nil
}

// Attachers returns the current attacher set.
func (s *Stream) Attachers() []*Attacher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotAttachersLocked()
}

// HasAttachers reports whether any client is attached.
func (s *Stream) HasAttachers() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attachers) > 0
}

func (s *Stream) snapshotAttachersLocked() []*Attacher {
	attachers := make([]*Attacher, 0, len(s.attachers))
	for attach := range s.attachers {
		attachers = append(attachers, attach)
	}
	return attachers
}

// addAttacher registers a joining attacher and, for a replay attach, queues
// the Replayer snapshot captured at that instant as its first frame. Both
// happen under one lock so the repaint and the live output queued behind it are
// exactly contiguous.
func (s *Stream) addAttacher(attach *Attacher, replay bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachers[attach] = struct{}{}
	if !replay || s.replay == nil {
		return
	}
	attach.beginReplay(s.observed, s.replay.Snapshot())
}

func (s *Stream) removeAttacher(attach *Attacher) {
	s.mu.Lock()
	delete(s.attachers, attach)
	s.mu.Unlock()
}

// ApplyResize records the size a client asked for and releases WaitForResize.
func (s *Stream) ApplyResize(resize frame.ResizePayload) {
	s.mu.Lock()
	s.resize = &resize
	s.mu.Unlock()
	s.resizeOnce.Do(func() { close(s.resizeReady) })
}

// PendingResize returns the most recent size a client asked for.
func (s *Stream) PendingResize() (frame.ResizePayload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resize == nil {
		return frame.ResizePayload{}, false
	}
	return *s.resize, true
}

// WaitForResize blocks until a client has sent a size or ctx is done. A process
// started before the first resize would otherwise come up at a default size and
// repaint noisily.
func (s *Stream) WaitForResize(ctx context.Context) {
	s.mu.Lock()
	ready := s.resize != nil
	s.mu.Unlock()
	if ready {
		return
	}
	select {
	case <-s.resizeReady:
	case <-ctx.Done():
	}
}

func (s *Stream) readFrames(attach *Attacher) {
	var receiver *resume.Receiver
	defer func() {
		if receiver != nil {
			receiver.Close()
		}
	}()
	for {
		next, err := attach.conn.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && !errors.Is(err, net.ErrClosed) {
				_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
			}
			attach.Close()
			return
		}
		switch next.Type {
		case frame.Session:
			if receiver != nil {
				s.failAttach(attach, fmt.Errorf("%w: session already established", resume.ErrProtocol))
				return
			}
			var established []byte
			receiver, established, err = s.resumes.Accept(next.Payload)
			if err != nil {
				s.failAttach(attach, err)
				return
			}
			if err := attach.writeControlFrame(frame.SessionOK, established); err != nil {
				attach.Close()
				return
			}
			continue
		case frame.Action:
			if receiver == nil {
				s.failAttach(attach, fmt.Errorf("%w: action arrived before session", resume.ErrProtocol))
				return
			}
			position, err := receiver.Apply(next.Payload, s.applyFrame)
			if err != nil {
				s.failAttach(attach, err)
				return
			}
			if err := attach.writeControlFrame(frame.Ack, resume.EncodePosition(position)); err != nil {
				attach.Close()
				return
			}
			continue
		case frame.Ready:
			attach.markReady()
			continue
		case frame.Repaint:
			// The screen it sends back goes to this attacher alone, so it is
			// answered here, where the attacher is known, rather than through
			// onFrame, which is the process's. What it sets in motion after
			// that is not private: see Stream.repaint.
			s.repaint(attach)
			continue
		case frame.SessionOK, frame.Ack:
			s.failAttach(attach, fmt.Errorf("%w: unexpected client frame type %d", resume.ErrProtocol, next.Type))
			return
		}
		// A physical attach that never establishes a resumable session remains a
		// valid direct attach. This is an intentional protocol capability for
		// one-shot and fail-fast execs, not a compatibility path.
		if receiver != nil && resume.IsActionType(next.Type) {
			s.failAttach(attach, fmt.Errorf("%w: resumable action type %d was not positioned", resume.ErrProtocol, next.Type))
			return
		}
		if err := s.applyFrame(next); err != nil {
			s.failAttach(attach, err)
			return
		}
	}
}

func (s *Stream) applyFrame(next frame.Frame) error {
	if s.onFrame == nil {
		return nil
	}
	return s.onFrame(next)
}

// failAttach ends an attach on a protocol error. The Error frame is queued
// behind the output the client is already owed, not ahead of it: a client
// stops reading at an Error, so one that overtook output would lose it, the
// exit frame included.
func (s *Stream) failAttach(attach *Attacher, err error) {
	_ = attach.WriteFrame(frame.Error, []byte(err.Error()))
	attach.Close()
}

// ReadyTimeout bounds how long a replay attach waits for the client's
// frame.Ready before streaming history anyway. Conforming clients always send
// it; the timeout only covers ones that do not.
const ReadyTimeout = 5 * time.Second

func (s *Stream) waitForReady(attach *Attacher) {
	timer := time.NewTimer(ReadyTimeout)
	defer timer.Stop()
	select {
	case <-attach.ready:
	case <-attach.ended:
	case <-s.done:
	case <-timer.C:
	}
}

// Attacher is one attached client.
//
// Its frames go through one bounded queue that a single writer goroutine
// (write) puts on the connection, so nothing that queues a frame waits on the
// client. Session acknowledgements (SessionOK, Ack) have a queue of their own
// that is written first and is not held while the attacher joins, because a
// resumable client must receive SessionOK before it can send the Ready that
// ends the join. Both count against one byte budget.
type Attacher struct {
	conn         execstream.Conn
	stream       *Stream
	maxPending   int
	writeTimeout time.Duration
	// ready is closed when the client sends frame.Ready, signaling that the
	// tunnel is established end to end and it is reading.
	ready     chan struct{}
	readyOnce sync.Once
	// ended is closed once the attach is over — closing gracefully or
	// detached — and is what Attach waits on; detached only once it is
	// detached. writerDone is closed when the writer goroutine has returned.
	ended      chan struct{}
	endOnce    sync.Once
	detached   chan struct{}
	writerDone chan struct{}

	mu sync.Mutex
	// changed is signaled whenever a queue, the budget, or the attacher's
	// state changes. The writer waits on it for a frame, and a producer of a
	// stream with no Replayer waits on it for room.
	changed *sync.Cond
	control []queuedFrame
	data    []queuedFrame
	// pending is the bytes queued in both queues plus the frame being
	// written, so the budget covers everything this client has not taken yet.
	pending int
	// joining is set until Attach has finished joining: the data queue is
	// held, so the snapshot queued at registration and everything broadcast
	// behind it reach the wire together once the client is ready. See
	// Stream.repaint for why a repaint waits for it too.
	joining bool
	// snapshotQueued says a replay snapshot is queued and not yet written,
	// which is the one frame the budget does not count (beginReplay).
	snapshotQueued bool
	// replayedThrough is the Replayer's chunk count at the moment this
	// attacher's latest snapshot was taken. Everything up to it is in that
	// snapshot, so delivering it again would paint it twice.
	replayedThrough uint64
	// closing says the attach is ending normally: the writer puts what is
	// queued on the wire and returns, and nothing more is accepted.
	closing bool
	// err is why the attach was detached; set once, and nothing is written
	// after it.
	err error
}

type queuedFrame struct {
	typ     byte
	payload []byte
	// cost is what the frame counts against the budget: its payload plus
	// queuedFrameOverhead, or zero for a replay snapshot, which the budget
	// does not cover (see Options.MaxPendingBytes).
	cost int
}

// queuedFrameOverhead is what a frame costs the budget beyond its payload, so
// a stream of empty frames is still bounded.
const queuedFrameOverhead = 16

func newQueuedFrame(typ byte, payload []byte) queuedFrame {
	return queuedFrame{typ: typ, payload: payload, cost: len(payload) + queuedFrameOverhead}
}

func (s *Stream) newAttacher(conn execstream.Conn) *Attacher {
	a := &Attacher{
		conn:         conn,
		stream:       s,
		maxPending:   s.maxPending,
		writeTimeout: s.writeTimeout,
		ready:        make(chan struct{}),
		ended:        make(chan struct{}),
		detached:     make(chan struct{}),
		writerDone:   make(chan struct{}),
		joining:      true,
	}
	a.changed = sync.NewCond(&a.mu)
	return a
}

// beginReplay queues a snapshot and records the chunk count it contains, so it
// reaches the wire behind what this client was already sent and is not painted
// over by the chunks inside it, which are dropped from here on. Attach begins
// there; a mid-stream repaint returns there.
//
// It is called under the Stream lock, beside the Snapshot it describes. That is
// what makes the pair atomic against Broadcast, which counts a chunk under the
// same lock and queues it after releasing it.
//
// The snapshot is queued outside the budget: the attach is owed it whatever
// its size, and counting it would leave a viewer that reconnected after
// falling behind a budget its own repaint had already spent. Only one is,
// though: a second asked for while one is still queued counts. Only a
// terminal stream has a Replayer, so this is always a replayable attacher.
func (a *Attacher) beginReplay(through uint64, snapshot []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replayedThrough = through
	if len(snapshot) == 0 || a.endedLocked() {
		return
	}
	f := queuedFrame{typ: frame.Stdout, payload: append([]byte(nil), snapshot...)}
	if a.snapshotQueued {
		// One repaint at a time rides outside the budget. A client asking for
		// another before it has taken the last is paying for it, or every
		// Repaint frame from one that stopped reading would queue a screen.
		f.cost = len(f.payload) + queuedFrameOverhead
		if a.pending > 0 && a.pending+f.cost > a.maxPending {
			a.detachLocked(fmt.Errorf("%w: %d bytes unwritten when another repaint was asked for", ErrFellBehind, a.pending))
			return
		}
		a.pending += f.cost
	}
	a.snapshotQueued = true
	a.data = append(a.data, f)
	a.changed.Broadcast()
}

// joined says this attacher's registration window is over: the data queue is
// released to the writer.
func (a *Attacher) joined() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.joining = false
	a.changed.Broadcast()
}

// isJoining reports whether this attacher is still inside the window Attach
// holds its output for.
func (a *Attacher) isJoining() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.joining
}

// broadcast queues one chunk of the process's output, which the Replayer
// absorbed as its chunk number at — or zero for output no snapshot can hold.
// payload is shared with the other attachers and is not modified.
//
// A chunk the client's own snapshot already contains is dropped. Without that,
// a chunk counted just before a repaint took its snapshot, and queued just
// after, would land behind that snapshot and be painted a second time: for a
// TUI the redraw hides it, and for anything that scrolls it stays on the screen
// as a duplicated line under the repaint that was meant to put the screen
// right.
func (a *Attacher) broadcast(typ byte, payload []byte, at uint64, replayable bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at != 0 && at <= a.replayedThrough {
		return nil
	}
	return a.enqueueLocked(&a.data, newQueuedFrame(typ, payload), replayable)
}

// WriteFrame queues one frame behind the output already queued for this
// attacher. It does not wait for the client, except as Options.MaxPendingBytes
// describes for a stream with no Replayer.
func (a *Attacher) WriteFrame(typ byte, payload []byte) error {
	replayable := a.stream.replayer() != nil
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enqueueLocked(&a.data, newQueuedFrame(typ, append([]byte(nil), payload...)), replayable)
}

// writeControlFrame queues a session acknowledgement (SessionOK, Ack) ahead of
// the output, and is written even while the attacher joins. A resumable client
// must receive SessionOK before it can send Ready, which is what ends the join.
// Errors go through WriteFrame, behind the output: see Stream.failAttach.
func (a *Attacher) writeControlFrame(typ byte, payload []byte) error {
	replayable := a.stream.replayer() != nil
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enqueueLocked(&a.control, newQueuedFrame(typ, append([]byte(nil), payload...)), replayable)
}

// enqueueLocked appends f to queue within the budget. An attacher with nothing
// pending takes any one frame, however large, so a frame bigger than the
// budget is delivered rather than refused. When f does not fit, a replayable
// attacher is detached and a non-replayable one waits for room: see
// Options.MaxPendingBytes.
func (a *Attacher) enqueueLocked(queue *[]queuedFrame, f queuedFrame, replayable bool) error {
	for {
		if a.err != nil {
			return a.err
		}
		if a.closing {
			return errEnded
		}
		if a.pending == 0 || a.pending+f.cost <= a.maxPending {
			break
		}
		if replayable {
			a.detachLocked(fmt.Errorf("%w: %d bytes unwritten", ErrFellBehind, a.pending))
			return a.err
		}
		a.changed.Wait()
	}
	*queue = append(*queue, f)
	a.pending += f.cost
	a.changed.Broadcast()
	return nil
}

// write is the attacher's one writer: it takes frames off the queues, control
// first, and writes them in order until the attach is over. On a stream with a
// Replayer each write is watched: one that does not complete within the write
// timeout detaches the attacher, and Attach then closes the connection, which
// is what returns the write. A pipe stream's writes are not: see
// Options.WriteTimeout.
func (a *Attacher) write() {
	defer close(a.writerDone)
	stall := time.AfterFunc(a.writeTimeout, func() {
		a.detach(fmt.Errorf("%w: a frame write made no progress for %s", ErrStalled, a.writeTimeout))
	})
	stall.Stop()
	defer stall.Stop()
	for {
		f, ok := a.next()
		if !ok {
			return
		}
		// Asked per frame: a stream gains its Replayer once its PTY exists,
		// which may be after this attacher joined.
		watched := a.stream.replayer() != nil
		if watched {
			stall.Reset(a.writeTimeout)
		}
		err := a.conn.WriteFrame(f.typ, f.payload)
		if watched {
			stall.Stop()
		}
		a.mu.Lock()
		if a.err == nil { // a detach already released the whole budget
			a.pending -= f.cost
		}
		if f.typ == frame.Stdout && f.cost == 0 { // the uncounted snapshot
			a.snapshotQueued = false
		}
		a.changed.Broadcast()
		a.mu.Unlock()
		if err != nil {
			a.detach(fmt.Errorf("write: %w", err))
			return
		}
	}
}

// next waits for the next frame to write. It reports false once there is
// nothing left to write: the attacher was detached, or it is closing and its
// queues are empty.
func (a *Attacher) next() (queuedFrame, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		if a.err != nil {
			return queuedFrame{}, false
		}
		if len(a.control) > 0 {
			return pop(&a.control), true
		}
		if !a.joining && len(a.data) > 0 {
			return pop(&a.data), true
		}
		// A joining attacher that is closing still owes its client what it
		// holds — a replay and an exit frame, say — so the writer waits for
		// the join to end rather than leaving with it.
		if a.closing && !a.joining {
			return queuedFrame{}, false
		}
		a.changed.Wait()
	}
}

func pop(queue *[]queuedFrame) queuedFrame {
	f := (*queue)[0]
	(*queue)[0] = queuedFrame{}
	*queue = (*queue)[1:]
	if len(*queue) == 0 {
		*queue = nil
	}
	return f
}

// Close ends this attach once what is queued for it has been written; nothing
// queued after it is accepted. The drain has no deadline of its own: see
// Stream.Attach.
func (a *Attacher) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.endedLocked() {
		return
	}
	a.closing = true
	a.endLocked()
}

// detach ends this attach at once, for err: what is queued is dropped and
// nothing more is written.
func (a *Attacher) detach(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.detachLocked(err)
}

func (a *Attacher) detachLocked(err error) {
	if a.err != nil {
		return
	}
	a.err = err
	a.control, a.data, a.pending, a.snapshotQueued = nil, nil, 0, false
	close(a.detached)
	a.endLocked()
}

func (a *Attacher) endLocked() {
	a.endOnce.Do(func() { close(a.ended) })
	a.changed.Broadcast()
}

// endedLocked reports whether the attach is over, closing or detached.
func (a *Attacher) endedLocked() bool { return a.closing || a.err != nil }

// finish waits for the writer to put what is queued on the wire — however
// long the client takes, since an output deadline is a truncation — unless the
// attacher is detached or done closes meanwhile. Then it closes the
// connection, returning a write still blocked on the client and the reader,
// and waits for the writer to return. It reports why the attacher was
// detached, or nil if it ended normally.
//
// A terminal stream's writer is still bounded while it drains, by the write
// timeout on each frame; a pipe stream's drain is bounded only by its client
// and by done.
func (a *Attacher) finish(done <-chan struct{}) error {
	select {
	case <-a.writerDone:
	case <-a.detached:
	case <-done:
		a.detach(errStreamDone)
	}
	_ = a.conn.Close()
	<-a.writerDone
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

func (a *Attacher) markReady() {
	a.readyOnce.Do(func() { close(a.ready) })
}
