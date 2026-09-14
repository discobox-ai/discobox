package resume

import (
	"bytes"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/execstream/frame"
)

func TestServerAppliesRetransmittedActionExactlyOnce(t *testing.T) {
	server := NewServer()
	token := bytes.Repeat([]byte{0x42}, tokenSize)
	request, err := encodeSession(sessionRequest{token: token, firstAvailable: 1})
	if err != nil {
		t.Fatal(err)
	}
	receiver, position, err := accept(server, request)
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

	resumed, position, err := accept(server, request)
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

	missingHistory, err := encodeSession(sessionRequest{token: token, firstAvailable: 2, accepted: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := accept(server, missingHistory); !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown resumed session error = %v, want ErrRejected", err)
	}

	initial, err := encodeSession(sessionRequest{token: token, firstAvailable: 1})
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, err := accept(server, initial)
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
	request, err := encodeSession(sessionRequest{token: token, firstAvailable: 1})
	if err != nil {
		t.Fatal(err)
	}
	receiver, _, err := accept(server, request)
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

	_, position, err = accept(server, request)
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
		request, err := encodeSession(sessionRequest{token: token, firstAvailable: 1})
		if err != nil {
			t.Fatal(err)
		}
		receiver, _, err := accept(server, request)
		if err != nil {
			t.Fatal(err)
		}
		receivers = append(receivers, receiver)
	}

	newToken := bytes.Repeat([]byte{0xff}, tokenSize)
	newRequest, err := encodeSession(sessionRequest{token: newToken, firstAvailable: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := accept(server, newRequest); !errors.Is(err, ErrRejected) {
		t.Fatalf("session beyond active capacity error = %v, want ErrRejected", err)
	}

	receivers[0].Close()
	replacement, _, err := accept(server, newRequest)
	if err != nil {
		t.Fatalf("accept after inactive session: %v", err)
	}
	replacement.Close()

	oldRequest, err := encodeSession(sessionRequest{token: bytes.Repeat([]byte{1}, tokenSize), firstAvailable: 2, accepted: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := accept(server, oldRequest); !errors.Is(err, ErrRejected) {
		t.Fatalf("evicted session error = %v, want ErrRejected", err)
	}
}

// accept is Server.Accept with the SessionOK payload decoded to its position.
func accept(server *Server, payload []byte) (*Receiver, uint64, error) {
	receiver, established, err := server.Accept(payload)
	if err != nil {
		return nil, 0, err
	}
	position, host, err := decodeSessionOK(established)
	if err != nil {
		return nil, 0, err
	}
	if host != server.instance {
		return nil, 0, errors.New("SessionOK names another host instance")
	}
	return receiver, position, nil
}

func TestServerStartsClientOfReplacedHostAtItsAcceptedPosition(t *testing.T) {
	previous := NewServer()
	server := NewServer()
	if previous.instance == server.instance {
		t.Fatal("two host streams share an instance")
	}
	token := bytes.Repeat([]byte{0x33}, tokenSize)

	// The client had actions 1-3 acknowledged and 4-5 outstanding on the
	// previous process. None of them reached this one.
	request, err := encodeSession(sessionRequest{token: token, firstAvailable: 4, accepted: 5, instance: previous.instance})
	if err != nil {
		t.Fatal(err)
	}
	receiver, position, err := accept(server, request)
	if err != nil {
		t.Fatalf("accept client of replaced host: %v", err)
	}
	if position != 5 {
		t.Fatalf("session position = %d, want the client's accepted position 5", position)
	}

	var applied []byte
	apply := func(next frame.Frame) error {
		applied = append(applied, next.Payload...)
		return nil
	}
	stale, err := encodeAction(5, frame.Input, []byte("stale"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Apply(stale, apply); err != nil {
		t.Fatal(err)
	}
	next, err := encodeAction(6, frame.Input, []byte("fresh"))
	if err != nil {
		t.Fatal(err)
	}
	if position, err := receiver.Apply(next, apply); err != nil || position != 6 {
		t.Fatalf("apply after restart = (%d, %v), want (6, nil)", position, err)
	}
	if string(applied) != "fresh" {
		t.Fatalf("applied input = %q, want only the action accepted after the restart", applied)
	}

	// Unknown to this host and claiming to be its client is still unrecoverable.
	own, err := encodeSession(sessionRequest{token: bytes.Repeat([]byte{0x34}, tokenSize), firstAvailable: 2, accepted: 1, instance: server.instance})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := accept(server, own); !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown session of this host error = %v, want ErrRejected", err)
	}
}
