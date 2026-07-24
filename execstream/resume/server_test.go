package resume

import (
	"bytes"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/execstream/frame"
)

func TestServerAppliesRetransmittedActionExactlyOnce(t *testing.T) {
	server := NewServer()
	token := bytes.Repeat([]byte{0x42}, tokenSize)
	request, err := encodeSession(token, 1)
	if err != nil {
		t.Fatal(err)
	}
	receiver, position, err := server.Accept(request)
	if err != nil {
		t.Fatal(err)
	}
	if position != 0 {
		t.Fatalf("initial position = %d, want 0", position)
	}

	wire, err := encodeAction(1, frame.Input, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	var applied []byte
	apply := func(next frame.Frame) error {
		applied = append(applied, next.Payload...)
		return nil
	}
	if position, err = receiver.Apply(wire, apply); err != nil || position != 1 {
		t.Fatalf("first apply = (%d, %v), want (1, nil)", position, err)
	}
	if position, err = receiver.Apply(wire, apply); err != nil || position != 1 {
		t.Fatalf("duplicate apply = (%d, %v), want (1, nil)", position, err)
	}
	if string(applied) != "a" {
		t.Fatalf("applied input = %q, want exactly one copy", applied)
	}

	resumed, position, err := server.Accept(request)
	if err != nil {
		t.Fatal(err)
	}
	if position != 1 {
		t.Fatalf("resumed position = %d, want 1", position)
	}
	if position, err = resumed.Apply(wire, apply); err != nil || position != 1 {
		t.Fatalf("resumed duplicate = (%d, %v), want (1, nil)", position, err)
	}
	if string(applied) != "a" {
		t.Fatalf("applied input after resume = %q, want exactly one copy", applied)
	}
}

func TestServerRejectsUnrecoverableOrOutOfOrderSession(t *testing.T) {
	server := NewServer()
	token := bytes.Repeat([]byte{0x24}, tokenSize)

	missingHistory, err := encodeSession(token, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.Accept(missingHistory); !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown resumed session error = %v, want ErrRejected", err)
	}

	initial, err := encodeSession(token, 1)
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, err := server.Accept(initial)
	if err != nil {
		t.Fatal(err)
	}
	gap, err := encodeAction(2, frame.Input, []byte("gap"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Apply(gap, nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("gap error = %v, want ErrProtocol", err)
	}
}

func TestServerDoesNotAcknowledgeFailedApplication(t *testing.T) {
	server := NewServer()
	token := bytes.Repeat([]byte{0x18}, tokenSize)
	request, err := encodeSession(token, 1)
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, err := server.Accept(request)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := encodeAction(1, frame.Input, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("stdin closed")
	position, err := receiver.Apply(wire, func(frame.Frame) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("apply error = %v, want %v", err, wantErr)
	}
	if position != 0 {
		t.Fatalf("failed action position = %d, want 0", position)
	}

	_, position, err = server.Accept(request)
	if err != nil {
		t.Fatal(err)
	}
	if position != 0 {
		t.Fatalf("resumed position after failed action = %d, want 0", position)
	}
}

func TestServerEvictsOnlyInactiveSessionsAtCapacity(t *testing.T) {
	server := NewServer()
	receivers := make([]*Receiver, 0, MaxSessions)
	for i := range MaxSessions {
		token := bytes.Repeat([]byte{byte(i + 1)}, tokenSize)
		request, err := encodeSession(token, 1)
		if err != nil {
			t.Fatal(err)
		}
		receiver, _, err := server.Accept(request)
		if err != nil {
			t.Fatal(err)
		}
		receivers = append(receivers, receiver)
	}

	newToken := bytes.Repeat([]byte{0xff}, tokenSize)
	newRequest, err := encodeSession(newToken, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.Accept(newRequest); !errors.Is(err, ErrRejected) {
		t.Fatalf("session beyond active capacity error = %v, want ErrRejected", err)
	}

	receivers[0].Close()
	replacement, _, err := server.Accept(newRequest)
	if err != nil {
		t.Fatalf("accept after inactive session: %v", err)
	}
	replacement.Close()

	oldRequest, err := encodeSession(bytes.Repeat([]byte{1}, tokenSize), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := server.Accept(oldRequest); !errors.Is(err, ErrRejected) {
		t.Fatalf("evicted session error = %v, want ErrRejected", err)
	}
}
