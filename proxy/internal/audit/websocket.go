package audit

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/wsrecord"
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
	if len(b) < 2 {
		return 0, nil
	}
	fin, compressed := b[0]&0x80 != 0, b[0]&0x40 != 0
	opcode := b[0] & 15
	if b[0]&0x30 != 0 || compressed && (!d.deflate || opcode != 1 && opcode != 2) {
		return 0, fmt.Errorf("unsupported WebSocket reserved bits")
	}
	masked := b[1]&0x80 != 0
	if masked != (d.direction == wsrecord.ClientToServer) {
		return 0, fmt.Errorf("invalid WebSocket masking")
	}
	n := uint64(b[1] & 127)
	head := 2
	switch n {
	case 126:
		if len(b) < 4 {
			return 0, nil
		}
		n = uint64(binary.BigEndian.Uint16(b[2:4]))
		head = 4
		if n < 126 {
			return 0, fmt.Errorf("invalid WebSocket length encoding")
		}
	case 127:
		if len(b) < 10 {
			return 0, nil
		}
		n = binary.BigEndian.Uint64(b[2:10])
		head = 10
		if n < 65536 || n>>63 != 0 {
			return 0, fmt.Errorf("invalid WebSocket length encoding")
		}
	}
	control := opcode >= 8
	switch opcode {
	case 0, 1, 2, 8, 9, 10:
	default:
		return 0, fmt.Errorf("unsupported WebSocket opcode")
	}
	if control && (!fin || n > 125 || compressed) {
		return 0, fmt.Errorf("invalid WebSocket control frame")
	}
	if n > wsrecord.MaxPayload {
		return 0, fmt.Errorf("WebSocket frame exceeds capture limit")
	}
	if masked {
		head += 4
	}
	if uint64(len(b)) < uint64(head)+n {
		return 0, nil
	}
	payload := b[head : head+int(n)]
	if masked {
		key := b[head-4 : head]
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	if control {
		if opcode == 8 && (len(payload) == 1 || len(payload) > 1 && !utf8.Valid(payload[2:])) {
			return 0, fmt.Errorf("invalid WebSocket close payload")
		}
		if err := d.emit(wsrecord.Record{Time: at, Direction: d.direction, Opcode: opcode, Payload: payload}); err != nil {
			return 0, err
		}
		return head + int(n), nil
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
		return head + int(n), nil
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
	return head + int(n), nil
}
