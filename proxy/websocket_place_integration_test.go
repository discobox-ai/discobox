package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"github.com/discobox-ai/discobox/proxy/internal/wsframe"
)

const (
	wsSentinel = "dsc_SENTINEL00000000000000000000000000000000"
	wsRealKey  = "dsc_REAL\"key\\0000000000000000000000000000"
	wsGateway  = `GET gateway.discord.gg,*.discord.gg ws=[{"at":"/d/token","when":{"op":2}},{"at":"/d/token","when":{"op":6}}]`
)

// wsResolver authorizes and names WebSocket places the way the pool's
// resolver does (pool-agent/proxyagent), over grants a test can change, and
// resolves the way a test says.
type wsResolver struct {
	noGatewayHeaders
	reportLog

	mu       sync.Mutex
	grants   []secretroute.Grant
	resolve  func() (secrets.ResolveResult, error)
	resolves int
}

func (r *wsResolver) setGrants(grants []secretroute.Grant) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grants = grants
}

func (r *wsResolver) currentGrants() []secretroute.Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.grants
}

func (r *wsResolver) ResolveRaw(context.Context, secrets.RawRequest) (secrets.RawResult, error) {
	return secrets.RawResult{}, secrets.ErrDenied
}

func (r *wsResolver) Gate(context.Context, secrets.GateRequest) (secrets.GateAdmission, error) {
	return secrets.GateAdmission{}, &secrets.GateRefusal{Reason: "no gate here"}
}

func (r *wsResolver) Authorize(_ context.Context, req secrets.AuthorizeRequest) (secrets.Verdict, error) {
	uses := map[string][]string{}
	for _, sentinel := range req.Sentinels {
		uses[sentinel] = append(uses[sentinel], secretroute.InHeaders)
	}
	for _, place := range req.Places {
		uses[place.Sentinel] = append(uses[place.Sentinel], place.At)
	}
	for _, at := range uses {
		if allowed, reason := secretroute.DecideGrants(r.currentGrants(), time.Now(), secretroute.Request{
			Method: req.Method, Host: req.Host, URL: req.URL, Header: req.Header, Body: req.Body, At: at, Message: req.Message,
		}); !allowed {
			return secrets.Verdict{Reason: reason}, nil
		}
	}
	return secrets.Verdict{Allow: true}, nil
}

func (r *wsResolver) BodyPlaces(context.Context, secrets.PlacesRequest) []string { return nil }

func (r *wsResolver) InspectsMessages(_ context.Context, req secrets.PlacesRequest) bool {
	return secretroute.WebSocketCandidate(r.currentGrants(), time.Now(), secretroute.Request{Method: req.Method, Host: req.Host, URL: req.URL})
}

func (r *wsResolver) Resolve(context.Context, secrets.ResolveRequest) (secrets.ResolveResult, error) {
	r.mu.Lock()
	r.resolves++
	resolve := r.resolve
	r.mu.Unlock()
	if resolve != nil {
		return resolve()
	}
	return secrets.ResolveResult{Value: wsRealKey, SecretID: "sec-discord", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func wsGrant(t *testing.T, routes ...string) secretroute.Grant {
	t.Helper()
	var list []secretroute.Route
	for _, text := range routes {
		route, err := secretroute.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, route)
	}
	return secretroute.Grant{Lists: [][]secretroute.Route{list}}
}

// wsFrame is one frame as an end received it, unmasked.
type wsFrame struct {
	fin     bool
	opcode  byte
	masked  bool
	payload []byte
}

func readWSFrame(r io.Reader) (wsFrame, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return wsFrame{}, err
	}
	f := wsFrame{fin: head[0]&0x80 != 0, opcode: head[0] & 15, masked: head[1]&0x80 != 0}
	n := uint64(head[1] & 127)
	switch n {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return wsFrame{}, err
		}
		n = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return wsFrame{}, err
		}
		n = binary.BigEndian.Uint64(ext)
	}
	var key [4]byte
	if f.masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return wsFrame{}, err
		}
	}
	f.payload = make([]byte, n)
	if _, err := io.ReadFull(r, f.payload); err != nil {
		return wsFrame{}, err
	}
	if f.masked {
		wsframe.Unmask(f.payload, key)
	}
	return f, nil
}

