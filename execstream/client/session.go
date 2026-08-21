// Package client attaches a caller's stdio to a remote process over an
// execstream connection.
//
// Everything this package does to the machine it runs on — raw mode, terminal
// size, signal delivery, stopping and resuming — goes through Console. That is
// what makes the parts worth testing testable: the order a suspend does things
// in, or which signals become frames, are properties of this package, while
// actually stopping a process is the platform's. The real implementation is
// OSConsole; tests supply their own.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/execstream"
	"github.com/discobox-ai/discobox/execstream/frame"
)

// Console is every interaction with the terminal and signals this session
// makes. A nil Console means the caller has no terminal to lend: no raw mode,
// no resize tracking, no signal forwarding.
type Console interface {
	// MakeRaw puts the terminal into raw mode and returns a func restoring the
	// mode it was in. When there is no terminal it returns a no-op restore and
	// no error, so callers need not special-case it.
	MakeRaw() (restore func(), err error)
	// Size reports the terminal size, or ok=false when there is no terminal.
	Size() (cols, rows int, ok bool)
	// Suspend stops this process and returns once it is resumed.
	Suspend()
	// NotifySignals starts delivering the signals worth forwarding to ch.
	NotifySignals(ch chan<- os.Signal)
	// StopSignals stops delivery to ch.
	StopSignals(ch chan<- os.Signal)
	// IsSuspendSignal reports whether sig means "stop this job".
	IsSuspendSignal(sig os.Signal) bool
	// SignalName is the wire name for sig, or ok=false to drop it.
	SignalName(sig os.Signal) (string, bool)
}

// Options configures a Session.
type Options struct {
	Conn   execstream.Conn
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Console is the terminal and signal environment. Nil disables raw mode,
	// resize tracking, and signal forwarding.
	Console Console
	// Kind names the remote thing in error messages ("sandbox exec").
	Kind string
	// Action names what failed in error messages ("attach exec").
	Action string
	// RawMode asks for the terminal to be put in raw mode for the session, so
	// keystrokes reach the remote process instead of the local line discipline.
	RawMode bool
	// Resize tracks the terminal size and sends it to the remote process.
	Resize bool
	// SignalReady sends a Ready frame once the output reader is running, telling
	// the remote it is safe to stream replay history. Set for replay attaches.
	SignalReady bool
	// CopyInput moves stdin to the remote. It owns the policy — detach
	// sequences, whether EOF closes the remote's stdin — which differs per
	// caller and is not the protocol's business.
	CopyInput func(context.Context, *Session) error
	// ErrorFrame handles an error frame from the remote. Nil ends the session.
	ErrorFrame func(payload []byte) error
	// OtherErr inspects a non-output error before it ends the session,
	// reporting whether it was handled.
	OtherErr func(error) (bool, error)
}

// Session is one attached stream.
type Session struct {
	opts Options
	// rawRestore returns the terminal to the mode it was in before the attach.
	// It is retained so a suspend can hand the terminal back and take it again
	// without disturbing the final restore.
	rawRestore func()
	writeMu    sync.Mutex
}

func New(opts Options) *Session { return &Session{opts: opts} }

// Stdin is the caller's input, for CopyInput implementations.
func (s *Session) Stdin() io.Reader { return s.opts.Stdin }

// WriteFrame sends one frame to the remote.
func (s *Session) WriteFrame(typ byte, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.opts.Conn.WriteFrame(typ, payload)
}

// CloseInput tells the remote no more input is coming, without detaching.
func (s *Session) CloseInput() error { return s.WriteFrame(frame.CloseInput, nil) }

// WriteInitialResize sends the current terminal size, if there is one.
func (s *Session) WriteInitialResize() error {
	cols, rows, ok := s.size()
	if !ok {
		return nil
	}
	return s.writeResize(cols, rows)
}

// Run serves the session until the remote exits, the caller detaches, or ctx is
// canceled. A remote process that exited non-zero is reported as an ExitError.
func (s *Session) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if s.opts.RawMode && s.opts.Console != nil {
		restore, err := s.opts.Console.MakeRaw()
		if err != nil {
			return err
		}
		s.rawRestore = restore
		defer restore()
	}

	outputErr := make(chan error, 1)
	otherErr := make(chan error, 3)
	go func() { outputErr <- s.copyOutput() }()
	if s.opts.SignalReady {
		// The output reader is running; tell the remote the tunnel is
		// established so it can stream replay history without losing bytes to
		// the handshake.
		if err := s.WriteFrame(frame.Ready, nil); err != nil {
			return err
		}
	}
	go func() {
		if s.opts.CopyInput != nil {
			if err := s.opts.CopyInput(ctx, s); err != nil {
				otherErr <- err
			}
		}
	}()
	if s.opts.Resize {
		go func() {
			if err := s.watchResize(ctx); err != nil {
				otherErr <- err
			}
		}()
	}
	go func() {
		if err := s.proxySignals(ctx); err != nil {
			otherErr <- err
		}
	}()

	for {
		select {
		case err := <-outputErr:
			cancel()
			_ = s.opts.Conn.Close()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case err := <-otherErr:
			if s.opts.OtherErr != nil {
				handled, out := s.opts.OtherErr(err)
				if handled {
					continue
				}
				cancel()
				_ = s.opts.Conn.Close()
				return out
			}
			cancel()
			_ = s.opts.Conn.Close()
			if IsDone(err) {
				return nil
			}
			return err
		case <-ctx.Done():
			_ = s.opts.Conn.Close()
			return ctx.Err()
		}
	}
}

