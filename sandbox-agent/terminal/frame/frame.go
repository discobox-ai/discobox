package frame

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

const (
	Output     byte = 1
	Input      byte = 2
	Resize     byte = 3
	Signal     byte = 4
	Error      byte = 5
	Exit       byte = 6
	CloseInput byte = 7
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
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
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
