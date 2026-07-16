package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestReconnectingAttachFramesResumesReads(t *testing.T) {
	firstClient, firstServer := net.Pipe()
	secondClient, secondServer := net.Pipe()
	defer firstServer.Close()
	defer secondServer.Close()

	var mu sync.Mutex
	var events []attachConnectionState
	frames := newReconnectingAttachFrames(t.Context(), firstClient, func(context.Context) (io.ReadWriteCloser, error) {
		return secondClient, nil
	}, nil, func(event attachConnectionEvent) {
		mu.Lock()
		events = append(events, event.State)
		mu.Unlock()
	})
	frames.backoff = func(int) time.Duration { return 0 }
	defer frames.Close()

	result := make(chan terminalFrame, 1)
	errResult := make(chan error, 1)
	go func() {
		frame, err := frames.ReadFrame()
		if err != nil {
			errResult <- err
			return
		}
		result <- frame
	}()

	_ = firstServer.Close()
	go func() { _ = writeTerminalFrame(secondServer, attachFrameOutput, []byte("after reconnect")) }()

	select {
	case err := <-errResult:
		t.Fatalf("ReadFrame: %v", err)
	case frame := <-result:
		if frame.typ != attachFrameOutput || string(frame.payload) != "after reconnect" {
			t.Fatalf("frame = %#v, want output after reconnect", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected output")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != attachConnectionReconnecting || events[1] != attachConnectionReconnected {
		t.Fatalf("events = %v, want reconnecting, reconnected", events)
	}
}

func TestReconnectingAttachFramesDropsInputWhileDisconnected(t *testing.T) {
	firstClient, firstServer := net.Pipe()
	secondClient, secondServer := net.Pipe()
	defer firstServer.Close()
	defer secondServer.Close()

	allowDial := make(chan struct{})
	reconnecting := make(chan struct{}, 1)
	frames := newReconnectingAttachFrames(t.Context(), firstClient, func(context.Context) (io.ReadWriteCloser, error) {
		<-allowDial
		return secondClient, nil
	}, nil, func(event attachConnectionEvent) {
		if event.State == attachConnectionReconnecting {
			select {
			case reconnecting <- struct{}{}:
			default:
			}
		}
	})
	frames.backoff = func(int) time.Duration { return 0 }
	defer frames.Close()

	frames.invalidate(firstClient)
	go func() { _ = frames.reconnect(errors.New("connection lost")) }()
	select {
	case <-reconnecting:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not start")
	}

	if err := frames.WriteFrame(attachFrameInput, []byte("do not replay")); err != nil {
		t.Fatalf("WriteFrame while reconnecting: %v", err)
	}
	close(allowDial)

	deadline := time.Now().Add(100 * time.Millisecond)
	if err := secondServer.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := readTerminalFrame(secondServer); err == nil {
		t.Fatal("disconnected input was replayed after reconnect")
	}
}

func TestReconnectingAttachFramesRestoresResizeAndReady(t *testing.T) {
	firstClient, firstServer := net.Pipe()
	secondClient, secondServer := net.Pipe()
	defer firstServer.Close()
	defer secondServer.Close()

	frames := newReconnectingAttachFrames(t.Context(), firstClient, func(context.Context) (io.ReadWriteCloser, error) {
		return secondClient, nil
	}, nil, nil)
	frames.backoff = func(int) time.Duration { return 0 }
	defer frames.Close()

	initialRead := make(chan struct{})
	go func() {
		_, _ = readTerminalFrame(firstServer)
		_, _ = readTerminalFrame(firstServer)
		close(initialRead)
	}()
	resize := []byte(`{"cols":80,"rows":24}`)
	if err := frames.WriteFrame(attachFrameResize, resize); err != nil {
		t.Fatalf("initial resize: %v", err)
	}
	if err := frames.WriteFrame(attachFrameReady, nil); err != nil {
		t.Fatalf("initial ready: %v", err)
	}
	<-initialRead

	restored := make(chan []terminalFrame, 1)
	go func() {
		got := make([]terminalFrame, 0, 2)
		for range 2 {
			frame, _ := readTerminalFrame(secondServer)
			got = append(got, frame)
		}
		restored <- got
		_ = writeTerminalFrame(secondServer, attachFrameOutput, []byte("repainted"))
	}()

	_ = firstServer.Close()
	frame, err := frames.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(frame.payload) != "repainted" {
		t.Fatalf("output = %q, want repainted", frame.payload)
	}
	got := <-restored
	if got[0].typ != attachFrameResize || string(got[0].payload) != string(resize) {
		t.Fatalf("first restored frame = %#v, want resize", got[0])
	}
	if got[1].typ != attachFrameReady {
		t.Fatalf("second restored frame type = %d, want ready", got[1].typ)
	}
}

func TestAttachReconnectBackoffIsCapped(t *testing.T) {
	if got := attachReconnectBackoff(1); got != 100*time.Millisecond {
		t.Fatalf("attempt 1 backoff = %s, want 100ms", got)
	}
	if got := attachReconnectBackoff(20); got != 5*time.Second {
		t.Fatalf("attempt 20 backoff = %s, want 5s", got)
	}
}

func TestReconnectingAttachFramesStopsWhenTerminalEnded(t *testing.T) {
	client, server := net.Pipe()
	dials := 0
	frames := newReconnectingAttachFrames(t.Context(), client, func(context.Context) (io.ReadWriteCloser, error) {
		dials++
		return nil, errors.New("should not dial")
	}, func(context.Context) (bool, error) {
		return true, nil
	}, nil)
	defer frames.Close()
	_ = server.Close()

	if _, err := frames.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame error = %v, want EOF", err)
	}
	if dials != 0 {
		t.Fatalf("dial calls = %d, want 0", dials)
	}
}