func (f wsFrame) closeCode() uint16 {
	if f.opcode != wsframe.Close || len(f.payload) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(f.payload)
}

// clientFrame is a frame as a client sends it, masked.
func clientFrame(fin bool, opcode byte, payload string) []byte {
	frame := wsframe.Encode(opcode, []byte(payload), true)
	if !fin {
		frame[0] &^= 0x80
	}
	return frame
}

// wsOrigin is a WebSocket origin that records every frame it receives and
// says when its connection ends.
type wsOrigin struct {
	*httptest.Server
	extensions string // a Sec-WebSocket-Extensions it answers with
	// stall, once closed, stops the origin reading, so its socket fills,
	// until stop is closed.
	stall, stop chan struct{}

	mu       sync.Mutex
	offered  []string
	frames   chan wsFrame
	conn     net.Conn
	upgrades int
}

func newWSOrigin() *wsOrigin {
	o := &wsOrigin{frames: make(chan wsFrame, 64)}
	o.Server = newOrigin(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		o.mu.Lock()
		o.offered = append(o.offered, r.Header.Get("Sec-WebSocket-Extensions"))
		o.conn = conn
		o.upgrades++
		o.mu.Unlock()
		head := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n"
		if o.extensions != "" {
			head += "Sec-WebSocket-Extensions: " + o.extensions + "\r\n"
		}
		_, _ = rw.WriteString(head + "\r\n")
		_ = rw.Flush()
		defer close(o.frames)
		defer conn.Close()
		for {
			if o.stall != nil {
				select {
				case <-o.stall:
					<-o.stop
					return
				default:
				}
			}
			f, err := readWSFrame(rw)
			if err != nil {
				return
			}
			o.frames <- f
			if f.opcode == wsframe.Ping {
				o.send(wsframe.Encode(wsframe.Pong, f.payload, false))
			}
		}
	})
	return o
}

// send writes server frames to the client.
func (o *wsOrigin) send(frame []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, _ = o.conn.Write(frame)
}

// next is the origin's next frame, or false once its connection ended.
func (o *wsOrigin) next(t *testing.T) (wsFrame, bool) {
	t.Helper()
	select {
	case f, ok := <-o.frames:
		return f, ok
	case <-time.After(10 * time.Second):
		t.Fatal("the origin waited too long for a frame")
		return wsFrame{}, false
	}
}

// wsProxy is a proxy for one sandbox with wsSentinel, and where its audit
// rows and streams go.
type wsProxy struct {
	server  *Server
	addr    string
	client  ClientMaterial
	dbPath  string
	streams string
	origin  *wsOrigin
}

func startWSProxy(ctx context.Context, t *testing.T, origin *wsOrigin, resolver SecretResolver, resolveRetry int64) *wsProxy {
	t.Helper()
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ProxyURL:    "https://127.0.0.1:0",
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &wsProxy{dbPath: filepath.Join(dir, "audit.db"), streams: filepath.Join(dir, "streams"), client: prepared.Clients["sandbox-1"], origin: origin}
	p.server, err = NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   p.dbPath,
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16, StreamDir: p.streams},
		Secrets: SecretsConfig{
			ResolveRetrySeconds: resolveRetry,
			Clients:             []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{wsSentinel}}},
		},
	}, prepared.Bundle, resolver)
	if err != nil {
		t.Fatal(err)
	}
	originAddr := origin.Listener.Addr().String()
	var dialer net.Dialer
	p.server.http.proxy.Tr = &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, originAddr)
		},
	}
	errCh := make(chan error, 1)
	go func() { errCh <- p.server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, p.server, errCh))
	p.addr = waitForAddr(t, p.server).String()
	return p
}

// wsClient is a sandbox's WebSocket connection through the proxy.
type wsClient struct {
	conn   net.Conn
	reader *bufio.Reader
	resp   *http.Response
}

