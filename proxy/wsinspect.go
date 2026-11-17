package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/discobox-ai/discobox/proxy/internal/wsframe"
)

// A credential may be carried in a message a client sends on a WebSocket
// connection, at a place one of its routes names (ADR 26-11-06-442). A
// connection some such route covers is inspected: the client's frames are
// read whole, message by message, and none of a message reaches the origin
// before the whole of it has been decided — its sentinels found, authorized
// against the current routes, resolved, swapped in, and sent as one frame.
// Every other connection is copied byte for byte, as before.

// The bounds of an inspected connection: of one message, and of everything
// held at once, queued and being assembled.
const (
	inspectPayloadLimit = 64 << 10
	inspectWireLimit    = 80 << 10
	inspectFrameLimit   = 1024
)

// upgradeContext is the upgrade request as the client sent it, which every
// message on its connection is decided against.
type upgradeContext struct {
	method string
	host   string
	url    string
	header http.Header
}

// websocketUpgrade reports whether req is a WebSocket handshake a message
// place can apply to: HTTP/1.1, GET, version 13, with a key. Anything else is
// copied as it is, its messages unread.
func websocketUpgrade(req *http.Request) bool {
	return req.ProtoMajor == 1 && req.ProtoMinor == 1 && req.Method == http.MethodGet &&
		headerContainsToken(req.Header, "Connection", "Upgrade") && headerContainsToken(req.Header, "Upgrade", "websocket") &&
		strings.TrimSpace(req.Header.Get("Sec-WebSocket-Version")) == "13" && strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key")) != ""
}

// planInspection decides, before the handshake is sent, whether its
// connection's messages are read: when some open WebSocket route of the
// client's credentials covers it. An inspected handshake offers no
// extension, so no message is compressed in either direction.
func (h *httpProxy) planInspection(req *http.Request, meta *requestMeta, client clientIdentity, preURL string) {
	if !websocketUpgrade(req) || !h.swapper.InspectsMessages(meta.ctx, secrets.PlacesRequest{
		ClientID: client.ID,
		Method:   req.Method,
		Host:     req.Host,
		URL:      preURL,
	}) {
		return
	}
	req.Header.Del("Sec-WebSocket-Extensions")
	meta.inspect = &upgradeContext{method: req.Method, host: req.Host, url: preURL, header: req.Header.Clone()}
}

// heldItem is one thing an inspected connection holds for the origin, in
// order: a complete message, or the client's close frame.
type heldItem struct {
	opcode  byte
	payload []byte // the application data, unmasked
	wire    []byte // the frames as the client sent them
	frames  int
	close   bool
}

// failCloseGrace is how long an ended connection's close frame may wait for
// the origin to take a write in progress before the connection is closed
// without it.
const failCloseGrace = 5 * time.Second

// charge is what held items cost against the connection's bounds.
type charge struct {
	payload, wire, frames int
}

func (c charge) add(o charge) charge {
	return charge{c.payload + o.payload, c.wire + o.wire, c.frames + o.frames}
}

func (c charge) within() bool {
	return c.payload <= inspectPayloadLimit && c.wire <= inspectWireLimit && c.frames <= inspectFrameLimit
}

// messageInspector is one inspected connection's enforcement.
type messageInspector struct {
	h       *httpProxy
	stream  *upgradedResponseStream
	client  clientIdentity
	upgrade upgradeContext
	ctx     context.Context
	cancel  context.CancelFunc

	// What Write alone touches: the bytes of a frame not yet complete, the
	// message being assembled, and whether the client has closed.
	pending    []byte
	assembling *heldItem
	closed     bool

	mu         sync.Mutex
	cond       *sync.Cond
	queue      []*heldItem
	held       charge // queued and assembling together
	retained   []string
	terminated bool
	code       uint16
	reason     string
	secretIDs  []string

	// writing serializes writes to the origin.
	writing sync.Mutex
	sent    atomic.Int64

	// What Read alone touches: the server's frame boundaries, and the close
	// frame sent the client in place of the rest of the stream.
	tracker    wsframe.Tracker
	stopped    bool
	closeFrame []byte
}

func newMessageInspector(h *httpProxy, stream *upgradedResponseStream, client clientIdentity, upgrade upgradeContext) *messageInspector {
	ctx, cancel := context.WithCancel(context.WithoutCancel(stream.meta.ctx))
	i := &messageInspector{h: h, stream: stream, client: client, upgrade: upgrade, ctx: ctx, cancel: cancel}
	i.cond = sync.NewCond(&i.mu)
	i.retained = h.swapper.Sentinels(client.ID)
	go i.run()
	return i
}

