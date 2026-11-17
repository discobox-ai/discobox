package wsframe

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncodeParseRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 125, 126, 65535, 65536} {
		payload := bytes.Repeat([]byte{'x'}, n)
		for _, mask := range []bool{false, true} {
			frame := Encode(Text, payload, mask)
			h, ok, err := ParseHeader(frame)
			if err != nil || !ok {
				t.Fatalf("n=%d mask=%v: %v %v", n, mask, ok, err)
			}
			if !h.Fin || h.Opcode != Text || h.Masked != mask || h.Length != uint64(n) || h.Size+n != len(frame) {
				t.Fatalf("n=%d mask=%v: header %+v", n, mask, h)
			}
			got := bytes.Clone(frame[h.Size:])
			Unmask(got, h.Mask)
			if mask && n > 0 && bytes.Equal(frame[h.Size:], payload) {
				t.Fatalf("n=%d: a masked payload went out as itself", n)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("n=%d mask=%v: payload changed", n, mask)
			}
		}
	}
	// A fresh key each time.
	if bytes.Equal(Encode(Text, []byte("same"), true), Encode(Text, []byte("same"), true)) {
		t.Fatal("two frames took the same mask key")
	}
}

func TestParseHeaderRefusals(t *testing.T) {
	for name, frame := range map[string][]byte{
		"long length in short form": {0x81, 126, 0, 5},
		"top bit":                   {0x81, 127, 0x80, 0, 0, 0, 0, 0, 0, 0},
		"8-byte under 64k":          {0x81, 127, 0, 0, 0, 0, 0, 0, 0xff, 0xff},
		"reserved opcode":           {0x83, 0},
		"fragmented ping":           {0x09, 0},
		"long ping":                 {0x89, 126, 0, 126},
	} {
		var e *Error
		if _, _, err := ParseHeader(frame); !errors.As(err, &e) || e.Code != CloseProtocolError {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, ok, err := ParseHeader([]byte{0x81, 0x85, 1, 2}); ok || err != nil {
		t.Fatalf("a partial mask key: %v %v", ok, err)
	}
}

func TestCloseFrame(t *testing.T) {
	long := string(bytes.Repeat([]byte("é"), 100))
	frame := CloseFrame(ClosePolicyViolation, long, false)
	h, _, _ := ParseHeader(frame)
	payload := frame[h.Size:]
	if len(payload) > 125 || ValidClose(payload) != nil || payload[0] != 0x03 || payload[1] != 0xf0 {
		t.Fatalf("close payload %q", payload)
	}
	if ValidClose([]byte{1}) == nil || ValidClose([]byte{3, 0xe8, 0xff}) == nil {
		t.Fatal("an invalid close payload passed")
	}
}

func TestTrackerBoundaries(t *testing.T) {
	stream := append(append(Encode(Text, []byte("hello"), false), Encode(Ping, nil, false)...), Encode(Binary, bytes.Repeat([]byte{1}, 300), false)...)
	var tr Tracker
	var cuts []int
	for off := 0; off < len(stream); {
		// One byte at a time, the worst a network can do.
		end := off + 1
		n := tr.Advance(stream[off:end])
		off += n
		if tr.AtBoundary() {
			cuts = append(cuts, off)
		}
	}
	want := []int{7, 9, len(stream)}
	if len(cuts) != len(want) || cuts[0] != want[0] || cuts[1] != want[1] || cuts[2] != want[2] {
		t.Fatalf("boundaries %v, want %v", cuts, want)
	}
	// Advance stops at the first boundary in a chunk.
	var whole Tracker
	if n := whole.Advance(stream); n != 7 || !whole.AtBoundary() {
		t.Fatalf("Advance = %d", n)
	}
}