// dial upgrades to host, offering extensions.
func (p *wsProxy) dial(ctx context.Context, t *testing.T, host, extensions string) *wsClient {
	t.Helper()
	conn := dialProxyMTLS(ctx, t, p.addr, p.client)
	t.Cleanup(func() { _ = conn.Close() })
	port := p.origin.Listener.Addr().(*net.TCPAddr).Port
	authority := net.JoinHostPort(host, strconv.Itoa(port))
	request := fmt.Sprintf("GET http://%s/?v=10&encoding=json HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n", authority, authority)
	if extensions != "" {
		request += "Sec-WebSocket-Extensions: " + extensions + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A 101's body is the connection, read through reader; a refusal's is
	// read by no test.
	t.Cleanup(func() { _ = resp.Body.Close() })
	return &wsClient{conn: conn, reader: reader, resp: resp}
}

func (c *wsClient) send(t *testing.T, frames ...[]byte) {
	t.Helper()
	for _, frame := range frames {
		if _, err := c.conn.Write(frame); err != nil {
			t.Fatalf("client write: %v", err)
		}
	}
}

// next is the client's next frame from the proxy.
func (c *wsClient) next(t *testing.T) wsFrame {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := readWSFrame(c.reader)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	return f
}

// closedWith waits for the proxy to close the client's connection with code,
// and the origin to be closed with it too, having received nothing more.
func closedWith(t *testing.T, client *wsClient, origin *wsOrigin, code uint16) {
	t.Helper()
	if f := client.next(t); f.closeCode() != code {
		t.Fatalf("client got %d %q, want close %d", f.opcode, f.payload, code)
	}
	f, ok := origin.next(t)
	if ok && f.closeCode() != code {
		t.Fatalf("origin got %d %q, want close %d", f.opcode, f.payload, code)
	}
	for ok {
		if f, ok = origin.next(t); ok {
			t.Fatalf("origin got %d %q after the close", f.opcode, f.payload)
		}
	}
}

func identify(token string, op int) string {
	return fmt.Sprintf(`{"op":%d,"d":{"token":%q,"intents":33281,"properties":{"os":"linux"}}}`, op, token)
}

// A Discord-style gateway grant: IDENTIFY and RESUME carry the real token at
// /d/token, in a fresh single masked frame, while the trail keeps the
// placeholder; a Presence update with the sentinel never reaches the origin,
// and the connection is closed with 1008 each way and recorded as blocked.
// An extension the client offered is never offered the origin.
func TestHTTPProxySwapsASentinelAtAWebSocketPlace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := newWSOrigin()
	t.Cleanup(origin.Close)
	resolver := &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}}
	proxy := startWSProxy(ctx, t, origin, resolver, 0)

	client := proxy.dial(ctx, t, "gateway.discord.gg", "permessage-deflate; client_max_window_bits")
	if client.resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", client.resp.StatusCode)
	}
	origin.mu.Lock()
	offered := origin.offered[0]
	origin.mu.Unlock()
	if offered != "" {
		t.Fatalf("the origin was offered %q", offered)
	}

	// IDENTIFY, fragmented across three frames with a ping between, the
	// sentinel split across two of them.
	message := identify(wsSentinel, 2)
	cut1, cut2 := strings.Index(message, wsSentinel)+5, strings.Index(message, wsSentinel)+20
	client.send(t,
		clientFrame(false, wsframe.Text, message[:cut1]),
		clientFrame(true, wsframe.Ping, "hb"),
		clientFrame(false, wsframe.Continuation, message[cut1:cut2]),
		clientFrame(true, wsframe.Continuation, message[cut2:]),
	)
	if f, _ := origin.next(t); f.opcode != wsframe.Ping || string(f.payload) != "hb" {
		t.Fatalf("origin first got %d %q, want the ping while the message is held", f.opcode, f.payload)
	}
	if f := client.next(t); f.opcode != wsframe.Pong {
		t.Fatalf("client got %d, want the origin's pong", f.opcode)
	}
	got, _ := origin.next(t)
	if !got.fin || got.opcode != wsframe.Text || !got.masked || string(got.payload) != identify(wsRealKey, 2) {
		t.Fatalf("origin got fin=%v op=%d masked=%v %s", got.fin, got.opcode, got.masked, got.payload)
	}

	// A message with no sentinel goes as sent; RESUME (op 6.0 is 6) swaps.
	heartbeat := clientFrame(true, wsframe.Text, `{"op":1,"d":4}`)
	client.send(t, heartbeat)
	if f, _ := origin.next(t); string(f.payload) != `{"op":1,"d":4}` {
		t.Fatalf("heartbeat = %s", f.payload)
	}
	client.send(t, clientFrame(true, wsframe.Text, `{"op":6.0,"d":{"token":"`+wsSentinel+`","seq":4}}`))
	if f, _ := origin.next(t); !strings.Contains(string(f.payload), strconv.Quote(wsRealKey)[1:len(strconv.Quote(wsRealKey))-1]) {
		t.Fatalf("resume = %s", f.payload)
	}

	// The server's messages reach the client unchanged, one that repeats the
	// credential included; the capture keeps the credential out of it.
	origin.send(wsframe.Encode(wsframe.Text, []byte(`{"op":0,"t":"READY"}`), false))
	if f := client.next(t); string(f.payload) != `{"op":0,"t":"READY"}` {
		t.Fatalf("client got %s", f.payload)
	}
	echo := `{"op":9,"d":"invalid token ` + wsRealKey + `","raw":` + strconv.Quote(wsRealKey) + `}`
	origin.send(wsframe.Encode(wsframe.Text, []byte(echo), false))
	if f := client.next(t); string(f.payload) != echo {
		t.Fatalf("client got %s", f.payload)
	}

	// Presence: the token where nothing allows it.
	client.send(t, clientFrame(true, wsframe.Text, `{"op":3,"d":{"activities":[{"name":"x"}],"token":"`+wsSentinel+`"}}`))
	closedWith(t, client, origin, wsframe.ClosePolicyViolation)

	exchange := waitForHTTPExchange(t, proxy.dbPath, "client_id = ? AND upgrade = ? AND blocked = ?", "sandbox-1", true, true)
	if !strings.HasPrefix(exchange.BlockedReason, "route: ") || !strings.Contains(exchange.BlockedReason, "/d/token") || exchange.Status != http.StatusSwitchingProtocols {
		t.Fatalf("blocked %q status %d", exchange.BlockedReason, exchange.Status)
	}
	if !exchange.UpgradeInspected || exchange.SwappedSecretIDs != "sec-discord" || exchange.UpgradeC2SSentBytes == 0 {
		t.Fatalf("inspected %v secrets %q sent %d", exchange.UpgradeInspected, exchange.SwappedSecretIDs, exchange.UpgradeC2SSentBytes)
	}
	// The trail holds what the sandbox sent, with its placeholder, and never
	// the real key.
	stream, err := os.ReadFile(filepath.Join(proxy.streams, filepath.FromSlash(exchange.StreamFile)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stream, []byte(wsSentinel)) || bytes.Contains(stream, []byte("dsc_REAL")) {
		t.Fatal("the stream capture does not hold the placeholder, or holds the real key")
	}
	assertNoRealKey(t, proxy.dbPath)
}

