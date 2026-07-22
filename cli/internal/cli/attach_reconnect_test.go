package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/obot-platform/discobox/execstream/frame"
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

	result := make(chan frame.Frame, 1)
	errResult := make(chan error, 1)
	go func() {
		f, err := frames.ReadFrame()
		if err != nil {
			errResult <- err
			return
		}
		result <- f
	}()

	_ = firstServer.Close()
	go func() { _ = frame.Write(secondServer, frame.Stdout, []byte("after reconnect")) }()

	select {
	case err := <-errResult:
		t.Fatalf("ReadFrame: %v", err)
	case f := <-result:
		if f.Type != frame.Stdout || string(f.Payload) != "after reconnect" {
			t.Fatalf("frame = %#v, want output after reconnect", f)
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

	if err := frames.WriteFrame(frame.Input, []byte("do not replay")); err != nil {
		t.Fatalf("WriteFrame while reconnecting: %v", err)
	}
	close(allowDial)

	deadline := time.Now().Add(100 * time.Millisecond)
	if err := secondServer.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := frame.Read(secondServer); err == nil {
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
		_, _ = frame.Read(firstServer)
		_, _ = frame.Read(firstServer)
		close(initialRead)
	}()
	resize := []byte(`{"cols":80,"rows":24}`)
	if err := frames.WriteFrame(frame.Resize, resize); err != nil {
		t.Fatalf("initial resize: %v", err)
	}
	if err := frames.WriteFrame(frame.Ready, nil); err != nil {
		t.Fatalf("initial ready: %v", err)
	}
	<-initialRead

	restored := make(chan []frame.Frame, 1)
	go func() {
		got := make([]frame.Frame, 0, 2)
		for range 2 {
			frame, _ := frame.Read(secondServer)
			got = append(got, frame)
		}
		restored <- got
		_ = frame.Write(secondServer, frame.Stdout, []byte("repainted"))
	}()

	_ = firstServer.Close()
	f, err := frames.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(f.Payload) != "repainted" {
		t.Fatalf("output = %q, want repainted", f.Payload)
	}
	got := <-restored
	if got[0].Type != frame.Resize || string(got[0].Payload) != string(resize) {
		t.Fatalf("first restored frame = %#v, want resize", got[0])
	}
	if got[1].Type != frame.Ready {
		t.Fatalf("second restored frame type = %d, want ready", got[1].Type)
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
