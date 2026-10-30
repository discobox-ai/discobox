// Package wsrecord defines the disk format shared by the proxy's WebSocket
// capture and the CLI's audit reader. It does not interpret application messages.
package wsrecord

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const Format = "discobox-websocket-messages-v1"
const MaxPayload = 16 << 20
const HeaderSize = 4
const RecordHeaderSize = 14
const (
	ClientToServer byte = 1
	ServerToClient byte = 2
	Text           byte = 1
	Binary         byte = 2
	Close          byte = 8
	Ping           byte = 9
	Pong           byte = 10
	// CaptureError is a recorder diagnostic, never a WebSocket opcode.
	CaptureError byte = 255
)

var magic = [4]byte{'D', 'B', 'W', '1'}

type Record struct {
	Time      time.Time
	Direction byte
	Opcode    byte
	Payload   []byte
}

func WriteHeader(w io.Writer) error {
	_, err := w.Write(magic[:])
	return err
}
func ReadHeader(r io.Reader) error {
	var got [4]byte
	if _, err := io.ReadFull(r, got[:]); err != nil {
		return err
	}
	if got != magic {
		return fmt.Errorf("invalid WebSocket recording header")
	}
	return nil
}
func Write(w io.Writer, record Record) error {
	if len(record.Payload) > MaxPayload {
		return fmt.Errorf("WebSocket recording payload exceeds %d bytes", MaxPayload)
	}
	var head [RecordHeaderSize]byte
	binary.BigEndian.PutUint64(head[:8], uint64(record.Time.UnixNano()))
	head[8], head[9] = record.Direction, record.Opcode
	binary.BigEndian.PutUint32(head[10:], uint32(len(record.Payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(record.Payload)
	return err
}
func Read(r io.Reader) (Record, error) {
	var head [RecordHeaderSize]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Record{}, err
	}
	n := binary.BigEndian.Uint32(head[10:])
	if n > MaxPayload {
		return Record{}, fmt.Errorf("WebSocket recording payload exceeds %d bytes", MaxPayload)
	}
	record := Record{Time: time.Unix(0, int64(binary.BigEndian.Uint64(head[:8]))).UTC(), Direction: head[8], Opcode: head[9], Payload: make([]byte, int(n))}
	_, err := io.ReadFull(r, record.Payload)
	if len(record.Payload) > 0 && errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return record, err
}

// Cut returns the end of the last whole record within head. A crash may leave
// a partial final record. No payload allocation is needed to find the boundary.
func Cut(r io.Reader, head int64) (int64, error) {
	if err := ReadHeader(r); err != nil {
		return 0, err
	}
	offset := int64(HeaderSize)
	for {
		var h [RecordHeaderSize]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return offset, nil
			}
			return offset, err
		}
		n := int64(binary.BigEndian.Uint32(h[10:]))
		if n > MaxPayload || offset+RecordHeaderSize+n > head {
			return offset, nil
		}
		if _, err := io.CopyN(io.Discard, r, n); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return offset, nil
			}
			return offset, err
		}
		offset += RecordHeaderSize + n
	}
}