// write takes what the client sent toward the origin. It never fails: a
// connection it ends is closed with a close frame each way, and what the
// client sends after that is dropped until it goes.
func (i *messageInspector) write(p []byte) (int, error) {
	if i.closed || i.isTerminated() {
		return len(p), nil
	}
	i.pending = append(i.pending, p...)
	for !i.closed && !i.isTerminated() {
		h, ok, err := wsframe.ParseHeader(i.pending)
		if err != nil {
			i.fail(err)
			break
		}
		if !ok {
			break
		}
		if err := i.checkFrame(h); err != nil {
			i.fail(err)
			break
		}
		total := h.Size + int(h.Length)
		if len(i.pending) < total {
			break
		}
		frame := bytes.Clone(i.pending[:total])
		i.pending = i.pending[total:]
		if err := i.take(h, frame); err != nil {
			i.fail(err)
			break
		}
	}
	i.pending = append([]byte(nil), i.pending...)
	return len(p), nil
}

// checkFrame refuses a frame before any of its payload is held: one
// reserved bits are set on (no extension was negotiated), one a client sent
// unmasked, one out of order, and one that would take its message past the
// bounds.
func (i *messageInspector) checkFrame(h wsframe.Header) error {
	switch {
	case h.RSV != 0:
		return &wsframe.Error{Code: wsframe.CloseProtocolError, Reason: "a WebSocket frame sets reserved bits, and no extension was negotiated"}
	case !h.Masked:
		return &wsframe.Error{Code: wsframe.CloseProtocolError, Reason: "a client's WebSocket frame is not masked"}
	case h.Control():
		return nil
	case h.Opcode == wsframe.Continuation && i.assembling == nil:
		return &wsframe.Error{Code: wsframe.CloseProtocolError, Reason: "a WebSocket continuation frame continues no message"}
	case h.Opcode != wsframe.Continuation && i.assembling != nil:
		return &wsframe.Error{Code: wsframe.CloseProtocolError, Reason: "a WebSocket message began before the last one ended"}
	}
	var so charge
	if i.assembling != nil {
		so = charge{len(i.assembling.payload), len(i.assembling.wire), i.assembling.frames}
	}
	if h.Length > inspectPayloadLimit || !so.add(charge{int(h.Length), h.Size + int(h.Length), 1}).within() {
		return &wsframe.Error{Code: wsframe.CloseTooBig, Reason: "a WebSocket message on a connection whose messages are inspected for credentials is larger than 64 KiB, 80 KiB of frames or 1024 frames"}
	}
	return nil
}

// take handles one complete frame.
func (i *messageInspector) take(h wsframe.Header, frame []byte) error {
	payload := bytes.Clone(frame[h.Size:])
	wsframe.Unmask(payload, h.Mask)
	if h.Control() {
		if found := i.carried(payload); found {
			return &wsframe.Error{Code: wsframe.ClosePolicyViolation, Reason: "route: a WebSocket control frame carries a credential's placeholder, which is never swapped there"}
		}
		if h.Opcode != wsframe.Close {
			return i.send(frame)
		}
		if err := wsframe.ValidClose(payload); err != nil {
			return err
		}
		// A close goes after what the client sent before it.
		i.closed = true
		return i.enqueue(&heldItem{close: true, wire: frame, frames: 1}, charge{0, len(frame), 1})
	}
	cost := charge{len(payload), len(frame), 1}
	if !i.reserve(cost) {
		return nil
	}
	if i.assembling == nil {
		i.assembling = &heldItem{opcode: h.Opcode}
	}
	m := i.assembling
	m.payload = append(m.payload, payload...)
	m.wire = append(m.wire, frame...)
	m.frames++
	if !h.Fin {
		return nil
	}
	i.assembling = nil
	if m.opcode == wsframe.Text && !utf8.Valid(m.payload) {
		return &wsframe.Error{Code: wsframe.CloseInvalidPayload, Reason: "a WebSocket text message is not UTF-8"}
	}
	return i.enqueue(m, charge{})
}

// carried reports whether a control frame's payload carries a sentinel of
// the client's, now or before.
func (i *messageInspector) carried(payload []byte) bool {
	_, err := i.h.swapper.MatchMessage(i.client.ID, payload, false, i.retainedSnapshot())
	return err != nil
}

// reserve waits until cost fits beside what is held, and charges it: the
// bounded queue's backpressure, which stops reading the client meanwhile.
// False when the connection ended while it waited.
func (i *messageInspector) reserve(cost charge) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for !i.terminated && !i.held.add(cost).within() {
		i.cond.Wait()
	}
	if i.terminated {
		return false
	}
	i.held = i.held.add(cost)
	return true
}

// enqueue queues an item for the worker, charging extra beside what reserve
// charged for it already.
func (i *messageInspector) enqueue(item *heldItem, extra charge) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.terminated {
		return nil
	}
	i.held = i.held.add(extra)
	i.queue = append(i.queue, item)
	i.cond.Broadcast()
	return nil
}

