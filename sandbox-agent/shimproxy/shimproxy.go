package shimproxy

import (
	"errors"

	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"

	"github.com/coder/websocket"
)

const defaultDialTimeout = 5 * time.Second

// statusTimeout bounds the entire status exchange, including response reads.
// Status reads are best-effort
// refresh inputs on hot paths (exec list/get), so an unreachable shim must fail
// fast rather than ride the generous retry used for start/attach.
const statusTimeout = 250 * time.Millisecond

// StatusJSON reads the shim's authoritative view of its exec. While the shim is
// alive it is the source of truth for the command's status: the systemd unit
// only mirrors the shim process, which deliberately outlives the command (it
// lingers so late attachers can replay output), so unit liveness says nothing
// about command liveness.
func StatusJSON[T any](ctx context.Context, socketPath string) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	var zero T
	shimConn, err := Dial(ctx, socketPath, statusTimeout)
	if err != nil {
		return zero, err
	}
	defer shimConn.Close()
	deadline, _ := ctx.Deadline()
	if err := shimConn.SetDeadline(deadline); err != nil {
		return zero, err
	}
	stop := context.AfterFunc(ctx, func() { _ = shimConn.Close() })
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/status", nil)
	if err != nil {
		return zero, err
	}
	if err := req.Write(shimConn); err != nil {
		return zero, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(shimConn), req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return zero, fmt.Errorf("shim status: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}

func StartJSON[T any](ctx context.Context, socketPath string) (T, error) {
	var zero T
	shimConn, err := Dial(ctx, socketPath, defaultDialTimeout)
	if err != nil {
		return zero, err
	}
	defer shimConn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/start", nil)
	if err != nil {
		return zero, err
	}
	if err := req.Write(shimConn); err != nil {
		return zero, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(shimConn), req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return zero, fmt.Errorf("start shim: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}

// ShimError is a shim answering a call with an error status.
type ShimError struct {
	Status  int
	Message string
}

func (e *ShimError) Error() string {
	return fmt.Sprintf("shim: %s: %s", http.StatusText(e.Status), e.Message)
}

// CallJSON makes one request of a shim's socket with an optional JSON body and
// decodes a JSON answer into T. A 204 decodes nothing. An error status comes
// back as a *ShimError, so a caller can tell a refusal from a shim that is gone.
func CallJSON[T any](ctx context.Context, socketPath, method, path string, body any) (T, error) {
	var zero T
	shimConn, err := Dial(ctx, socketPath, defaultDialTimeout)
	if err != nil {
		return zero, err
	}
	defer shimConn.Close()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reader)
	if err != nil {
		return zero, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The dial's deadline covers connecting; a wait holds the connection far
	// longer, and ctx is what bounds it.
	_ = shimConn.SetDeadline(time.Time{})
	stop := context.AfterFunc(ctx, func() { _ = shimConn.Close() })
	defer stop()
	if err := req.Write(shimConn); err != nil {
		return zero, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(shimConn), req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return zero, &ShimError{Status: resp.StatusCode, Message: strings.TrimSpace(string(message))}
	}
	var out T
	if resp.StatusCode == http.StatusNoContent {
		return out, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}

func AttachHTTPUpgrade(ctx context.Context, w http.ResponseWriter, socketPath, protocol string, replay bool) error {
	shimConn, shimReader, err := attachShim(ctx, socketPath, protocol, replay)
	if err != nil {
		return err
	}
	defer shimConn.Close()

	clientConn, clientRW, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return err
	}
	defer clientConn.Close()
	// The http.Server's per-request read/write deadlines survive the hijack and
	// would kill this long-lived attach stream mid-session; the attach owns the
	// conn from here on, so clear them.
	_ = clientConn.SetDeadline(time.Time{})
	_, _ = clientRW.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + protocol + "\r\n\r\n")
	if err := clientRW.Flush(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(shimConn, clientRW)
		closeWrite(shimConn)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, shimReader)
		closeWrite(clientConn)
	}()
	wg.Wait()
	return nil
}

// AttachWebSocket bridges an exec attach to a websocket client, pinging the
// client every pingInterval to keep an idle tunnel alive across NATs and
// intermediate proxies and to find a client that is gone, which an idle
// raw-byte tunnel never would. A ping is judged after pingTimeout (see
// keepalive).
func AttachWebSocket(ctx context.Context, w http.ResponseWriter, r *http.Request, socketPath, protocol string, replay bool, pingInterval, pingTimeout time.Duration) error {
	shimConn, shimReader, err := attachShim(ctx, socketPath, protocol, replay)
	if err != nil {
		return err
	}
	defer shimConn.Close()

	clientConn := newWatchedConn()
	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The websocket gives a pong at most five seconds to take its writer
		// and be written, and closes the connection when one taken stalls
		// that long. While an output write is pending the writer is held
		// behind a client that is not reading, so a pong could only fail;
		// and a client that is not reading counts no pong missing.
		OnPingReceived: func(context.Context, []byte) bool { return clientConn.writing.Load() == 0 },
	})
	if err != nil {
		return err
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	clientConn.Conn = websocket.NetConn(ctx, wsConn, websocket.MessageBinary)
	defer clientConn.Close()

	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go func() {
		if err := keepalive(pingCtx, wsConn, clientConn, pingInterval, pingTimeout); err != nil {
			// The peer is gone; close the tunnel so both copy loops unblock.
			_ = clientConn.Close()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(shimConn, clientConn)
		closeWrite(shimConn)
		// Nothing reads the client from here on, so no pong would ever be
		// read and the keepalive could not tell a live client from a dead
		// one: a websocket whose reading has ended (the client closed it, or
		// a read failed) ends the attach.
		_ = clientConn.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, shimReader)
		_ = clientConn.Close()
	}()
	wg.Wait()
	return nil
}

// watchedConn is an attach's client end, recording what the keepalive needs to
// judge an unanswered ping: whether a read is waiting on the client, and
// whether an output write is pending or has moved since. Times are on the
// monotonic clock, as offsets from epoch.
type watchedConn struct {
	net.Conn
	epoch time.Time
	// readingSince is when the Read now waiting on the client began, and zero
	// while none is: the input copy is delivering what it read to a shim that
	// is not reading it yet. The websocket reads a pong only inside a Read.
	readingSince atomic.Int64
	// writing is how many output writes are in progress; moves counts every
	// one that began or ended. A write blocked on a client that is not reading
	// holds the websocket's writer, and no ping or pong can be sent behind it.
	writing atomic.Int32
	moves   atomic.Int64
}

func newWatchedConn() *watchedConn { return &watchedConn{epoch: time.Now()} }

// now is the time since epoch, never zero.
func (c *watchedConn) now() int64 { return int64(time.Since(c.epoch)) + 1 }

// idle reports whether a read is waiting on the client and no output write is
// pending: nothing is backed up either way.
func (c *watchedConn) idle() bool {
	return c.readingSince.Load() != 0 && c.writing.Load() == 0
}

func (c *watchedConn) Read(p []byte) (int, error) {
	c.readingSince.Store(c.now())
	defer c.readingSince.Store(0)
	return c.Conn.Read(p)
}

func (c *watchedConn) Write(p []byte) (int, error) {
	c.moves.Add(1)
	c.writing.Add(1)
	defer func() {
		c.writing.Add(-1)
		c.moves.Add(1)
	}()
	return c.Conn.Write(p)
}

// keepalive pings the client every interval until ctx is canceled, and returns
// an error when the client is gone: the connection closed under a ping, or a
// ping failed or went unanswered for timeout while the attach was idle the
// whole time, a read waiting on the client and no output write pending, begun
// or ended.
//
// A ping is sent only while the attach is idle. A client that paused reading
// (`| less`, a slow disk) backs the output write up, and that write holds the
// websocket's writer, which gives a ping five seconds to take it, so a ping
// could only fail. Input backed up behind a shim that is not reading stops
// this end's reads, so no pong would be read, and leaves the client's own
// writer held by its input write, behind which a client on this websocket
// library cannot write its pong for more than five seconds either. A ping
// already out when the attach backs up is not answered yet rather than
// missing, and waits on, judged again each timeout. Either way the attach is
// held back as a pipe would hold it. A client that dies while the attach is
// backed up is left to the transport to fail the blocked write (TCP's
// retransmission timeout, where the agent's peer is the client's end); one
// that dies while its input is backed up is not found until the command reads
// its input or exits. Pending is all this end can see: a ping written into a
// send buffer the client left full with no write pending still stalls, and
// the websocket closes the connection five seconds into it (#1695).
func keepalive(ctx context.Context, conn *websocket.Conn, client *watchedConn, interval, timeout time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if !client.idle() {
			continue
		}
		pingCtx, cancel := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() { result <- conn.Ping(pingCtx) }()
		err := awaitPong(ctx, result, client, timeout)
		cancel()
		if err != nil {
			return err
		}
		ticker.Reset(interval)
	}
}

// awaitPong waits for a sent ping's result, giving up on the client only when
// the connection closed or the attach stayed idle through a failed ping or a
// whole timeout (see keepalive). A ping that failed while the attach was busy
// is dropped, and the next one tries again.
func awaitPong(ctx context.Context, result <-chan error, client *watchedConn, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	start := client.now()
	moves := client.moves.Load()
	idleSinceStart := func() bool {
		reading := client.readingSince.Load()
		return reading != 0 && reading <= start &&
			client.writing.Load() == 0 && client.moves.Load() == moves
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-result:
			switch {
			case err == nil, ctx.Err() != nil:
				return nil
			case errors.Is(err, net.ErrClosed), idleSinceStart():
				return err
			}
			return nil
		case <-timer.C:
		}
		if idleSinceStart() {
			return errors.New("keepalive ping unanswered")
		}
		start = client.now()
		moves = client.moves.Load()
		timer.Reset(timeout)
	}
}