func (s *Session) copyOutput() error {
	for {
		next, err := s.opts.Conn.ReadFrame()
		if err != nil {
			return err
		}
		switch next.Type {
		case frame.Stdout:
			if _, err := s.opts.Stdout.Write(next.Payload); err != nil {
				return err
			}
		case frame.Stderr:
			if _, err := s.opts.Stderr.Write(next.Payload); err != nil {
				return err
			}
		case frame.Error:
			if s.opts.ErrorFrame != nil {
				return s.opts.ErrorFrame(next.Payload)
			}
			return nil
		case frame.Exit:
			return ExitErrorFromPayload(s.opts.Kind, next.Payload)
		default:
			return fmt.Errorf("%s: unexpected frame type %d", s.opts.Action, next.Type)
		}
	}
}

func (s *Session) size() (cols, rows int, ok bool) {
	if s.opts.Console == nil {
		return 0, 0, false
	}
	return s.opts.Console.Size()
}

func (s *Session) watchResize(ctx context.Context) error {
	cols, rows, ok := s.size()
	if ok {
		if err := s.writeResize(cols, rows); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			nextCols, nextRows, ok := s.size()
			if !ok || (nextCols == cols && nextRows == rows) {
				continue
			}
			cols, rows = nextCols, nextRows
			if err := s.writeResize(cols, rows); err != nil {
				return err
			}
		}
	}
}

func (s *Session) proxySignals(ctx context.Context) error {
	if s.opts.Console == nil {
		return nil
	}
	signals := make(chan os.Signal, 8)
	s.opts.Console.NotifySignals(signals)
	defer s.opts.Console.StopSignals(signals)
	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-signals:
			if s.opts.Console.IsSuspendSignal(sig) {
				if err := s.suspend(); err != nil {
					return err
				}
				continue
			}
			name, ok := s.opts.Console.SignalName(sig)
			if !ok {
				continue
			}
			if err := s.WriteFrame(frame.Signal, []byte(name)); err != nil {
				return err
			}
		}
	}
}

// suspend gives Ctrl-Z its local meaning across the connection: the remote job
// stops, this process stops, and fg resumes both. Forwarding alone would leave
// the caller attached to a stopped job with no way to resume it; stopping alone
// would leave the remote running unattended.
//
// The terminal is handed back in its pre-attach mode before stopping, since the
// shell that regains it expects cooked mode, and taken again on resume. The
// remote may also have missed resizes while this process was stopped, so the
// current size is re-sent.
func (s *Session) suspend() error {
	if err := s.WriteFrame(frame.Signal, []byte("TSTP")); err != nil {
		return err
	}
	if s.rawRestore != nil {
		s.rawRestore()
	}
	s.opts.Console.Suspend()
	if s.rawRestore != nil {
		// Discard the new restore: s.rawRestore must stay the pre-attach mode so
		// Run's deferred restore leaves the terminal as the caller handed it over.
		if _, err := s.opts.Console.MakeRaw(); err != nil {
			return err
		}
	}
	if err := s.WriteFrame(frame.Signal, []byte("CONT")); err != nil {
		return err
	}
	if s.opts.Resize {
		return s.WriteInitialResize()
	}
	return nil
}

func (s *Session) writeResize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > math.MaxUint16 || rows > math.MaxUint16 {
		return nil
	}
	payload, err := json.Marshal(frame.ResizePayload{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}
	return s.WriteFrame(frame.Resize, payload)
}

// ExitError reports that the remote process exited with a non-zero status.
type ExitError struct {
	Code int
}

func (e ExitError) Error() string { return fmt.Sprintf("process exited with code %d", e.Code) }

// ExitCode is the status the caller should exit with, clamped to what a process
// exit status can carry.
func (e ExitError) ExitCode() int {
	code := e.Code
	if code < 0 {
		code = 1
	}
	if code > 255 {
		code = 255
	}
	return code
}

// ExitErrorFromPayload decodes an exit frame. It returns nil for a clean exit,
// an ExitError for a non-zero status, and a plain error when the remote failed
// without producing a status.
func ExitErrorFromPayload(kind string, payload []byte) error {
	exit, err := frame.DecodeExit(payload)
	if err != nil {
		return fmt.Errorf("decode %s exit frame: %w", kind, err)
	}
	if exit.ExitCode != nil && *exit.ExitCode != 0 {
		return ExitError{Code: int(*exit.ExitCode)}
	}
	if strings.EqualFold(exit.Status, "failed") {
		if strings.TrimSpace(exit.Error) == "" {
			return fmt.Errorf("%s failed", kind)
		}
		return fmt.Errorf("%s failed: %s", kind, exit.Error)
	}
	return nil
}

// IsDone reports whether err is the ordinary end of an attach rather than a
// failure worth showing the caller.
func IsDone(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "closed network connection") ||
		strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "connection reset by peer")
}