func assertNoRealKey(t *testing.T, dbPath string) {
	t.Helper()
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, []byte("dsc_REAL")) {
		t.Fatal("the audit database holds the real key")
	}
}

// What a message carries that no place can be closes the connection with
// 1008 and sends nothing of it: a sentinel inside a longer string, in a
// binary message, in a ping, or in a second place a route does not name.
func TestHTTPProxyRefusesASentinelOutsideAWebSocketPlace(t *testing.T) {
	for name, frames := range map[string][][]byte{
		"inside a longer string": {clientFrame(true, wsframe.Text, `{"op":2,"d":{"token":"Bot `+wsSentinel+`"}}`)},
		"binary":                 {clientFrame(true, wsframe.Binary, "\x00"+wsSentinel)},
		"ping":                   {clientFrame(true, wsframe.Ping, wsSentinel)},
		"a second place":         {clientFrame(true, wsframe.Text, `{"op":2,"d":{"token":"`+wsSentinel+`","presence":"`+wsSentinel+`"}}`)},
		"a member name":          {clientFrame(true, wsframe.Text, `{"op":2,"d":{"`+wsSentinel+`":"x"}}`)},
		"not one object":         {clientFrame(true, wsframe.Text, `[{"op":2,"d":{"token":"`+wsSentinel+`"}}]`)},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			origin := newWSOrigin()
			t.Cleanup(origin.Close)
			proxy := startWSProxy(ctx, t, origin, &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}}, 0)
			client := proxy.dial(ctx, t, "gateway.discord.gg", "")
			client.send(t, frames...)
			closedWith(t, client, origin, wsframe.ClosePolicyViolation)
			assertNoRealKey(t, proxy.dbPath)
		})
	}
}

