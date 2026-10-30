package audit

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/wsrecord"
)

func wsFrame(opcode byte, fin, compressed, masked bool, payload []byte) []byte {
	b := []byte{opcode, 0}
	if fin {
		b[0] |= 128
	}
	if compressed {
		b[0] |= 64
	}
	n := len(payload)
	switch {
	case n < 126:
		b[1] = byte(n)
	case n <= 65535:
		b[1] = 126
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b[1] = 127
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	key := []byte{1, 2, 3, 4}
	if masked {
		b[1] |= 128
		b = append(b, key...)
	}
	for i, v := range payload {
		if masked {
			v ^= key[i%4]
		}
		b = append(b, v)
	}
	return b
}

func TestWebSocketMessagesAcrossReads(t *testing.T) {
	for _, n := range []int{0, 5, 125, 126, 65535, 65536} {
		for _, masked := range []bool{false, true} {
			var records []wsrecord.Record
			ds, err := websocketDecoders("", func(r wsrecord.Record) error {
				r.Payload = bytes.Clone(r.Payload)
				records = append(records, r)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			d := ds[1]
			if masked {
				d = ds[0]
			}
			payload := bytes.Repeat([]byte("x"), n)
			wire := append(wsFrame(1, false, false, masked, payload[:n/2]), wsFrame(9, true, false, masked, []byte("ping"))...)
			wire = append(wire, wsFrame(0, true, false, masked, payload[n/2:])...)
			wire = append(wire, wsFrame(2, true, false, masked, []byte{0, 255})...)
			for len(wire) > 0 {
				k := min(7, len(wire))
				d.feed(wire[:k], time.Unix(1, 0))
				wire = wire[k:]
			}
			if len(records) != 3 || records[0].Opcode != 9 || !bytes.Equal(records[1].Payload, payload) || !bytes.Equal(records[2].Payload, []byte{0, 255}) {
				t.Fatalf("n=%d masked=%v records=%v", n, masked, records)
			}
		}
	}
}

func TestWebSocketCompression(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		extensions := "permessage-deflate; client_max_window_bits=15; server_max_window_bits=10"
		if !takeover {
			extensions += "; client_no_context_takeover; server_no_context_takeover"
		}
		for _, masked := range []bool{true, false} {
			var records []wsrecord.Record
			ds, err := websocketDecoders(extensions, func(r wsrecord.Record) error {
				r.Payload = bytes.Clone(r.Payload)
				records = append(records, r)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			d := ds[1]
			if masked {
				d = ds[0]
			}
			var compressed bytes.Buffer
			compressor, _ := flate.NewWriter(&compressed, flate.BestCompression)
			defer compressor.Close()
			// The second message relies on the dictionary when takeover is enabled.
			message := bytes.Repeat([]byte("a long repeated message with some dictionary state!"), 100)
			for range 3 {
				compressed.Reset()
				if !takeover {
					compressor.Reset(&compressed)
				}
				_, _ = compressor.Write(message)
				_ = compressor.Flush()
				encoded := bytes.Clone(compressed.Bytes())
				encoded = encoded[:len(encoded)-4]
				mid := len(encoded) / 2
				wire := append(wsFrame(1, false, true, masked, encoded[:mid]), wsFrame(0, true, false, masked, encoded[mid:])...)
				for _, b := range wire {
					d.feed([]byte{b}, time.Now())
				}
			}
			if len(records) != 3 {
				t.Fatalf("records=%v", records)
			}
			for _, r := range records {
				if r.Opcode != 1 || !bytes.Equal(r.Payload, message) {
					t.Fatalf("takeover=%v mask=%v: opcode %d payload %.100q", takeover, masked, r.Opcode, r.Payload)
				}
			}
		}
	}
}

func TestWebSocketUnsupportedAndMalformed(t *testing.T) {
	for _, extension := range []string{"unknown", "permessage-deflate, unknown", "permessage-deflate; client_max_window_bits=7", "permessage-deflate; server_no_context_takeover=yes", "permessage-deflate; server_max_window_bits=15; server_max_window_bits=15"} {
		if _, err := websocketDecoders(extension, func(wsrecord.Record) error { return nil }); err == nil {
			t.Fatalf("accepted %s", extension)
		}
	}
	for _, wire := range [][]byte{
		wsFrame(0, true, false, true, nil), wsFrame(1, true, false, false, nil),
		wsFrame(9, false, false, true, nil), wsFrame(1, true, true, true, nil),
		wsFrame(3, true, false, true, nil), wsFrame(1, true, false, true, []byte{255}),
		append([]byte{0x81, 0xff}, binary.BigEndian.AppendUint64(nil, 1<<40)...),
	} {
		var got []wsrecord.Record
		ds, _ := websocketDecoders("", func(r wsrecord.Record) error { got = append(got, r); return nil })
		ds[0].feed(wire, time.Now())
		ds[0].feed(wsFrame(1, true, false, true, []byte("later")), time.Now())
		if len(got) != 1 || got[0].Opcode != wsrecord.CaptureError {
			t.Fatalf("wire %x records %v", wire, got)
		}
	}
}

func TestWebSocketSpoolRedactsAndTruncatesByMessage(t *testing.T) {
	recorder, dir := openSweptRecorder(t)
	recorder.ConfigureStreamSpool(filepath.Join(dir, "streams"), 64)
	recorder.SetRedactor(func(string) Redactor { return fixedRedactor{} })
	record, session, err := recorder.BeginUpgradeStream("sbx-1", "websocket", "")
	if err != nil {
		t.Fatal(err)
	}
	wire := append(wsFrame(1, false, false, true, []byte(redactSecret[:7])), wsFrame(0, true, false, true, []byte(redactSecret[7:]))...)
	for _, b := range wire {
		session.RecordChunk(StreamClientToServer, []byte{b})
	}
	for range 10 {
		session.RecordChunk(StreamServerToClient, wsFrame(1, true, false, false, []byte(strings.Repeat("x", 200))))
	}
	_ = session.Close()
	path := filepath.Join(dir, "streams", filepath.FromSlash(record.File))
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := wsrecord.ReadHeader(file); err != nil {
		t.Fatal(err)
	}
	first, err := wsrecord.Read(file)
	if err != nil || string(first.Payload) != "[REDACTED]" {
		t.Fatalf("record=%v err=%v", first, err)
	}
	recorder.ConfigureSpoolBudget(SpoolBudget{MaxBytes: 1000, HeadBytes: 300})
	if _, err := recorder.EnforceBudget(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _ = file.Seek(0, 0)
	if err := wsrecord.ReadHeader(file); err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		_, err := wsrecord.Read(file)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("kept %d messages, want 2", count)
	}
}

func TestWebSocketCaptureStopsAfterQueueLoss(t *testing.T) {
	// Start the consumer only after overflowing its queue, making the loss
	// deterministic. Frames following the gap must never become valid records.
	var file bytes.Buffer
	_ = wsrecord.WriteHeader(&file)
	s := newStreamSession(nopWriteCloser{&file}, 1, nil)
	s.websocket = true
	s.RecordChunk(StreamClientToServer, wsFrame(1, true, false, true, []byte("before")))
	s.RecordChunk(StreamClientToServer, wsFrame(1, true, false, true, []byte("lost")))
	s.RecordChunk(StreamServerToClient, wsFrame(1, true, false, false, []byte("after")))
	go s.run()
	_ = s.Close()
	r := bytes.NewReader(file.Bytes())
	if err := wsrecord.ReadHeader(r); err != nil {
		t.Fatal(err)
	}
	first, err := wsrecord.Read(r)
	if err != nil || string(first.Payload) != "before" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	for range 2 {
		got, err := wsrecord.Read(r)
		if err != nil || got.Opcode != wsrecord.CaptureError {
			t.Fatalf("gap=%v err=%v", got, err)
		}
	}
	if _, err := wsrecord.Read(r); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected record after loss: %v", err)
	}
	if s.DroppedChunks() != 2 {
		t.Fatalf("dropped=%d", s.DroppedChunks())
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestWebSocketExpandedMessageLimit(t *testing.T) {
	var compressed bytes.Buffer
	compressor, _ := flate.NewWriter(&compressed, flate.BestSpeed)
	_, _ = compressor.Write(bytes.Repeat([]byte("x"), wsrecord.MaxPayload+1))
	_ = compressor.Flush()
	encoded := compressed.Bytes()
	encoded = encoded[:len(encoded)-4]
	defer compressor.Close()
	var got wsrecord.Record
	ds, err := websocketDecoders("permessage-deflate", func(r wsrecord.Record) error { got = r; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ds[1].feed(wsFrame(1, true, true, false, encoded), time.Now())
	if got.Opcode != wsrecord.CaptureError || !strings.Contains(string(got.Payload), "expanded message exceeds") {
		t.Fatalf("record=%v", got)
	}
}

func TestWebSocketStorageFailureStopsBothDirections(t *testing.T) {
	writer := &failingStreamWriter{}
	s := newStreamSession(writer, 8, nil)
	s.websocket = true
	s.RecordChunk(StreamClientToServer, wsFrame(1, true, false, true, []byte("first")))
	s.RecordChunk(StreamServerToClient, wsFrame(1, true, false, false, []byte("second")))
	go s.run()
	_ = s.Close()
	if writer.calls != 1 {
		t.Fatalf("writes after storage failure: %d", writer.calls)
	}
	if s.DroppedChunks() != 2 || s.DroppedBytes() == 0 {
		t.Fatalf("loss not counted: %d chunks %d bytes", s.DroppedChunks(), s.DroppedBytes())
	}
}

type failingStreamWriter struct{ calls int }

func (w *failingStreamWriter) Write(p []byte) (int, error) {
	w.calls++
	return min(3, len(p)), io.ErrShortWrite
}
func (*failingStreamWriter) Close() error { return nil }
