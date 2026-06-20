package sessions

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameInput, []byte("hello")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if frame.Type != FrameInput || string(frame.Payload) != "hello" {
		t.Fatalf("unexpected frame %#v", frame)
	}
}

func TestMergeConfigRejectsUnknownAgent(t *testing.T) {
	_, err := MergeConfig(DefaultAgents(), Config{Agents: []Agent{{ID: "unknown", Command: []string{"x"}}}})
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
}
