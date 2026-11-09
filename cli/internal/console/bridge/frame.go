package bridge

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// The framing (ADR 26-11-07-289 §1): one pipe each way, each frame a kind
// (1 byte), a stream (u32), a length (u32), all big-endian, then the payload.
// KindJSON is a Message on stream 0; every other kind is an execstream frame
// type (pkg/execstream/frame) on a non-zero stream.

// KindJSON is a JSON message.
const KindJSON byte = 255

// MaxFrame is the largest payload either side accepts. Larger ends the
// bridge: the two sides no longer agree where a frame starts.
const MaxFrame = 16 << 20

// Frame is one frame.
type Frame struct {
	Kind    byte
	Stream  uint32
	Payload []byte
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (Frame, error) {
	var head [9]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{Kind: head[0], Stream: binary.BigEndian.Uint32(head[1:5])}
	size := binary.BigEndian.Uint32(head[5:9])
	if size > MaxFrame {
		return Frame{}, fmt.Errorf("a frame of %d bytes is over the bridge's %d", size, MaxFrame)
	}
	f.Payload = make([]byte, size)
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// Writer writes whole frames to one pipe, from any goroutine.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter writes frames to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write writes one frame, head and payload together.
func (w *Writer) Write(kind byte, stream uint32, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("a frame of %d bytes is over the bridge's %d", len(payload), MaxFrame)
	}
	buf := make([]byte, 9+len(payload))
	buf[0] = kind
	binary.BigEndian.PutUint32(buf[1:5], stream)
	binary.BigEndian.PutUint32(buf[5:9], uint32(len(payload)))
	copy(buf[9:], payload)
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.w.Write(buf)
	return err
}
