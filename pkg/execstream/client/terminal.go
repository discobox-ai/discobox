package client

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/discobox-ai/discobox/pkg/execstream"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// Terminal presents a framed attach as the byte stream a terminal draws: Read
// yields the exec's output, Write sends input, Resize and Repaint send their
// frames. The attach ends with io.EOF when the exec exits cleanly, an
// ExitError carrying any other code, and any other error when it is lost.
//
// It is what every program that draws or relays an exec's screen reads: the
// console's panes, and the AHP hosts in the CLI and in the sandbox.
type Terminal struct {
	frames  execstream.Conn
	readBuf []byte // output left over from a partly consumed frame

	writeMu sync.Mutex
	ready   sync.Once
}

// NewTerminal reads and writes an exec's attach through frames.
func NewTerminal(frames execstream.Conn) *Terminal {
	return &Terminal{frames: frames}
}

func (t *Terminal) Read(p []byte) (int, error) {
	// The ready frame pairs with a replay the attach asked for: the remote must
	// not stream the scrollback until something is reading it. It is sent on
	// the first read rather than at open, because that is the moment there
	// demonstrably is one.
	var readyErr error
	t.ready.Do(func() { readyErr = t.frames.WriteFrame(frame.Ready, nil) })
	if readyErr != nil {
		return 0, readyErr
	}
	if len(t.readBuf) > 0 {
		n := copy(p, t.readBuf)
		t.readBuf = t.readBuf[n:]
		return n, nil
	}
	for {
		f, err := t.frames.ReadFrame()
		if err != nil {
			return 0, err
		}
		switch f.Type {
		// A terminal is one visual stream, so stderr is shown inline with
		// stdout the way a terminal shows it. (Terminals are TTY execs and
		// never send it; this keeps an attach to a pipe exec from dropping
		// the stream.)
		case frame.Stdout, frame.Stderr:
			if len(f.Payload) == 0 {
				continue
			}
			n := copy(p, f.Payload)
			if n < len(f.Payload) {
				t.readBuf = append([]byte(nil), f.Payload[n:]...)
			}
			return n, nil
		case frame.Exit:
			if err := ExitErrorFromPayload("terminal", f.Payload); err != nil {
				return 0, err
			}
			return 0, io.EOF
		case frame.Error:
			return 0, fmt.Errorf("terminal: %s", string(f.Payload))
		default:
			// Frames a terminal does not consume (ready, for one).
		}
	}
}

func (t *Terminal) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.frames.WriteFrame(frame.Input, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize sends the terminal's size; a size with no area is not sent.
func (t *Terminal) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	payload, err := json.Marshal(struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.frames.WriteFrame(frame.Resize, payload)
}

// Repaint asks the exec's screen to be drawn again in full.
func (t *Terminal) Repaint() error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.frames.WriteFrame(frame.Repaint, nil)
}

func (t *Terminal) Close() error { return t.frames.Close() }
