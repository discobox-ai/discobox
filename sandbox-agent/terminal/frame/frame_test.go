package frame

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Input, []byte("hello")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if got.Type != Input || string(got.Payload) != "hello" {
		t.Fatalf("unexpected frame %#v", got)
	}
}

func TestResizeRoundTrip(t *testing.T) {
	payload, err := EncodeResize(120, 40)
	if err != nil {
		t.Fatalf("encode resize: %v", err)
	}
	got, err := DecodeResize(payload)
	if err != nil {
		t.Fatalf("decode resize: %v", err)
	}
	if got.Cols != 120 || got.Rows != 40 {
		t.Fatalf("resize = %#v", got)
	}
}
