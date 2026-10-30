package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/discobox-ai/discobox/pkg/wsrecord"
)

// renderWebSocketRecording streams complete messages to the existing audit
// recording card. It never infers requests, responses, or application events.
func renderWebSocketRecording(out io.Writer, r io.Reader) error {
	if err := wsrecord.ReadHeader(r); err != nil {
		return fmt.Errorf("read WebSocket recording: %w", err)
	}
	for {
		record, err := wsrecord.Read(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			_, err = fmt.Fprintln(out, "[capture truncated during a message record]")
			return err
		}
		if err != nil {
			return fmt.Errorf("read WebSocket recording: %w", err)
		}
		direction := "sent"
		if record.Direction == wsrecord.ServerToClient {
			direction = "received"
		}
		kind := map[byte]string{wsrecord.Text: "text", wsrecord.Binary: "binary", wsrecord.Close: "close", wsrecord.Ping: "ping", wsrecord.Pong: "pong", wsrecord.CaptureError: "capture error"}[record.Opcode]
		if kind == "" {
			kind = "unknown"
		}
		if _, err = fmt.Fprintf(out, "%s  %s  %s  (%d bytes)\n", record.Time.Format(time.RFC3339Nano), direction, kind, len(record.Payload)); err != nil {
			return err
		}
		var payload string
		switch record.Opcode {
		case wsrecord.Binary, wsrecord.Close, wsrecord.Ping, wsrecord.Pong:
			payload = hex.EncodeToString(record.Payload)
		default:
			// Keep JSON in its original layout: indenting arbitrary JSON can expand
			// quadratically with depth. The card already caps rendered output.
			payload = terminalSafeMultiline(string(record.Payload))
		}
		if _, err = fmt.Fprintf(out, "%s\n\n", payload); err != nil {
			return err
		}
	}
}