// Every effective list must hold a WebSocket block allowing the place: a
// binding's list that names the gateway host but no message place refuses.
// So does a grant that lapsed, and one that lapses while the message waits
// on its value.
func TestHTTPProxyHoldsAWebSocketPlaceToEveryList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := newWSOrigin()
	t.Cleanup(origin.Close)
	narrowed := wsGrant(t, wsGateway)
	narrowed.Lists = append(narrowed.Lists, wsGrant(t, "*.discord.gg").Lists...)
	resolver := &wsResolver{grants: []secretroute.Grant{narrowed}}
	proxy := startWSProxy(ctx, t, origin, resolver, 0)
	client := proxy.dial(ctx, t, "gateway.discord.gg", "")
	client.send(t, clientFrame(true, wsframe.Text, identify(wsSentinel, 2)))
	closedWith(t, client, origin, wsframe.ClosePolicyViolation)
	exchange := waitForHTTPExchange(t, proxy.dbPath, "client_id = ? AND blocked = ?", "sandbox-1", true)
	if !strings.Contains(exchange.BlockedReason, "names no message place") {
		t.Fatalf("reason %q", exchange.BlockedReason)
	}

	// A grant that lapses while its value is being resolved: the message is
	// decided again when it is to be sent, and refused.
	origin2 := newWSOrigin()
	t.Cleanup(origin2.Close)
	var expires time.Time
	slow := &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}, resolve: func() (secrets.ResolveResult, error) {
		time.Sleep(time.Until(expires) + 100*time.Millisecond)
		return secrets.ResolveResult{Value: wsRealKey, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	proxy2 := startWSProxy(ctx, t, origin2, slow, 0)
	client2 := proxy2.dial(ctx, t, "gateway.discord.gg", "")
	expires = time.Now().Add(300 * time.Millisecond)
	lapsing := wsGrant(t, wsGateway)
	lapsing.ExpiresAt = &expires
	slow.setGrants([]secretroute.Grant{lapsing})
	client2.send(t, clientFrame(true, wsframe.Text, identify(wsSentinel, 2)))
	closedWith(t, client2, origin2, wsframe.ClosePolicyViolation)
	assertNoRealKey(t, proxy2.dbPath)
}

// A denied or pending credential leaves the placeholder in place and the
// message is sent, for the origin to refuse; a control plane that cannot be
// reached through the whole retry window sends nothing and closes with 1011.
func TestHTTPProxyResolvesAWebSocketPlaceLikeARequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	origin := newWSOrigin()
	t.Cleanup(origin.Close)
	denied := &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}, resolve: func() (secrets.ResolveResult, error) {
		return secrets.ResolveResult{}, secrets.ErrDenied
	}}
	proxy := startWSProxy(ctx, t, origin, denied, 0)
	client := proxy.dial(ctx, t, "gateway.discord.gg", "")
	client.send(t, clientFrame(true, wsframe.Text, identify(wsSentinel, 2)))
	if f, _ := origin.next(t); string(f.payload) != identify(wsSentinel, 2) {
		t.Fatalf("origin got %s", f.payload)
	}

	origin2 := newWSOrigin()
	t.Cleanup(origin2.Close)
	down := &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}, resolve: func() (secrets.ResolveResult, error) {
		return secrets.ResolveResult{}, errors.New("control plane unreachable")
	}}
	proxy2 := startWSProxy(ctx, t, origin2, down, 1)
	client2 := proxy2.dial(ctx, t, "gateway.discord.gg", "")
	start := time.Now()
	client2.send(t, clientFrame(true, wsframe.Text, identify(wsSentinel, 2)))
	closedWith(t, client2, origin2, wsframe.CloseInternalError)
	if waited := time.Since(start); waited < 900*time.Millisecond {
		t.Fatalf("closed after %v, before the retry window", waited)
	}
	exchange := waitForHTTPExchange(t, proxy2.dbPath, "client_id = ? AND blocked = ?", "sandbox-1", true)
	if !strings.HasPrefix(exchange.BlockedReason, "credential unavailable: ") {
		t.Fatalf("reason %q", exchange.BlockedReason)
	}
}

