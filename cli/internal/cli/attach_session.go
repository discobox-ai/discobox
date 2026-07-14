package cli

import (
	"context"
	"encoding/binary"
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

	"golang.org/x/term"
)

type framedAttachSession struct {
	conn       io.ReadWriteCloser
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
	mu          sync.Mutex
}

func (s *framedAttachSession) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resizeFile, _ := s.stdin.(*os.File)
	if s.rawMode && resizeFile != nil && term.IsTerminal(int(resizeFile.Fd())) {
		state, err := term.MakeRaw(int(resizeFile.Fd()))
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(int(resizeFile.Fd()), state) }()
	}

	outputErr := make(chan error, 1)
	otherErr := make(chan error, 3)
	go func() { outputErr <- s.copyOutput() }()
	if s.signalReady {
		// The output reader is running; tell the shim the tunnel is established so
		// it can stream replay history without losing bytes to the handshake.
		if err := s.writeFrame(attachFrameReady, nil); err != nil {
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
			_ = s.conn.Close()
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
				_ = s.conn.Close()
				return out
			}
			cancel()
			_ = s.conn.Close()
			if isAttachDone(err) {
				return nil
			}
			return err
		case <-ctx.Done():
			_ = s.conn.Close()
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
		frame, err := readTerminalFrame(s.conn)
		if err != nil {
			return err
		}
		switch frame.typ {
		case attachFrameOutput:
			if _, err := s.stdout.Write(frame.payload); err != nil {
				return err
			}
		case attachFrameError:
			if s.errorFrame != nil {
				return s.errorFrame(frame.payload)
			}
			return nil
		case attachFrameExit:
			return attachExitErrorFromPayload(s.kind, frame.payload)
		default:
			return fmt.Errorf("%s: unexpected frame type %d", s.action, frame.typ)
		}
	}
}

func (s *framedAttachSession) closeInput() error {
	return s.writeFrame(attachFrameCloseInput, nil)
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
			name, ok := signalName(sig)
			if !ok {
				continue
			}
			if err := s.writeFrame(attachFrameSignal, []byte(name)); err != nil {
				return err
			}
		}
	}
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
	return s.writeFrame(attachFrameResize, payload)
}

func (s *framedAttachSession) writeFrame(typ byte, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeTerminalFrame(s.conn, typ, payload)
}

func terminalSize(file *os.File) (cols, rows int, ok bool) {
	if file == nil || !term.IsTerminal(int(file.Fd())) {
		return 0, 0, false
	}
	cols, rows, err := term.GetSize(int(file.Fd()))
	return cols, rows, err == nil && cols > 0 && rows > 0
}

type terminalFrame struct {
	typ     byte
	payload []byte
}

func writeTerminalFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > attachFrameMaxPayload {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var header [5]byte
	header[0] = typ
	size := uint32(len(payload)) // #nosec G115 -- payload length is bounded by attachFrameMaxPayload.
	binary.BigEndian.PutUint32(header[1:], size)
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func readTerminalFrame(r io.Reader) (terminalFrame, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return terminalFrame{}, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > attachFrameMaxPayload {
		return terminalFrame{}, fmt.Errorf("frame payload too large: %d", size)
	}
	payload := make([]byte, int(size))
	if size > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return terminalFrame{}, err
		}
	}
	return terminalFrame{typ: header[0], payload: payload}, nil
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
