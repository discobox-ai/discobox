// Package host serves one process's output to any number of attached clients.
//
// It owns the attacher set, the ordering guarantees around joining and leaving
// it, and the retained exit frame. It owns no process, no terminal, and no
// transport: the process lives behind Broadcast and the OnFrame callback, the
// terminal (if any) lives behind Replayer, and the transport is an
// execstream.Conn. That is what makes the ordering rules here testable over
// net.Pipe, with no sockets and no PTY.
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

	"github.com/discobox-ai/discobox/execstream"
	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/execstream/resume"
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
	// Done is closed when the process is gone; it releases attachers blocked
	// waiting for output.
	Done <-chan struct{}
	// Replay, when set, enables repaint-on-attach for clients that ask for it.
	Replay Replayer
	// OnFrame applies the control frames a client sends — input, resize, signal,
	// close-input. Ready and resumable-transport frames are consumed by this
	// package and never reach it.
	OnFrame func(frame.Frame) error
}

// Stream is the fan-out for one process.
type Stream struct {
	done    <-chan struct{}
	onFrame func(frame.Frame) error
	resumes *resume.Server

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
	return &Stream{
		done:        opts.Done,
		replay:      opts.Replay,
		onFrame:     opts.OnFrame,
		resumes:     resume.NewServer(),
		attachers:   map[*Attacher]struct{}{},
		resizeReady: make(chan struct{}),
	}
}

