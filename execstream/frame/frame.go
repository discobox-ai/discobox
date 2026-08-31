package frame

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Frame types. The three stream frames take the file descriptor numbers they
// carry — stdin 0, stdout 1, stderr 2 — so the wire reads like the process it
// proxies; control frames follow.
//
// stdout and stderr are always distinct frames. A client that wants them
// interleaved can merge them itself, which is the direction that loses no
// information; the shim merging them first is irreversible. A TTY exec simply
// never emits Stderr: the kernel already merged both onto the PTY, and there is
// nothing for the client to tell apart. Nothing on the wire distinguishes "a
// TTY exec" from "a pipe exec that wrote nothing to stderr", and nothing should.
const (
	Input      byte = 0
	Stdout     byte = 1
	Stderr     byte = 2
	Resize     byte = 3
	Signal     byte = 4
	Error      byte = 5
	Exit       byte = 6
	CloseInput byte = 7
	// Ready is sent by an attaching client once its output read loop is active,
	// signaling the shim that the full attach tunnel is established end to end.
	// The shim withholds replay history until it arrives so no history bytes are
	// written into the upgrade-handshake window, where an intermediate proxy hop
	// can drop bytes buffered before its tunnel is wired up.
	Ready byte = 8
	// Session opens or resumes one logical client session over a physical attach
	// connection. SessionOK reports the highest action position the host applied.
	// Action carries a positioned Input, Signal, or CloseInput frame, and Ack
	// cumulatively acknowledges applied actions. Together these frames let a
	// client reconnect and retransmit without losing or duplicating process input.
	Session   byte = 9
	SessionOK byte = 10
	Action    byte = 11
	Ack       byte = 12
	// CloseOutput is CloseInput's mirror: the far end of a byte tunnel is done
	// sending and is still able to receive. A TCP tunnel needs both halves —
	// the side that closes first is whichever end of the forwarded connection
	// finishes first, and closing the whole tunnel for it cuts off data still
	// traveling the other way. An exec has no use for it: a process that
	// closes its output is a process that has exited, which Exit already says.
	CloseOutput byte = 13
)

const maxPayload = 16 * 1024 * 1024

type Frame struct {
	Type    byte
	Payload []byte
}

type ResizePayload struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type ExitPayload struct {
	Status   string `json:"status"`
	ExitCode *int64 `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

func Write(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > maxPayload {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var header [5]byte
	header[0] = typ
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if err := writeFull(w, header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	return writeFull(w, payload)
}

func writeFull(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n > 0 {
			payload = payload[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func Read(r io.Reader) (Frame, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maxPayload {
		return Frame{}, fmt.Errorf("frame payload too large: %d", size)
	}
	payload := make([]byte, int(size))
	if size > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}
	return Frame{Type: header[0], Payload: payload}, nil
}

func EncodeResize(cols, rows uint16) ([]byte, error) {
	if cols == 0 || rows == 0 {
		return nil, fmt.Errorf("rows and cols are required")
	}
	return json.Marshal(ResizePayload{Cols: cols, Rows: rows})
}

func DecodeResize(payload []byte) (ResizePayload, error) {
	var req ResizePayload
	if err := json.Unmarshal(payload, &req); err != nil {
		return ResizePayload{}, err
	}
	if req.Cols == 0 || req.Rows == 0 {
		return ResizePayload{}, fmt.Errorf("rows and cols are required")
	}
	return req, nil
}

func EncodeExit(status string, exitCode *int64, message string) ([]byte, error) {
	return json.Marshal(ExitPayload{
		Status:   status,
		ExitCode: exitCode,
		Error:    message,
	})
}

func DecodeExit(payload []byte) (ExitPayload, error) {
	var req ExitPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		return ExitPayload{}, err
	}
	return req, nil
}