// OneShot is a connected non-interactive attach. Connecting and running are
// split so a caller can guarantee ordering: the attach must be connected before
// the exec starts, or a fast command's output is broadcast to nobody and lost.
type OneShot struct {
	conn   net.Conn
	reader *bufio.Reader
}

// ConnectOneShot opens the attach for a one-shot run. The caller starts the exec
// only after this returns, then calls Run.
//
// A streaming attach is a raw byte tunnel — the frame protocol is spoken
// end-to-end between the remote client and the shim, and this process is only a
// pipe. A one-shot caller has no tunnel to speak over, so this is the one place
// the agent speaks the frame protocol itself, keeping that protocol from leaking
// into callers that only want "feed it bytes, give me its output".
func ConnectOneShot(ctx context.Context, socketPath, protocol string) (*OneShot, error) {
	shimConn, shimReader, err := attachShim(ctx, socketPath, protocol, false)
	if err != nil {
		return nil, err
	}
	return &OneShot{conn: shimConn, reader: shimReader}, nil
}

func (o *OneShot) Close() { _ = o.conn.Close() }

// Run writes stdin, closes the input side, and returns everything the exec
// emitted, output and error streams interleaved as produced. The exit status is
// not returned: the exec record is authoritative for that, and the caller reads
// it there.
func (o *OneShot) Run(ctx context.Context, stdin []byte) ([]byte, error) {
	// Write failures here are benign when the command has already finished: a
	// fast-exiting command leads the shim to send the exit frame and close the
	// attach before these writes land, and the output is still buffered on the
	// socket for the read loop below. A command that actually needed the input
	// surfaces through its exit status, which the caller checks.
	if len(stdin) > 0 {
		if err := frame.Write(o.conn, frame.Input, stdin); err != nil {
			return o.drain(ctx)
		}
	}
	// Close the input side so a command reading to EOF (cat, tee) terminates.
	if err := frame.Write(o.conn, frame.CloseInput, nil); err != nil {
		return o.drain(ctx)
	}
	return o.drain(ctx)
}