// run sends the queued items to the origin, in order, one at a time.
func (i *messageInspector) run() {
	for {
		i.mu.Lock()
		for len(i.queue) == 0 && !i.terminated {
			i.cond.Wait()
		}
		if i.terminated {
			i.mu.Unlock()
			return
		}
		item := i.queue[0]
		i.mu.Unlock()

		var err error
		if item.close {
			err = i.send(item.wire)
		} else {
			err = i.forward(item)
		}

		i.mu.Lock()
		if len(i.queue) > 0 && i.queue[0] == item {
			i.queue = i.queue[1:]
			i.held = i.held.add(charge{-len(item.payload), -len(item.wire), -item.frames})
		}
		i.cond.Broadcast()
		i.mu.Unlock()
		if err != nil {
			i.fail(err)
			return
		}
	}
}

// forward decides one complete message and sends it, or says why it ends the
// connection.
func (i *messageInspector) forward(item *heldItem) error {
	swapper := i.h.swapper
	path := "/"
	if u, err := url.Parse(i.upgrade.url); err == nil && u.Path != "" {
		path = u.Path
	}
	// One window for the whole message, however many sentinels and asks it
	// takes (ADR 26-10-14-482 §2).
	retryUntil := time.Now().Add(swapper.ResolveRetry())
	for {
		// Read under the configuration it will be sent under: a message
		// decided again after a change is read again too, so a sentinel
		// taken away meanwhile is refused (MatchMessage's retained).
		revision := swapper.Revision()
		uses, err := swapper.MatchMessage(i.client.ID, item.payload, item.opcode == wsframe.Text, i.retainedSnapshot())
		i.retain(swapper.Sentinels(i.client.ID))
		var refusal *secrets.MessageRefusal
		if errors.As(err, &refusal) {
			return &wsframe.Error{Code: wsframe.ClosePolicyViolation, Reason: "route: " + refusal.Reason}
		}
		if uses.Empty() {
			return i.send(item.wire)
		}
		req := secrets.AuthorizeRequest{
			ClientID: i.client.ID,
			Places:   uses.Places(),
			Message:  uses.Message(),
			Method:   i.upgrade.method,
			Host:     i.upgrade.host,
			URL:      i.upgrade.url,
			Header:   i.upgrade.header,
		}
		verdict, err := secrets.RetryTransient(i.ctx, retryUntil, func(ctx context.Context) (secrets.Verdict, error) {
			return swapper.Authorize(ctx, req)
		}, func(error) bool { return false })
		if err != nil {
			return &wsframe.Error{Code: wsframe.CloseInternalError, Reason: "credential unavailable: the credential's routes could not be read: " + err.Error()}
		}
		if !verdict.Allow {
			return &wsframe.Error{Code: wsframe.ClosePolicyViolation, Reason: "route: " + refusalReason(verdict, nil)}
		}
		swapped, result, echoes := swapper.ApplyMessage(i.ctx, i.client.ID, i.upgrade.host, path, uses, retryUntil)
		if len(result.Errors) > 0 {
			return &wsframe.Error{Code: wsframe.CloseInternalError, Reason: "credential unavailable: " + strings.Join(result.Errors, "; ")}
		}
		wire := item.wire
		if swapped != nil {
			if len(swapped) > inspectPayloadLimit {
				return &wsframe.Error{Code: wsframe.CloseTooBig, Reason: "a WebSocket message with its credential swapped in is larger than 64 KiB"}
			}
			wire = wsframe.Encode(wsframe.Text, swapped, true)
		}
		// The connection's write lock first, outside publication: once the
		// message is decided nothing else may write before it, so a decision
		// is never left waiting on the origin while a revocation lands. No
		// configuration waits on this lock.
		i.writing.Lock()
		var decided *wsframe.Error
		stale, err := swapper.Decide(revision, i.client.ID, uses.Sentinels(), func() error {
			// Decided again at the moment of sending, so a binding that
			// lapsed or a route narrowed while the message was resolved
			// stops it. No network I/O here: publication waits on this.
			verdict, err := swapper.Authorize(i.ctx, req)
			switch {
			case err != nil:
				decided = &wsframe.Error{Code: wsframe.CloseInternalError, Reason: "credential unavailable: the credential's routes could not be read: " + err.Error()}
			case !verdict.Allow:
				decided = &wsframe.Error{Code: wsframe.ClosePolicyViolation, Reason: "route: " + refusalReason(verdict, nil)}
			}
			return nil
		})
		if err == nil && decided == nil && !stale {
			if swapped != nil {
				// What the origin sends back may repeat the value: keep it
				// out of the capture before the origin has it.
				i.stream.stream.Redact(echoes)
			}
			err = i.sendLocked(wire)
			i.writing.Unlock()
			if err == nil && swapped != nil {
				i.noteSwap(result.SecretIDs)
			}
			return err
		}
		i.writing.Unlock()
		switch {
		case err != nil:
			return err
		case decided != nil:
			return decided
		case !time.Now().Before(retryUntil):
			return &wsframe.Error{Code: wsframe.CloseInternalError, Reason: "credential unavailable: the sandbox's credentials changed throughout the message's wait"}
		}
	}
}

