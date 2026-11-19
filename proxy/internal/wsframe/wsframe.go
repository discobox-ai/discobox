// Package wsframe reads and writes RFC 6455 WebSocket frames for the proxy's
// enforcement of credentials in messages (ADR 26-11-06-442 §3). What to do
// when a frame does not read is the caller's: enforcement closes the
// connection. The audit capture still reads frames with its own decoder,
// which stops recording while traffic goes on.
package wsframe

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

// Opcodes.
const (
	Continuation byte = 0
	Text         byte = 1
	Binary       byte = 2
	Close        byte = 8
	Ping         byte = 9
	Pong         byte = 10
)

// Close codes the proxy ends a connection with.
const (
	CloseProtocolError   uint16 = 1002
	CloseInvalidPayload  uint16 = 1007
	ClosePolicyViolation uint16 = 1008
	CloseTooBig          uint16 = 1009
	CloseInternalError   uint16 = 1011
)

// MaxHeader is the longest a frame header is: 2 bytes, an 8-byte length and a
// 4-byte mask key.
const MaxHeader = 14

// Header is one frame's header.
type Header struct {
	Fin    bool
	RSV    byte // the three reserved bits, high to low
	Opcode byte
	Masked bool
	Mask   [4]byte
	Length uint64
	// Size is how many bytes the header took.
	Size int
}

// Control reports whether the frame is a control frame.
func (h Header) Control() bool { return h.Opcode >= 8 }

// Error is a framing fault, and the close code it ends the connection with.
type Error struct {
	Code   uint16
	Reason string
}

func (e *Error) Error() string { return e.Reason }

func protocolError(format string, args ...any) *Error {
	return &Error{Code: CloseProtocolError, Reason: fmt.Sprintf(format, args...)}
}

// ParseHeader reads the header at the start of b. ok is false when b holds
// too little of it yet. It refuses a length not in its minimal encoding or
// with its top bit set, an opcode RFC 6455 reserves, and a control frame
// that is fragmented or longer than 125 bytes. Reserved bits and masking are
// the caller's to judge.
func ParseHeader(b []byte) (h Header, ok bool, err error) {
	if len(b) < 2 {
		return Header{}, false, nil
	}
	h.Fin = b[0]&0x80 != 0
	h.RSV = (b[0] >> 4) & 7
	h.Opcode = b[0] & 15
	h.Masked = b[1]&0x80 != 0
	n := uint64(b[1] & 127)
	size := 2
	switch n {
	case 126:
		if len(b) < 4 {
			return Header{}, false, nil
		}
		n = uint64(binary.BigEndian.Uint16(b[2:4]))
		size = 4
		if n < 126 {
			return Header{}, false, protocolError("a WebSocket frame length is not in its shortest form")
		}
	case 127:
		if len(b) < 10 {
			return Header{}, false, nil
		}
		n = binary.BigEndian.Uint64(b[2:10])
		size = 10
		if n < 1<<16 || n>>63 != 0 {
			return Header{}, false, protocolError("a WebSocket frame length is not in its shortest form")
		}
	}
	switch h.Opcode {
	case Continuation, Text, Binary, Close, Ping, Pong:
	default:
		return Header{}, false, protocolError("a WebSocket frame has the reserved opcode %d", h.Opcode)
	}
	if h.Control() && (!h.Fin || n > 125) {
		return Header{}, false, protocolError("a WebSocket control frame is fragmented or longer than 125 bytes")
	}
	if h.Masked {
		if len(b) < size+4 {
			return Header{}, false, nil
		}
		copy(h.Mask[:], b[size:size+4])
		size += 4
	}
	h.Length = n
	h.Size = size
	return h, true, nil
}

// Unmask applies a mask key to payload in place; applying it twice restores
// it.
func Unmask(payload []byte, key [4]byte) {
	for i := range payload {
		payload[i] ^= key[i%4]
	}
}

// Encode is one final frame of opcode carrying payload, masked with a fresh
// random key when mask is set, as a client's frames are.
func Encode(opcode byte, payload []byte, mask bool) []byte {
	n := len(payload)
	out := make([]byte, 0, MaxHeader+n)
	out = append(out, 0x80|opcode)
	maskBit := byte(0)
	if mask {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		out = append(out, maskBit|byte(n))
	case n < 1<<16:
		out = append(out, maskBit|126, byte(n>>8), byte(n))
	default:
		out = append(out, maskBit|127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	if !mask {
		return append(out, payload...)
	}
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic(fmt.Sprintf("wsframe: no randomness for a mask key: %v", err))
	}
	out = append(out, key[:]...)
	start := len(out)
	out = append(out, payload...)
	Unmask(out[start:], key)
	return out
}

// CloseFrame is a close frame with code and reason, the reason cut to fit a
// control frame on a rune boundary.
func CloseFrame(code uint16, reason string, mask bool) []byte {
	const room = 125 - 2
	if len(reason) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	payload := binary.BigEndian.AppendUint16(nil, code)
	payload = append(payload, reason...)
	return Encode(Close, payload, mask)
}

// ValidClose reports whether a close frame's payload is one RFC 6455 allows:
// empty, or a code and a UTF-8 reason.
func ValidClose(payload []byte) *Error {
	switch {
	case len(payload) == 0:
		return nil
	case len(payload) == 1:
		return protocolError("a WebSocket close frame has a one-byte payload")
	case !utf8.Valid(payload[2:]):
		return &Error{Code: CloseInvalidPayload, Reason: "a WebSocket close reason is not UTF-8"}
	}
	return nil
}

// Tracker follows the frame boundaries of one direction's bytes without
// holding them, so a frame can be added between two of them.
type Tracker struct {
	header    []byte
	remaining uint64
	broken    bool
}

// Advance takes the next bytes of the stream, and returns how many of them
// end at the first frame boundary they reach, or len(b) when they reach
// none. A stream it cannot read stops at no boundary again.
func (t *Tracker) Advance(b []byte) int {
	for i := 0; i < len(b); {
		if t.broken {
			return len(b)
		}
		if t.remaining > 0 {
			step := uint64(len(b) - i)
			if step > t.remaining {
				step = t.remaining
			}
			t.remaining -= step
			i += int(step)
			if t.remaining == 0 {
				return i
			}
			continue
		}
		t.header = append(t.header, b[i])
		i++
		h, ok, err := ParseHeader(t.header)
		if err != nil {
			t.broken = true
			return len(b)
		}
		if !ok {
			continue
		}
		t.header = t.header[:0]
		t.remaining = h.Length
		if t.remaining == 0 {
			return i
		}
	}
	return len(b)
}

// AtBoundary reports whether the bytes so far end a frame.
func (t *Tracker) AtBoundary() bool {
	return !t.broken && t.remaining == 0 && len(t.header) == 0
}