// AttachOptions configures one attach.
type AttachOptions struct {
	// Replay asks for the current state to be repainted before live output. It
	// is honored only when the Stream has a Replayer.
	Replay bool
	// Ready is called once this attacher has joined the broadcast set and is
	// buffering, and before any frame reaches conn. A transport that must
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
func (s *Stream) Attach(ctx context.Context, conn execstream.Conn, opts AttachOptions) error {
	attach := &Attacher{
		conn:      conn,
		done:      make(chan struct{}),
		ready:     make(chan struct{}),
		buffering: true,
	}

	// Register before announcing: see AttachOptions.Ready. Buffering means live
	// frames queue rather than racing the announcement onto the wire.
	replay := s.replayer()
	wantReplay := opts.Replay && replay != nil
	// Set before registration, so a Repaint frame that arrives on the read
	// goroutine below — started once the announcement is out — finds the join
	// still in progress rather than racing it. See Stream.repaint.
	attach.joining = true
	snapshot := s.addAttacher(attach, wantReplay)
	defer s.removeAttacher(attach)

	if opts.Ready != nil {
		if err := opts.Ready(); err != nil {
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
		if err := attach.writeSnapshot(snapshot); err != nil {
			attach.Close()
		}
	}
	// Live streaming begins here for every attach: everything broadcast since
	// registration goes out in order, behind the snapshot when there was one.
	if err := attach.flushBuffer(); err != nil {
		attach.Close()
	}
	// Everything owed on attach is on the wire, so a repaint asked for from
	// here on has a screen of its own to send and a buffer of its own to flush.
	attach.joined()
	if wantReplay {
		replay.AfterReplay()
	}

	// A client that arrived after a fast command finished still gets the exit
	// frame, after any replay, instead of a bare disconnect.
	if payload, ok := s.exitFrame(); ok {
		_ = attach.WriteFrame(frame.Exit, payload)
		return nil
	}
	select {
	case <-attach.done:
	case <-s.done:
	case <-ctx.Done():
	}
	return nil
}

// repaint replays the current screen to one client that is already attached.
//
// It is the replay half of Attach, at a later moment: buffer, snapshot under
// the lock so the repaint and the live frames behind it stay contiguous, write
// it ahead of them, and let the Replayer nudge the program into confirming it.
// A stream with no Replayer has no screen to repaint and does nothing, which is
// the same answer it gives an attach that asks for one.
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
// the wire — behind the screen it registered with, when it asked for one.
// Painting a second screen into that window would race the two onto the wire in
// either order, and the older one landing last is a client left staring at a
// screen from before it asked; flushing that buffer early would put frames the
// new snapshot already contains on top of it.
//
// The snapshot is taken before the fence is set, and no fence is set without
// one. A Replayer that has dropped its emulator — the screen fails open, so a
// panic in it degrades to plain live streaming — returns nothing, and a fence
// set for a screen that was never sent would drop live output that nothing has
// painted. Then the redraw below is the whole of the repaint, which is what
// failing open means.
func (s *Stream) repaint(attach *Attacher) {
	s.mu.Lock()
	replay := s.replay
	var snapshot []byte
	if replay != nil && !attach.isJoining() {
		snapshot = replay.Snapshot()
		if len(snapshot) > 0 {
			attach.beginReplay(s.observed)
		}
	} else {
		replay = nil
	}
	s.mu.Unlock()
	if replay == nil {
		return
	}
	if err := attach.writeSnapshot(snapshot); err != nil {
		attach.Close()
	}
	if err := attach.flushBuffer(); err != nil {
		attach.Close()
	}
	replay.AfterReplay()
}

// Broadcast delivers one chunk of the process's output to every attacher as a
// frame of typ, which is frame.Stdout or frame.Stderr.
func (s *Stream) Broadcast(typ byte, payload []byte) {
	// Feed the Replayer, count the chunk, and snapshot the attacher set under
	// one lock, so an attacher registering or asking for a repaint falls
	// cleanly on one side of it: either the Replayer absorbed it before the
	// snapshot was taken — and the count says so, so the attacher drops it
	// rather than painting it under the snapshot that already holds it — or it
	// arrives as a buffered live frame behind that snapshot. The wire writes
	// stay outside the lock so a slow client cannot stall the process, which is
	// why the count, and not the write order, is what decides.
	s.mu.Lock()
	// Only stdout is replayable state; a stream with a Replayer is a TTY stream,
	// which never produces stderr frames anyway. Everything else is stamped
	// zero, which no snapshot can contain, so it is always delivered.
	var at uint64
	if s.replay != nil && typ == frame.Stdout {
		s.replay.Observe(payload)
		s.observed++
		at = s.observed
	}
	attachers := s.snapshotAttachersLocked()
	s.mu.Unlock()
	for _, attach := range attachers {
		if err := attach.broadcast(typ, payload, at); err != nil {
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

// addAttacher registers a buffering attacher and, for a replay attach, returns
// the Replayer snapshot captured at that instant. Both happen under one lock so
// the repaint and the buffered live output are exactly contiguous.
func (s *Stream) addAttacher(attach *Attacher, replay bool) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachers[attach] = struct{}{}
	if !replay || s.replay == nil {
		return nil
	}
	attach.beginReplay(s.observed)
	return s.replay.Snapshot()
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
			var position uint64
			receiver, position, err = s.resumes.Accept(next.Payload)
			if err != nil {
				s.failAttach(attach, err)
				return
			}
			if err := attach.writeControlFrame(frame.SessionOK, resume.EncodePosition(position)); err != nil {
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

func (s *Stream) failAttach(attach *Attacher, err error) {
	_ = attach.writeControlFrame(frame.Error, []byte(err.Error()))
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
	case <-attach.done:
	case <-s.done:
	case <-timer.C:
	}
}

// Attacher is one attached client.
type Attacher struct {
	conn      execstream.Conn
	mu        sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
	// ready is closed when the client sends frame.Ready, signaling that the
	// tunnel is established end to end and it is reading.
	ready     chan struct{}
	readyOnce sync.Once
	// While buffering, live frames queue instead of being written, so a replay
	// can reach the wire first. flushBuffer drains the queue and clears the flag.
	buffering bool
	buffered  []bufferedFrame
	// replayedThrough is the Replayer's chunk count at the moment this
	// attacher's snapshot was taken. Everything up to it is in that snapshot,
	// so delivering it again would paint it twice.
	replayedThrough uint64
	// joining is set until Attach has put everything buffered since
	// registration on the wire, replay or no replay. See Stream.repaint.
	joining bool
}

type bufferedFrame struct {
	typ     byte
	payload []byte
}

// beginReplay sends live frames back to the queue and records the chunk count
// the snapshot about to be taken contains, so a repaint reaches the wire ahead
// of the frames behind it and is not painted over by the ones inside it. Attach
// begins there; a mid-stream repaint returns there.
//
// It is called under the Stream lock, beside the Snapshot it describes. That is
// what makes the pair atomic against Broadcast, which counts a chunk under the
// same lock and delivers it after releasing it.
func (a *Attacher) beginReplay(through uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buffering = true
	a.replayedThrough = through
}

// joined says this attacher's registration window is over: whatever it was
// owed on attach has reached the wire.
func (a *Attacher) joined() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.joining = false
}

// isJoining reports whether this attacher is still inside the window Attach
// buffers for.
func (a *Attacher) isJoining() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.joining
}

// broadcast delivers one chunk of the process's output, which the Replayer
// absorbed as its chunk number at — or zero for output no snapshot can hold.
//
// A chunk the client's own snapshot already contains is dropped. Without that,
// a chunk counted just before a repaint took its snapshot, and written just
// after, would be queued behind that snapshot and painted a second time: for a
// TUI the redraw hides it, and for anything that scrolls it stays on the screen
// as a duplicated line under the repaint that was meant to put the screen
// right.
func (a *Attacher) broadcast(typ byte, payload []byte, at uint64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at != 0 && at <= a.replayedThrough {
		return nil
	}
	if a.buffering {
		a.buffered = append(a.buffered, bufferedFrame{typ: typ, payload: append([]byte(nil), payload...)})
		return nil
	}
	return a.writeLocked(typ, payload)
}

// WriteFrame sends one frame, or queues it while this attacher is buffering.
func (a *Attacher) WriteFrame(typ byte, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.buffering {
		a.buffered = append(a.buffered, bufferedFrame{typ: typ, payload: append([]byte(nil), payload...)})
		return nil
	}
	return a.writeLocked(typ, payload)
}

// writeControlFrame bypasses replay buffering for transport acknowledgements
// and protocol errors. A resumable client must receive SessionOK before it can
// send Ready, which is what releases the replay buffer.
func (a *Attacher) writeControlFrame(typ byte, payload []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeLocked(typ, payload)
}

func (a *Attacher) writeLocked(typ byte, payload []byte) error {
	if err := a.conn.WriteFrame(typ, payload); err != nil {
		a.Close()
		return err
	}
	return nil
}

// writeSnapshot writes the repaint straight to the wire, ahead of the buffered
// live frames. The attacher is still buffering, so nothing else can interleave.
// An empty snapshot writes nothing.
func (a *Attacher) writeSnapshot(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeLocked(frame.Stdout, payload)
}

// flushBuffer writes the queued live frames in order and switches to live
// streaming. The queue holds exactly what was broadcast from registration
// onward, so it continues seamlessly from the snapshot.
func (a *Attacher) flushBuffer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.buffered {
		if err := a.writeLocked(f.typ, f.payload); err != nil {
			return err
		}
	}
	a.buffered = nil
	a.buffering = false
	return nil
}

// Close ends this attach.
func (a *Attacher) Close() {
	a.closeOnce.Do(func() { close(a.done) })
}

func (a *Attacher) markReady() {
	a.readyOnce.Do(func() { close(a.ready) })
}
