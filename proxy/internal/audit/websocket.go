package audit

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/wsrecord"
	"github.com/discobox-ai/discobox/proxy/internal/wsframe"
)

// websocketDecoder observes one direction. It never writes to the connection.
// Both compressed input and expanded output are bounded independently.
type websocketDecoder struct {
	direction          byte
	deflate, noContext bool
	frame              []byte
	message            []byte
	opcode             byte
	compressed         bool
	dictionary         []byte
	stopped            bool
	emit               func(wsrecord.Record) error
}

func websocketDecoders(extensions string, emit func(wsrecord.Record) error) ([2]*websocketDecoder, error) {
	decoders := [2]*websocketDecoder{{direction: wsrecord.ClientToServer, emit: emit}, {direction: wsrecord.ServerToClient, emit: emit}}
	if strings.TrimSpace(extensions) == "" {
		return decoders, nil
	}
	parts := strings.Split(extensions, ";")
	if !strings.EqualFold(strings.TrimSpace(parts[0]), "permessage-deflate") {
		return decoders, fmt.Errorf("unsupported WebSocket extension")
	}
	seen := map[string]bool{}
	for _, part := range parts[1:] {
		key, value, hasValue := strings.Cut(strings.TrimSpace(part), "=")
		key = strings.ToLower(key)
		if seen[key] {
			return decoders, fmt.Errorf("duplicate WebSocket compression parameter")
		}
		seen[key] = true
		switch key {
		case "client_no_context_takeover", "server_no_context_takeover":
			if hasValue {
				return decoders, fmt.Errorf("invalid WebSocket compression parameter")
			}
			decoders[0].noContext = decoders[0].noContext || key == "client_no_context_takeover"
			decoders[1].noContext = decoders[1].noContext || key == "server_no_context_takeover"
		case "client_max_window_bits", "server_max_window_bits":
			n, err := strconv.Atoi(strings.Trim(value, "\""))
			if !hasValue || err != nil || n < 8 || n > 15 {
				return decoders, fmt.Errorf("invalid WebSocket compression window")
			}
		default:
			return decoders, fmt.Errorf("unsupported WebSocket compression parameter")
		}
	}
	for _, d := range decoders {
		d.deflate = true
	}
	return decoders, nil
}

func (d *websocketDecoder) fail(at time.Time, reason string) {
	if d.stopped {
		return
	}
	d.stopped = true
	d.frame, d.message, d.dictionary = nil, nil, nil
	_ = d.emit(wsrecord.Record{Time: at, Direction: d.direction, Opcode: wsrecord.CaptureError, Payload: []byte(reason)})
}

func (d *websocketDecoder) feed(p []byte, at time.Time) {
	if d.stopped {
		return
	}
	// Parse between bounded appends, so one large network read containing many
	// small messages does not need one correspondingly large capture buffer.
	for len(p) > 0 && !d.stopped {
		n := min(len(p), 32<<10)
		d.frame = append(d.frame, p[:n]...)
		p = p[n:]
		for !d.stopped {
			consumed, err := d.parse(at)
			if err != nil {
				d.fail(at, err.Error())
				break
			}
			if consumed == 0 {
				break
			}
			d.frame = d.frame[consumed:]
		}
	}
}

func (d *websocketDecoder) parse(at time.Time) (int, error) {
	b := d.frame
	h, ok, err := wsframe.ParseHeader(b)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	compressed := h.RSV&4 != 0
	if h.RSV&3 != 0 || compressed && (!d.deflate || h.Opcode != wsframe.Text && h.Opcode != wsframe.Binary) {
		return 0, fmt.Errorf("unsupported WebSocket reserved bits")
	}
	if h.Masked != (d.direction == wsrecord.ClientToServer) {
		return 0, fmt.Errorf("invalid WebSocket masking")
	}
	if h.Length > wsrecord.MaxPayload {
		return 0, fmt.Errorf("WebSocket frame exceeds capture limit")
	}
	end := h.Size + int(h.Length)
	if len(b) < end {
		return 0, nil
	}
	payload := b[h.Size:end]
	if h.Masked {
		wsframe.Unmask(payload, h.Mask)
	}
	opcode, fin := h.Opcode, h.Fin
	if h.Control() {
		if opcode == wsframe.Close {
			if err := wsframe.ValidClose(payload); err != nil {
				return 0, err
			}
		}
		if err := d.emit(wsrecord.Record{Time: at, Direction: d.direction, Opcode: opcode, Payload: payload}); err != nil {
			return 0, err
		}
		return end, nil
	}
	if opcode == 0 {
		if d.opcode == 0 {
			return 0, fmt.Errorf("unexpected WebSocket continuation")
		}
	} else {
		if d.opcode != 0 {
			return 0, fmt.Errorf("WebSocket message interrupted by another message")
		}
		d.opcode, d.compressed = opcode, compressed
	}
	if len(d.message)+len(payload) > wsrecord.MaxPayload {
		return 0, fmt.Errorf("WebSocket message exceeds capture limit")
	}
	d.message = append(d.message, payload...)
	if !fin {
		return end, nil
	}
	message := d.message
	if d.compressed {
		// RFC 7692 strips the sync-flush trailer. Restore it and a final empty
		// block so the standard inflater can distinguish valid EOF from damage.
		input := io.MultiReader(bytes.NewReader(message), bytes.NewReader([]byte{0, 0, 255, 255, 1, 0, 0, 255, 255}))
		inflater := flate.NewReaderDict(input, d.dictionary)
		expanded, err := io.ReadAll(io.LimitReader(inflater, wsrecord.MaxPayload+1))
		_ = inflater.Close()
		if err != nil {
			return 0, fmt.Errorf("invalid WebSocket compressed message")
		}
		if len(expanded) > wsrecord.MaxPayload {
			return 0, fmt.Errorf("WebSocket expanded message exceeds capture limit")
		}
		message = expanded
		if !d.noContext {
			d.dictionary = append(d.dictionary, message...)
			if len(d.dictionary) > 32768 {
				d.dictionary = bytes.Clone(d.dictionary[len(d.dictionary)-32768:])
			}
		}
	}
	if d.opcode == wsrecord.Text && !utf8.Valid(message) {
		return 0, fmt.Errorf("invalid WebSocket text encoding")
	}
	if err := d.emit(wsrecord.Record{Time: at, Direction: d.direction, Opcode: d.opcode, Payload: message}); err != nil {
		return 0, err
	}
	d.message = nil
	d.opcode = 0
	return end, nil
}
