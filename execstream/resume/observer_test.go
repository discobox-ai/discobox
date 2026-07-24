package resume

import (
	"context"
	"testing"
	"time"

	"github.com/obot-platform/discobox/execstream"
	"github.com/obot-platform/discobox/execstream/frame"
)

func TestObserverAnnotatesActionLifecycle(t *testing.T) {
	clientConn, serverConn := newConnPair(t)
	go acceptSession(t, serverConn, 0)

	events := make(chan ActionEvent, 4)
	ctx := WithObserver(t.Context(), ObserverFunc(func(event ActionEvent) {
		events <- event
	}))
	client, err := New(ctx, clientConn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		next, readErr := serverConn.ReadFrame()
		if readErr != nil {
			t.Errorf("read action: %v", readErr)
			return
		}
		action, decodeErr := decodeAction(next.Payload)
		if decodeErr != nil {
			t.Errorf("decode action: %v", decodeErr)
			return
		}
		if writeErr := serverConn.WriteFrame(frame.Ack, encodePosition(action.position)); writeErr != nil {
			t.Errorf("write ack: %v", writeErr)
			return
		}
		if writeErr := serverConn.WriteFrame(frame.Stdout, []byte("echo")); writeErr != nil {
			t.Errorf("write output: %v", writeErr)
		}
	}()

	if err := client.WriteFrame(frame.Input, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	<-serverDone

	got := make([]ActionEvent, 0, 3)
	deadline := time.After(time.Second)
	for len(got) < 3 {
		select {
		case event := <-events:
			got = append(got, event)
		case <-deadline:
			t.Fatalf("events = %#v, want accepted, physical write, acknowledged", got)
		}
	}

	wantPhases := []ActionPhase{ActionAccepted, ActionPhysicalWrite, ActionAcknowledged}
	for i, event := range got {
		if event.Phase != wantPhases[i] {
			t.Fatalf("event %d phase = %q, want %q", i, event.Phase, wantPhases[i])
		}
		if event.Position != 1 || event.Type != frame.Input || event.PayloadBytes != 1 {
			t.Fatalf("event %d = %#v, want position 1 input with one payload byte", i, event)
		}
		if event.At.IsZero() {
			t.Fatalf("event %d has zero timestamp", i)
		}
	}
	if got[1].Duration < 0 {
		t.Fatalf("physical write duration = %s", got[1].Duration)
	}
	if got[2].PendingBytes != 0 {
		t.Fatalf("ack pending bytes = %d, want 0", got[2].PendingBytes)
	}
}

func TestObserverAnnotatesRetransmission(t *testing.T) {
	firstClient, firstServer := newConnPair(t)
	secondClient, secondServer := newConnPair(t)
	go acceptSession(t, firstServer, 0)

	events := make(chan ActionEvent, 8)
	ctx := WithObserver(t.Context(), ObserverFunc(func(event ActionEvent) {
		events <- event
	}))
	client, err := New(ctx, firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			return secondClient, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close() })

	firstRead := make(chan struct{})
	go func() {
		defer close(firstRead)
		next, readErr := firstServer.ReadFrame()
		if readErr != nil {
			t.Errorf("read first action: %v", readErr)
			return
		}
		if _, decodeErr := decodeAction(next.Payload); decodeErr != nil {
			t.Errorf("decode first action: %v", decodeErr)
		}
		_ = firstServer.Close()
	}()
	if err := client.WriteFrame(frame.Input, []byte("x")); err != nil {
		t.Fatal(err)
	}
	<-firstRead

	go func() {
		acceptSession(t, secondServer, 0)
		next, readErr := secondServer.ReadFrame()
		if readErr != nil {
			t.Errorf("read retransmitted action: %v", readErr)
			return
		}
		action, decodeErr := decodeAction(next.Payload)
		if decodeErr != nil {
			t.Errorf("decode retransmitted action: %v", decodeErr)
			return
		}
		if writeErr := secondServer.WriteFrame(frame.Ack, encodePosition(action.position)); writeErr != nil {
			t.Errorf("write ack: %v", writeErr)
			return
		}
		if writeErr := secondServer.WriteFrame(frame.Stdout, []byte("continued")); writeErr != nil {
			t.Errorf("write output: %v", writeErr)
		}
	}()

	next, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if next.Type != frame.Stdout || string(next.Payload) != "continued" {
		t.Fatalf("output = %#v, want continued stdout", next)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Phase != ActionRetransmitted {
				continue
			}
			if event.Position != 1 || event.Type != frame.Input || event.PayloadBytes != 1 || event.Err != nil {
				t.Fatalf("retransmission event = %#v", event)
			}
			if event.At.IsZero() || event.Duration < 0 {
				t.Fatalf("retransmission timing = %#v", event)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for retransmission annotation")
		}
	}
}
