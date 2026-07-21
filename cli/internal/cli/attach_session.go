package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/execstream/frame"

	"golang.org/x/term"
)

type framedAttachSession struct {
	frames     attachFrameTransport
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	kind       string
	action     string
	rawMode    bool
	resize     bool
	copyInput  func(context.Context, *framedAttachSession) error
	errorFrame func([]byte) error
	otherErr   func(error) (bool, error)
	// signalReady sends a Ready frame once the output reader is running, telling
	// the shim it is safe to stream replay history. Set for replay attaches.
	signalReady bool
	// rawFile and rawState record the terminal put into raw mode by run, so a
	// suspend can hand it back in the mode the shell expects and take it again on
	// resume. rawState is always the mode from before the attach, so the deferred
	// restore in run stays correct across any number of suspends.
	rawFile  *os.File
	rawState *term.State
}

type attachFrameTransport interface {
	ReadFrame() (frame.Frame, error)
	WriteFrame(typ byte, payload []byte) error
	Close() error
}

type directAttachFrames struct {
	conn io.ReadWriteCloser
	mu   sync.Mutex
}

func (c *directAttachFrames) ReadFrame() (frame.Frame, error) {
	return frame.Read(c.conn)
}

func (c *directAttachFrames) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return frame.Write(c.conn, typ, payload)
}

func (c *directAttachFrames) Close() error { return c.conn.Close() }

func (s *framedAttachSession) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resizeFile, _ := s.stdin.(*os.File)
	if s.rawMode && resizeFile != nil && term.IsTerminal(int(resizeFile.Fd())) {
		state, err := term.MakeRaw(int(resizeFile.Fd()))
		if err != nil {
			return err
		}
		s.rawFile, s.rawState = resizeFile, state
		defer func() { _ = term.Restore(int(resizeFile.Fd()), state) }()
	}

	outputErr := make(chan error, 1)
	otherErr := make(chan error, 3)
	go func() { outputErr <- s.copyOutput() }()
	if s.signalReady {
		// The output reader is running; tell the shim the tunnel is established so
		// it can stream replay history without losing bytes to the handshake.
		if err := s.writeFrame(frame.Ready, nil); err != nil {
			return err
		}
	}
	go func() {
		if s.copyInput != nil {
			if err := s.copyInput(ctx, s); err != nil {
				otherErr <- err
			}
		}
	}()
	if s.resize {
		go func() {
			if err := s.watchResize(ctx, resizeFile); err != nil {
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
			_ = s.frames.Close()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case err := <-otherErr:
			if s.otherErr != nil {
				handled, out := s.otherErr(err)
				if handled {
					continue
				}
				cancel()
				_ = s.frames.Close()
				return out
			}
			cancel()
			_ = s.frames.Close()
			if isAttachDone(err) {
				return nil
			}
			return err
		case <-ctx.Done():
			_ = s.frames.Close()
			return ctx.Err()
		}
	}
}

func (s *framedAttachSession) writeInitialResize() error {
	file, _ := s.stdin.(*os.File)
	cols, rows, ok := terminalSize(file)
	if !ok {
		return nil
	}
	return s.writeResize(cols, rows)
}

func (s *framedAttachSession) copyOutput() error {
	for {
		f, err := s.frames.ReadFrame()
		if err != nil {
			return err
		}
		switch f.Type {
		case frame.Stdout:
			if _, err := s.stdout.Write(f.Payload); err != nil {
				return err
			}
		case frame.Stderr:
			if _, err := s.stderr.Write(f.Payload); err != nil {
				return err
			}
		case frame.Error:
			if s.errorFrame != nil {
				return s.errorFrame(f.Payload)
			}
			return nil
		case frame.Exit:
			return attachExitErrorFromPayload(s.kind, f.Payload)
		default:
			return fmt.Errorf("%s: unexpected frame type %d", s.action, f.Type)
		}
	}
}

func (s *framedAttachSession) closeInput() error {
	return s.writeFrame(frame.CloseInput, nil)
}

func (s *framedAttachSession) watchResize(ctx context.Context, file *os.File) error {
	cols, rows, ok := terminalSize(file)
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
			nextCols, nextRows, ok := terminalSize(file)
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

func (s *framedAttachSession) proxySignals(ctx context.Context) error {
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, proxiedSignals()...)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-signals:
			if isSuspendSignal(sig) {
				if err := s.suspend(); err != nil {
					return err
				}
				continue
			}
			name, ok := signalName(sig)
			if !ok {
				continue
			}
			if err := s.writeFrame(frame.Signal, []byte(name)); err != nil {
				return err
			}
		}
	}
}

// suspend gives Ctrl-Z its local meaning across the connection: the remote job
// stops, this process stops, and fg resumes both. Forwarding alone would leave
// the user attached to a stopped job with no way to resume it, and stopping
// alone would leave the remote running unattended — which is the bug this
// avoids.
//
// The terminal is handed back in its pre-attach mode before stopping, since the
// shell that regains it expects cooked mode, and taken again on resume. The
// remote may also have missed resizes while this process was stopped, so the
// current size is re-sent.
func (s *framedAttachSession) suspend() error {
	if err := s.writeFrame(frame.Signal, []byte("TSTP")); err != nil {
		return err
	}
	if s.rawFile != nil && s.rawState != nil {
		_ = term.Restore(int(s.rawFile.Fd()), s.rawState)
	}
	suspendSelf()
	if s.rawFile != nil && s.rawState != nil {
		// Discard the returned state: s.rawState must stay the pre-attach mode so
		// the final restore in run leaves the terminal as the user handed it over.
		_, _ = term.MakeRaw(int(s.rawFile.Fd()))
	}
	if err := s.writeFrame(frame.Signal, []byte("CONT")); err != nil {
		return err
	}
	if s.resize {
		return s.writeInitialResize()
	}
	return nil
}

func (s *framedAttachSession) writeResize(cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > math.MaxUint16 || rows > math.MaxUint16 {
		return nil
	}
	payload, err := json.Marshal(struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}
	return s.writeFrame(frame.Resize, payload)
}

func (s *framedAttachSession) writeFrame(typ byte, payload []byte) error {
	return s.frames.WriteFrame(typ, payload)
}

func terminalSize(file *os.File) (cols, rows int, ok bool) {
	if file == nil || !term.IsTerminal(int(file.Fd())) {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(int(file.Fd()))
	return cols, rows, err == nil && cols > 0 && rows > 0
}

func printAttachErrorFrame(stderr io.Writer) func([]byte) error {
	return func(payload []byte) error {
		message := strings.TrimSpace(string(payload))
		if message != "" {
			_, _ = fmt.Fprintln(stderr, message)
		}
		return nil
	}
}
