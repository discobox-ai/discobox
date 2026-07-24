package resume

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/obot-platform/discobox/execstream"
	"github.com/obot-platform/discobox/execstream/frame"
)

type pipeConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (c *pipeConn) ReadFrame() (frame.Frame, error) { return frame.Read(c.conn) }
func (c *pipeConn) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return frame.Write(c.conn, typ, payload)
}
func (c *pipeConn) Close() error { return c.conn.Close() }

func newConnPair(t *testing.T) (*pipeConn, *pipeConn) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return &pipeConn{conn: client}, &pipeConn{conn: server}
}

func newTCPConnPair(t *testing.T) (*pipeConn, *pipeConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	var server *net.TCPConn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("timed out accepting TCP test connection")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return &pipeConn{conn: client}, &pipeConn{conn: server}
}

func acceptSession(t *testing.T, conn execstream.Conn, position uint64) sessionRequest {
	t.Helper()
	next, err := conn.ReadFrame()
	if err != nil {
		t.Errorf("read session: %v", err)
		return sessionRequest{}
	}
	if next.Type != frame.Session {
		t.Errorf("session frame type = %d, want %d", next.Type, frame.Session)
		return sessionRequest{}
	}
	request, err := decodeSession(next.Payload)
	if err != nil {
		t.Errorf("decode session: %v", err)
		return sessionRequest{}
	}
	if err := conn.WriteFrame(frame.SessionOK, encodePosition(position)); err != nil {
		t.Errorf("write SessionOK: %v", err)
	}
	return request
}

func TestClientQueuesDisconnectedInputAndReplaysIt(t *testing.T) {
	firstClient, firstServer := newConnPair(t)
	secondClient, secondServer := newConnPair(t)
	go acceptSession(t, firstServer, 0)

	allowDial := make(chan struct{})
	reconnecting := make(chan struct{}, 1)
	client, err := New(t.Context(), firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			<-allowDial
			return secondClient, nil
		},
		Event: func(event Event) {
			if event.State == ConnectionReconnecting {
				select {
				case reconnecting <- struct{}{}:
				default:
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close() })

	result := make(chan frame.Frame, 1)
	errResult := make(chan error, 1)
	go func() {
		next, err := client.ReadFrame()
		if err != nil {
			errResult <- err
			return
		}
		result <- next
	}()
	_ = firstServer.Close()
	select {
	case <-reconnecting:
	case <-time.After(time.Second):
		t.Fatal("reconnect did not start")
	}
	if err := client.WriteFrame(frame.Input, []byte("preserved")); err != nil {
		t.Fatalf("write disconnected input: %v", err)
	}

	applied := make(chan action, 1)
	go func() {
		acceptSession(t, secondServer, 0)
		next, err := secondServer.ReadFrame()
		if err != nil {
			t.Errorf("read replayed action: %v", err)
			return
		}
		got, err := decodeAction(next.Payload)
		if err != nil {
			t.Errorf("decode replayed action: %v", err)
			return
		}
		applied <- got
		_ = secondServer.WriteFrame(frame.Ack, encodePosition(got.position))
		_ = secondServer.WriteFrame(frame.Stdout, []byte("reconnected"))
	}()
	close(allowDial)

	select {
	case err := <-errResult:
		t.Fatalf("ReadFrame: %v", err)
	case next := <-result:
		if next.Type != frame.Stdout || string(next.Payload) != "reconnected" {
			t.Fatalf("output = %#v, want reconnected stdout", next)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconnected output")
	}
	got := <-applied
	if got.frame.Type != frame.Input || string(got.frame.Payload) != "preserved" {
		t.Fatalf("replayed action = %#v", got)
	}
}

func TestClientUsesHostPositionWhenAcknowledgementWasLost(t *testing.T) {
	firstClient, firstServer := newConnPair(t)
	secondClient, secondServer := newConnPair(t)
	go func() {
		acceptSession(t, firstServer, 0)
		next, err := firstServer.ReadFrame()
		if err != nil {
			t.Errorf("read first action: %v", err)
			return
		}
		if _, err := decodeAction(next.Payload); err != nil {
			t.Errorf("decode first action: %v", err)
		}
		// The action was applied, but the transport dies before its Ack arrives.
		_ = firstServer.Close()
	}()

	client, err := New(t.Context(), firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			return secondClient, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close() })
	if err := client.WriteFrame(frame.Input, []byte("once")); err != nil {
		t.Fatal(err)
	}

	resent := make(chan bool, 1)
	go func() {
		request := acceptSession(t, secondServer, 1)
		if request.firstAvailable != 1 {
			t.Errorf("first available = %d, want 1 while Ack is missing", request.firstAvailable)
		}
		_ = secondServer.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err := secondServer.ReadFrame()
		resent <- err == nil
		_ = secondServer.conn.SetReadDeadline(time.Time{})
		_ = secondServer.WriteFrame(frame.Stdout, []byte("continued"))
	}()

	next, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if next.Type != frame.Stdout || string(next.Payload) != "continued" {
		t.Fatalf("output = %#v, want continued stdout", next)
	}
	if <-resent {
		t.Fatal("client retransmitted an action the host position already acknowledged")
	}
}

func TestClientPreservesOutputInterleavedBeforeReplayAcknowledgement(t *testing.T) {
	firstClient, firstServer := newConnPair(t)
	secondClient, secondServer := newConnPair(t)
	go acceptSession(t, firstServer, 0)

	allowDial := make(chan struct{})
	client, err := New(t.Context(), firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			<-allowDial
			return secondClient, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close() })

	client.invalidate(firstClient)
	if err := client.WriteFrame(frame.Input, []byte("echo")); err != nil {
		t.Fatal(err)
	}
	go func() {
		acceptSession(t, secondServer, 0)
		next, err := secondServer.ReadFrame()
		if err != nil {
			t.Errorf("read replayed action: %v", err)
			return
		}
		action, err := decodeAction(next.Payload)
		if err != nil {
			t.Errorf("decode replayed action: %v", err)
			return
		}
		// A fast process can produce output after applying the action but before
		// the host has written its acknowledgement.
		_ = secondServer.WriteFrame(frame.Stdout, []byte("echoed"))
		_ = secondServer.WriteFrame(frame.Ack, encodePosition(action.position))
	}()
	close(allowDial)

	next, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if next.Type != frame.Stdout || string(next.Payload) != "echoed" {
		t.Fatalf("interleaved output = %#v, want echoed stdout", next)
	}
}

