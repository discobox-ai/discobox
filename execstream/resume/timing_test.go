package resume

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obot-platform/discobox/execstream/frame"
)

func TestTimingReportsAppliedInputRoundTrip(t *testing.T) {
	clientConn, serverConn := newConnPair(t)
	go acceptSession(t, serverConn, 0)

	events := make(chan TimingEvent, 2)
	client, err := New(t.Context(), clientConn, Options{
		Timing: TimingOptions{
			Observe:           func(event TimingEvent) { events <- event },
			HeartbeatInterval: time.Hour,
			SlowAfter:         time.Nanosecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	go func() {
		next, readErr := serverConn.ReadFrame()
		if readErr != nil {
			return
		}
		action, decodeErr := decodeAction(next.Payload)
		if decodeErr != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
		_ = serverConn.WriteFrame(frame.Ack, encodePosition(action.position))
		_ = serverConn.WriteFrame(frame.Stdout, []byte("done"))
	}()

	if err := client.WriteFrame(frame.Input, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-events:
		if event.Source != TimingActionAcknowledgement || !event.Input() {
			t.Fatalf("event = %#v, want input acknowledgement", event)
		}
		if event.Position != 1 || event.PayloadBytes != 1 || event.PendingBytes != 0 {
			t.Fatalf("event action metadata = %#v", event)
		}
		if event.RoundTrip < 5*time.Millisecond {
			t.Fatalf("round trip = %s, want at least injected delay", event.RoundTrip)
		}
		if !event.Slow || event.Err != nil || event.At.IsZero() {
			t.Fatalf("event classification = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for action timing")
	}
}

type probePipeConn struct {
	*pipeConn
	delay  time.Duration
	err    error
	probes atomic.Int32
}

func (c *probePipeConn) Probe(ctx context.Context) error {
	c.probes.Add(1)
	timer := time.NewTimer(c.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return c.err
	}
}

func TestTimingReportsPhysicalHeartbeat(t *testing.T) {
	rawClient, serverConn := newConnPair(t)
	clientConn := &probePipeConn{pipeConn: rawClient, delay: 5 * time.Millisecond}
	go acceptSession(t, serverConn, 0)

	events := make(chan TimingEvent, 2)
	client, err := New(t.Context(), clientConn, Options{
		Timing: TimingOptions{
			Observe:           func(event TimingEvent) { events <- event },
			HeartbeatInterval: time.Millisecond,
			HeartbeatTimeout:  100 * time.Millisecond,
			SlowAfter:         time.Nanosecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case event := <-events:
		if event.Source != TimingHeartbeat || event.RoundTrip < 5*time.Millisecond {
			t.Fatalf("heartbeat event = %#v", event)
		}
		if !event.Slow || event.Err != nil || event.At.IsZero() {
			t.Fatalf("heartbeat classification = %#v", event)
		}
		if clientConn.probes.Load() == 0 {
			t.Fatal("physical connection was not probed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for heartbeat timing")
	}
}

func TestTimingReportsHeartbeatTimeout(t *testing.T) {
	rawClient, serverConn := newConnPair(t)
	clientConn := &probePipeConn{pipeConn: rawClient, delay: time.Hour}
	go acceptSession(t, serverConn, 0)

	events := make(chan TimingEvent, 2)
	client, err := New(t.Context(), clientConn, Options{
		Timing: TimingOptions{
			Observe:           func(event TimingEvent) { events <- event },
			HeartbeatInterval: time.Millisecond,
			HeartbeatTimeout:  5 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case event := <-events:
		if event.Source != TimingHeartbeat || !event.Slow {
			t.Fatalf("heartbeat event = %#v", event)
		}
		if !errors.Is(event.Err, context.DeadlineExceeded) {
			t.Fatalf("heartbeat error = %v, want deadline exceeded", event.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for heartbeat timeout")
	}
}

func TestTimingRejectsNegativeDurations(t *testing.T) {
	clientConn, _ := newConnPair(t)
	_, err := New(t.Context(), clientConn, Options{
		Timing: TimingOptions{
			Observe:           func(TimingEvent) {},
			HeartbeatInterval: -time.Second,
		},
	})
	if err == nil {
		t.Fatal("expected negative heartbeat interval to fail")
	}
}