// send writes whole frames to the origin, never interleaved with another
// write.
func (i *messageInspector) send(wire []byte) error {
	i.writing.Lock()
	defer i.writing.Unlock()
	return i.sendLocked(wire)
}

// sendLocked is send with the write lock held.
func (i *messageInspector) sendLocked(wire []byte) error {
	if i.isTerminated() {
		return nil
	}
	n, err := i.stream.source.Write(wire)
	i.sent.Add(int64(n))
	return err
}

// fail ends the connection: a close frame to the origin, with the code a
// framing or policy fault names (none for a connection that already broke),
// and one to the client in place of the rest of the server's stream (read).
// What was held is dropped unsent, and held work stops.
func (i *messageInspector) fail(err error) {
	code, reason := uint16(0), err.Error()
	var frameErr *wsframe.Error
	if errors.As(err, &frameErr) {
		code = frameErr.Code
	}
	i.mu.Lock()
	if i.terminated {
		i.mu.Unlock()
		return
	}
	i.terminated = true
	i.code, i.reason = code, reason
	i.queue = nil
	i.held = charge{}
	i.cond.Broadcast()
	i.mu.Unlock()
	i.cancel()
	// The close frame waits its turn behind any write in progress; an origin
	// that has stopped reading would hold it there for good, so the
	// connection is closed under that write after a bound either way.
	time.AfterFunc(failCloseGrace, func() { _ = i.stream.source.Close() })
	go func() {
		i.writing.Lock()
		if code != 0 {
			n, _ := i.stream.source.Write(wsframe.CloseFrame(code, "blocked by proxy", true))
			i.sent.Add(int64(n))
		}
		i.writing.Unlock()
		_ = i.stream.source.Close()
	}()
}

func (i *messageInspector) isTerminated() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.terminated
}

// read reads the server's stream for the client, unchanged, until the
// connection ends: then, at the end of the server's frame in progress, the
// client is sent the close frame and the stream ends.
func (i *messageInspector) read(p []byte) (int, error) {
	for {
		if i.stopped {
			if len(i.closeFrame) > 0 {
				n := copy(p, i.closeFrame)
				i.closeFrame = i.closeFrame[n:]
				return n, nil
			}
			return 0, io.EOF
		}
		if i.isTerminated() && i.tracker.AtBoundary() {
			i.stopped = true
			i.closeFrame = i.clientClose()
			continue
		}
		n, err := i.stream.source.Read(p)
		if !i.isTerminated() {
			off := 0
			for off < n {
				off += i.tracker.Advance(p[off:n])
			}
			return n, err
		}
		pass := 0
		if n > 0 && !i.tracker.AtBoundary() {
			pass = i.tracker.Advance(p[:n])
		}
		if pass > 0 {
			return pass, nil
		}
		if err != nil && !i.tracker.AtBoundary() {
			return 0, err
		}
	}
}

// clientClose is the close frame the client is sent when the connection
// ends: none for one that broke rather than being closed.
func (i *messageInspector) clientClose() []byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.code == 0 {
		return nil
	}
	return wsframe.CloseFrame(i.code, "blocked by proxy: "+i.reason, false)
}

func (i *messageInspector) retainedSnapshot() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.retained
}

// retain adds the client's sentinels now to those the connection recognizes:
// a sentinel taken from the client stays one its messages are refused for.
func (i *messageInspector) retain(current []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	next := append([]string(nil), i.retained...)
	for _, sentinel := range current {
		if !slices.Contains(next, sentinel) {
			next = append(next, sentinel)
		}
	}
	i.retained = next
}

func (i *messageInspector) noteSwap(secretIDs []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, id := range secretIDs {
		if !slices.Contains(i.secretIDs, id) {
			i.secretIDs = append(i.secretIDs, id)
		}
	}
}

// close stops held work when the connection goes.
func (i *messageInspector) close() {
	i.mu.Lock()
	if !i.terminated {
		i.terminated = true
		i.queue = nil
		i.cond.Broadcast()
	}
	i.mu.Unlock()
	i.cancel()
}

// outcome is what the connection's audit row says of it: the reason it was
// ended for, if it was, the secrets swapped into its messages, and the bytes
// sent to the origin.
func (i *messageInspector) outcome() (reason string, secretIDs []string, sent int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.code != 0 {
		reason = i.reason
	}
	return reason, append([]string(nil), i.secretIDs...), i.sent.Load()
}