// A sentinel taken from the sandbox while its message is resolved is not
// sent: the configuration changed, so the message is decided again, and a
// sentinel the connection knew and the sandbox no longer holds is refused.
func TestHTTPProxyRefusesAWebSocketSentinelRemovedMidMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := newWSOrigin()
	t.Cleanup(origin.Close)
	release := make(chan struct{})
	resolver := &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}, resolve: func() (secrets.ResolveResult, error) {
		<-release
		return secrets.ResolveResult{Value: wsRealKey, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	proxy := startWSProxy(ctx, t, origin, resolver, 0)
	client := proxy.dial(ctx, t, "gateway.discord.gg", "")
	client.send(t, clientFrame(true, wsframe.Text, identify(wsSentinel, 2)))
	deadline := time.Now().Add(5 * time.Second)
	for {
		resolver.mu.Lock()
		asked := resolver.resolves
		resolver.mu.Unlock()
		if asked > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the message was never resolved")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cfg := proxy.server.cfg
	cfg.Secrets.Clients = []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{"dsc_OTHER0000000000000000000000000000000000"}}}
	if err := proxy.server.ApplyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	close(release)
	closedWith(t, client, origin, wsframe.ClosePolicyViolation)
	assertNoRealKey(t, proxy.dbPath)
}

// The bounds: a message past 64 KiB is refused with 1009 whether or not it
// carries a sentinel, as is a flood of empty fragments, never streamed past.
// A malformed frame closes with 1002, invalid text with 1007.
func TestHTTPProxyBoundsAnInspectedConnection(t *testing.T) {
	big := clientFrame(true, wsframe.Text, `{"op":1,"d":"`+strings.Repeat("x", 64<<10)+`"}`)
	var flood []byte
	flood = append(flood, clientFrame(false, wsframe.Text, "")...)
	for range 1100 {
		flood = append(flood, clientFrame(false, wsframe.Continuation, "")...)
	}
	unmasked := wsframe.Encode(wsframe.Text, []byte(`{"op":1}`), false)
	for name, tc := range map[string]struct {
		frames [][]byte
		code   uint16
	}{
		"too large":         {[][]byte{big}, wsframe.CloseTooBig},
		"too many frames":   {[][]byte{flood}, wsframe.CloseTooBig},
		"unmasked":          {[][]byte{unmasked}, wsframe.CloseProtocolError},
		"orphan continue":   {[][]byte{clientFrame(true, wsframe.Continuation, "x")}, wsframe.CloseProtocolError},
		"invalid text":      {[][]byte{clientFrame(true, wsframe.Text, "\xff")}, wsframe.CloseInvalidPayload},
		"reserved bit, rsv": {[][]byte{func() []byte { f := clientFrame(true, wsframe.Text, "{}"); f[0] |= 0x40; return f }()}, wsframe.CloseProtocolError},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			origin := newWSOrigin()
			t.Cleanup(origin.Close)
			proxy := startWSProxy(ctx, t, origin, &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}}, 0)
			client := proxy.dial(ctx, t, "gateway.discord.gg", "")
			client.send(t, tc.frames...)
			closedWith(t, client, origin, tc.code)
		})
	}
}