// drain reads attach frames to completion, returning the exec's output.
func (o *OneShot) drain(ctx context.Context) ([]byte, error) {
	var out []byte
	for {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		f, err := frame.Read(o.reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The shim hung up without an explicit exit frame; whatever it
				// emitted is still the exec's output.
				return out, nil
			}
			return out, err
		}
		switch f.Type {
		case frame.Stdout, frame.Stderr, frame.Error:
			out = append(out, f.Payload...)
		case frame.Exit:
			return out, nil
		}
	}
}

func attachShim(ctx context.Context, socketPath, protocol string, replay bool) (net.Conn, *bufio.Reader, error) {
	shimConn, err := Dial(ctx, socketPath, defaultDialTimeout)
	if err != nil {
		return nil, nil, err
	}
	attachURL := "http://unix/attach"
	if replay {
		attachURL += "?replay=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, attachURL, nil)
	if err != nil {
		_ = shimConn.Close()
		return nil, nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol)
	if err := req.Write(shimConn); err != nil {
		_ = shimConn.Close()
		return nil, nil, err
	}
	shimReader := bufio.NewReader(shimConn)
	resp, err := http.ReadResponse(shimReader, req)
	if err != nil {
		_ = shimConn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		_ = shimConn.Close()
		return nil, nil, fmt.Errorf("attach shim: %s", resp.Status)
	}
	return shimConn, shimReader, nil
}

func Dial(ctx context.Context, socketPath string, timeout time.Duration) (net.Conn, error) {
	if strings.TrimSpace(socketPath) == "" {
		return nil, fmt.Errorf("shim socket path is required")
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for shim socket")
	}
	return nil, lastErr
}

func closeWrite(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