func TestClientBackpressuresWhenPendingBufferIsFull(t *testing.T) {
	clientConn, serverConn := newConnPair(t)
	go acceptSession(t, serverConn, 0)
	client, err := New(t.Context(), clientConn, Options{MaxPendingBytes: actionHeaderLen + 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	firstRead := make(chan action, 1)
	go func() {
		next, err := serverConn.ReadFrame()
		if err != nil {
			t.Errorf("read first action: %v", err)
			return
		}
		got, err := decodeAction(next.Payload)
		if err != nil {
			t.Errorf("decode first action: %v", err)
			return
		}
		firstRead <- got
	}()
	if err := client.WriteFrame(frame.Input, []byte("a")); err != nil {
		t.Fatal(err)
	}
	first := <-firstRead

	secondDone := make(chan error, 1)
	go func() { secondDone <- client.WriteFrame(frame.Input, []byte("b")) }()
	select {
	case err := <-secondDone:
		t.Fatalf("second write returned before space was acknowledged: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	output := make(chan frame.Frame, 1)
	go func() {
		next, err := client.ReadFrame()
		if err == nil {
			output <- next
		}
	}()
	if err := serverConn.WriteFrame(frame.Ack, encodePosition(first.position)); err != nil {
		t.Fatal(err)
	}
	secondWire, err := serverConn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	second, err := decodeAction(secondWire.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(second.frame.Payload) != "b" {
		t.Fatalf("second input = %q, want b", second.frame.Payload)
	}
	if err := serverConn.WriteFrame(frame.Ack, encodePosition(second.position)); err != nil {
		t.Fatal(err)
	}
	if err := serverConn.WriteFrame(frame.Stdout, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	select {
	case next := <-output:
		if string(next.Payload) != "done" {
			t.Fatalf("output = %q, want done", next.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for output")
	}
}

func TestClientRestoresLatestResizeAndReady(t *testing.T) {
	firstClient, firstServer := newConnPair(t)
	secondClient, secondServer := newConnPair(t)
	initialStateRead := make(chan struct{})
	go func() {
		acceptSession(t, firstServer, 0)
		for range 2 {
			if _, err := firstServer.ReadFrame(); err != nil {
				t.Errorf("read initial state: %v", err)
				return
			}
		}
		close(initialStateRead)
	}()

	allowDial := make(chan struct{})
	client, err := New(t.Context(), firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			<-allowDial
			return secondClient, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close() })

	initialResize := []byte(`{"cols":80,"rows":24}`)
	if err := client.WriteFrame(frame.Resize, initialResize); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(frame.Ready, nil); err != nil {
		t.Fatal(err)
	}
	<-initialStateRead

	client.invalidate(firstClient)
	latestResize := []byte(`{"cols":120,"rows":40}`)
	if err := client.WriteFrame(frame.Resize, latestResize); err != nil {
		t.Fatal(err)
	}
	restored := make(chan []frame.Frame, 1)
	go func() {
		acceptSession(t, secondServer, 0)
		got := make([]frame.Frame, 0, 2)
		for range 2 {
			next, err := secondServer.ReadFrame()
			if err != nil {
				t.Errorf("read restored state: %v", err)
				return
			}
			got = append(got, next)
		}
		restored <- got
		_ = secondServer.WriteFrame(frame.Stdout, []byte("restored"))
	}()
	close(allowDial)

	next, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if string(next.Payload) != "restored" {
		t.Fatalf("output = %q, want restored", next.Payload)
	}
	got := <-restored
	if got[0].Type != frame.Resize || string(got[0].Payload) != string(latestResize) {
		t.Fatalf("restored resize = %#v, want latest resize", got[0])
	}
	if got[1].Type != frame.Ready {
		t.Fatalf("restored frame type = %d, want Ready", got[1].Type)
	}
}

func TestClientReplaysSignalAndCloseInput(t *testing.T) {
	firstClient, firstServer := newConnPair(t)
	secondClient, secondServer := newConnPair(t)
	go acceptSession(t, firstServer, 0)

	allowDial := make(chan struct{})
	client, err := New(t.Context(), firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			<-allowDial
			return secondClient, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close() })

	client.invalidate(firstClient)
	if err := client.WriteFrame(frame.Signal, []byte("TERM")); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(frame.CloseInput, nil); err != nil {
		t.Fatal(err)
	}
	applied := make(chan []frame.Frame, 1)
	go func() {
		acceptSession(t, secondServer, 0)
		got := make([]frame.Frame, 0, 2)
		for range 2 {
			next, err := secondServer.ReadFrame()
			if err != nil {
				t.Errorf("read replayed action: %v", err)
				return
			}
			action, err := decodeAction(next.Payload)
			if err != nil {
				t.Errorf("decode replayed action: %v", err)
				return
			}
			got = append(got, action.frame)
			if err := secondServer.WriteFrame(frame.Ack, encodePosition(action.position)); err != nil {
				t.Errorf("acknowledge replayed action: %v", err)
				return
			}
		}
		applied <- got
		_ = secondServer.WriteFrame(frame.Stdout, []byte("done"))
	}()
	close(allowDial)

	if _, err := client.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	got := <-applied
	if got[0].Type != frame.Signal || string(got[0].Payload) != "TERM" {
		t.Fatalf("first replayed action = %#v, want TERM signal", got[0])
	}
	if got[1].Type != frame.CloseInput {
		t.Fatalf("second replayed action type = %d, want CloseInput", got[1].Type)
	}
}

func TestClientReconnectsWhenPeerHalfClosesWrite(t *testing.T) {
	testClientReconnectsAfterHalfClose(t, func(_, server *pipeConn) error {
		return server.conn.(*net.TCPConn).CloseWrite()
	})
}

func TestClientReconnectsWhenLocalSocketWriteIsHalfClosed(t *testing.T) {
	testClientReconnectsAfterHalfClose(t, func(client, _ *pipeConn) error {
		return client.conn.(*net.TCPConn).CloseWrite()
	})
}

func testClientReconnectsAfterHalfClose(t *testing.T, halfClose func(client, server *pipeConn) error) {
	t.Helper()
	firstClient, firstServer := newTCPConnPair(t)
	secondClient, secondServer := newTCPConnPair(t)
	go acceptSession(t, firstServer, 0)

	allowDial := make(chan struct{})
	events := make(chan Event, 2)
	client, err := New(t.Context(), firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			<-allowDial
			return secondClient, nil
		},
		Event: func(event Event) {
			events <- event
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { _ = client.Close() })

	result := make(chan frame.Frame, 1)
	readErr := make(chan error, 1)
	go func() {
		next, err := client.ReadFrame()
		if err != nil {
			readErr <- err
			return
		}
		result <- next
	}()
	if err := halfClose(firstClient, firstServer); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(frame.Input, []byte("preserved")); err != nil {
		t.Fatalf("write after half-close: %v", err)
	}
	select {
	case event := <-events:
		if event.State != ConnectionReconnecting {
			t.Fatalf("first event = %q, want reconnecting", event.State)
		}
	case <-time.After(time.Second):
		t.Fatal("half-close did not trigger reconnect")
	}

	applied := make(chan action, 1)
	go func() {
		acceptSession(t, secondServer, 0)
		next, err := secondServer.ReadFrame()
		if err != nil {
			t.Errorf("read replayed action: %v", err)
			return
		}
		action, err := decodeAction(next.Payload)
		if err != nil {
			t.Errorf("decode replayed action: %v", err)
			return
		}
		applied <- action
		_ = secondServer.WriteFrame(frame.Ack, encodePosition(action.position))
		_ = secondServer.WriteFrame(frame.Stdout, []byte("continued"))
	}()
	close(allowDial)

	select {
	case next := <-result:
		if next.Type != frame.Stdout || string(next.Payload) != "continued" {
			t.Fatalf("output after half-close = %#v", next)
		}
	case err := <-readErr:
		t.Fatalf("read after half-close: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for output after half-close")
	}
	got := <-applied
	if got.frame.Type != frame.Input || string(got.frame.Payload) != "preserved" {
		t.Fatalf("replayed action = %#v, want preserved input", got)
	}
}

func TestNonReconnectingClientReturnsPhysicalReadFailure(t *testing.T) {
	clientConn, serverConn := newConnPair(t)
	go acceptSession(t, serverConn, 0)
	client, err := New(t.Context(), clientConn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = serverConn.Close()
	if _, err := client.ReadFrame(); err == nil {
		// net.Pipe may return either EOF or io.ErrClosedPipe. The important
		// property is that a non-reconnecting client terminates.
		t.Fatal("ReadFrame returned nil after physical close")
	}
}

func TestClientStopsReconnectingWhenRemoteIsDone(t *testing.T) {
	clientConn, serverConn := newConnPair(t)
	go acceptSession(t, serverConn, 0)
	dials := 0
	client, err := New(t.Context(), clientConn, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			dials++
			return nil, errors.New("unexpected dial")
		},
		Done: func(context.Context) (bool, error) {
			return true, io.EOF
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }
	_ = serverConn.Close()

	if _, err := client.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame error = %v, want EOF", err)
	}
	if dials != 0 {
		t.Fatalf("dial calls = %d, want 0", dials)
	}
}

func TestReconnectBackoffIsCapped(t *testing.T) {
	if got := reconnectBackoff(1); got != 100*time.Millisecond {
		t.Fatalf("first reconnect backoff = %s, want 100ms", got)
	}
	if got := reconnectBackoff(20); got != 5*time.Second {
		t.Fatalf("capped reconnect backoff = %s, want 5s", got)
	}
}

func TestClientRejectsMalformedAcknowledgement(t *testing.T) {
	clientConn, serverConn := newConnPair(t)
	go func() {
		acceptSession(t, serverConn, 0)
		_ = serverConn.WriteFrame(frame.Ack, []byte("bad"))
	}()
	client, err := New(t.Context(), clientConn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.ReadFrame(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("malformed acknowledgement error = %v, want ErrProtocol", err)
	}
}

func TestCloseInterruptsReconnectHandshake(t *testing.T) {
	firstClient, firstServer := newConnPair(t)
	secondClient, secondServer := newConnPair(t)
	go acceptSession(t, firstServer, 0)

	client, err := New(t.Context(), firstClient, Options{
		Dial: func(context.Context) (execstream.Conn, error) {
			return secondClient, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.backoff = func(int) time.Duration { return 0 }

	readDone := make(chan error, 1)
	go func() {
		_, err := client.ReadFrame()
		readDone <- err
	}()
	if err := firstServer.Close(); err != nil {
		t.Fatal(err)
	}

	sessionReceived := make(chan struct{})
	go func() {
		next, err := secondServer.ReadFrame()
		if err == nil && next.Type == frame.Session {
			close(sessionReceived)
		}
		// Leave the handshake unanswered until Close interrupts the connection.
	}()
	select {
	case <-sessionReceived:
	case <-time.After(time.Second):
		t.Fatal("reconnect handshake did not start")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("ReadFrame returned nil after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt reconnect handshake")
	}
}

func TestContextCancellationInterruptsInitialHandshake(t *testing.T) {
	clientConn, serverConn := newConnPair(t)
	ctx, cancel := context.WithCancel(t.Context())

	newDone := make(chan error, 1)
	go func() {
		_, err := New(ctx, clientConn, Options{})
		newDone <- err
	}()
	next, err := serverConn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if next.Type != frame.Session {
		t.Fatalf("initial frame type = %d, want %d", next.Type, frame.Session)
	}
	cancel()
	select {
	case err := <-newDone:
		if err == nil {
			t.Fatal("New returned nil after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not interrupt initial handshake")
	}
}