// A connection no WebSocket route covers is copied byte for byte, as before:
// a sentinel in its messages is sent as it is, swapped nowhere, and its
// extensions are negotiated as the two ends choose. An inspected connection
// whose server answers with an extension it was not offered is refused.
func TestHTTPProxyLeavesAnUninspectedConnectionAlone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := newWSOrigin()
	t.Cleanup(origin.Close)
	proxy := startWSProxy(ctx, t, origin, &wsResolver{grants: []secretroute.Grant{wsGrant(t, "*")}}, 0)
	client := proxy.dial(ctx, t, "gateway.discord.gg", "x-unknown")
	origin.mu.Lock()
	offered := origin.offered[0]
	origin.mu.Unlock()
	if offered != "x-unknown" {
		t.Fatalf("offered %q", offered)
	}
	frame := clientFrame(true, wsframe.Text, identify(wsSentinel, 2))
	client.send(t, frame)
	if f, _ := origin.next(t); string(f.payload) != identify(wsSentinel, 2) {
		t.Fatalf("origin got %s", f.payload)
	}
	_ = client.conn.Close()
	inspected := waitForHTTPExchange(t, proxy.dbPath, "client_id = ? AND upgrade = ?", "sandbox-1", true)
	if inspected.UpgradeInspected || inspected.Blocked {
		t.Fatalf("an uncovered connection was inspected: %+v", inspected)
	}

	origin2 := newWSOrigin()
	origin2.extensions = "permessage-deflate"
	t.Cleanup(origin2.Close)
	proxy2 := startWSProxy(ctx, t, origin2, &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}}, 0)
	refused := proxy2.dial(ctx, t, "gateway.discord.gg", "permessage-deflate")
	if refused.resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", refused.resp.StatusCode)
	}
	row := waitForHTTPExchange(t, proxy2.dbPath, "client_id = ? AND blocked = ?", "sandbox-1", true)
	if !strings.Contains(row.BlockedReason, "extension") {
		t.Fatalf("reason %q", row.BlockedReason)
	}
}

// An origin that stops reading holds up its own connection and nothing else:
// a message waiting to be written to it never keeps a configuration from
// being installed for the whole pool.
func TestHTTPProxyAStalledOriginHoldsUpNoConfiguration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	origin := newWSOrigin()
	origin.stall, origin.stop = make(chan struct{}), make(chan struct{})
	t.Cleanup(origin.Close)
	// Before the proxy shuts down, which waits on its connection.
	defer close(origin.stop)
	proxy := startWSProxy(ctx, t, origin, &wsResolver{grants: []secretroute.Grant{wsGrant(t, wsGateway)}}, 0)
	client := proxy.dial(ctx, t, "gateway.discord.gg", "")
	client.send(t, clientFrame(true, wsframe.Text, identify(wsSentinel, 2)))
	if f, _ := origin.next(t); !strings.Contains(string(f.payload), "dsc_REAL") {
		t.Fatalf("origin got %s", f.payload)
	}
	close(origin.stall)
	// Credential-bearing messages until the origin's socket is full and a
	// write to it blocks.
	big := `{"op":2,"d":{"token":"` + wsSentinel + `","pad":"` + strings.Repeat("x", 60<<10) + `"}}`
	go func() {
		for range 400 {
			if _, err := client.conn.Write(clientFrame(true, wsframe.Text, big)); err != nil {
				return
			}
		}
	}()
	time.Sleep(time.Second)
	applied := make(chan error, 1)
	go func() {
		cfg := proxy.server.cfg
		applied <- proxy.server.ApplyConfig(cfg)
	}()
	select {
	case err := <-applied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a configuration waited on a stalled origin")
	}
}
