package proxyagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy"
)

// A WebSocket message is decided under the proxy's publish lock
// (Swapper.Decide), and deciding calls this package's resolver, which takes
// its routes lock; publishing the policy file takes the routes lock and then
// the publish lock, one after the other and never one inside the other.
// Driven together — messages swapped on an open connection while the policy
// file is published again and again — they finish, and every message reaches
// the origin with the real value at its place (ADR 26-11-06-442 §4).
func TestWebSocketSwapsWhileThePolicyIsRepublished(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const sentinel = "dsc_SENTINELSENTINELSENTINELSENTINEL1"
	const other = "dsc_OTHEROTHEROTHEROTHEROTHEROTHEROT2"
	const realValue = "dsc_REALREALREALREALREALREALREALREAL3"
	const messages = 1500

	var swapped, leaked atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		_ = rw.Flush()
		for {
			opcode, payload, err := readTestFrame(rw)
			if err != nil || opcode == 8 {
				return
			}
			switch {
			case strings.Contains(string(payload), `"token":"`+realValue+`"`):
				swapped.Add(1)
			default:
				leaked.Add(1)
			}
			_, _ = conn.Write(testFrame(1, []byte("ack"), false))
		}
	}))
	defer origin.Close()
	originHost := mustHostname(t, origin.URL)

	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		var body resolveRequestBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Sentinel != sentinel || body.SandboxID != "sb-1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		expiry := time.Now().Add(time.Hour)
		_ = json.NewEncoder(w).Encode(resolveResponseBody{Status: "approved", Value: realValue, ExpiresAt: &expiry})
	}))
	defer controlPlane.Close()

	route, err := secretroute.Parse("GET " + originHost + ` ws=[{"at":"/d/token","when":{"op":2}}]`)
	if err != nil {
		t.Fatal(err)
	}
	grants := map[string][]secretroute.Grant{sentinel: oneGrant([][]secretroute.Route{{route}}), other: oneGrant([][]secretroute.Route{{route}})}

	root := withTestRoot(t)
	if err := WriteResolveContext(root, testProjectID, testPoolID, controlPlane.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	bundle, err := PrepareBundle(root, testProjectID, testPoolID)
	if err != nil {
		t.Fatal(err)
	}
	material, err := proxy.EnsureClientCertificate(bundle, "sb-1", PoolProxyURL, "", time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	publishSentinels := func(withOther bool) {
		set := []string{sentinel}
		if withOther {
			set = append(set, other)
		}
		if err := UpsertSandboxSentinels(root, testProjectID, testPoolID, "sb-1", set, nil, grants, nil); err != nil {
			t.Errorf("write sentinels: %v", err)
		}
	}
	publishSentinels(false)
	cfg := proxy.DefaultConfig()
	cfg.AllowLoopbackDestinations = true
	cfg.ListenAddress = "127.0.0.1:0"
	cfg.CertDir = bundle.Dir
	cfg.DatabaseDSN = filepath.Join(root.Dir(), testProjectID+"-audit.db")
	cfg.Recording.Enabled = false
	resolver := newSecretResolver(root, testProjectID, testPoolID)
	server, err := proxy.NewServer(ctx, cfg, bundle, resolver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	policy := newPolicyPublisher(server, cfg, func(err error) { t.Errorf("apply proxy config: %v", err) })
	secretsFile := root.ProxySecretsFile(testProjectID, testPoolID)
	if err := policy.loadFile(secretsFile, resolver); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.ListenAndServe() }()

	conn, reader := dialTestWebSocket(t, waitForProxyAddr(t, server), material, origin.URL)
	defer conn.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			publishSentinels(i%2 == 0)
			if err := policy.loadFile(secretsFile, resolver); err != nil {
				t.Errorf("republish: %v", err)
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		for i := range messages {
			msg := fmt.Sprintf(`{"op":2,"d":{"token":%q,"seq":%d}}`, sentinel, i)
			if _, err := conn.Write(testFrame(1, []byte(msg), true)); err != nil {
				done <- err
				return
			}
			opcode, payload, err := readTestFrame(reader)
			if err != nil {
				done <- err
				return
			}
			if opcode != 1 {
				done <- fmt.Errorf("message %d: got opcode %d %q, want the origin's ack", i, opcode, payload)
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("messages and republishing did not finish: a lock cycle between deciding and publishing")
	}
	close(stop)
	wg.Wait()
	if swapped.Load() != messages || leaked.Load() != 0 {
		t.Fatalf("origin got %d swapped and %d other messages, want %d swapped", swapped.Load(), leaked.Load(), messages)
	}
}

// dialTestWebSocket upgrades to target through the proxy over mTLS, as a
// sandbox's plain-HTTP WebSocket client does.
func dialTestWebSocket(t *testing.T, proxyAddr string, material proxy.ClientMaterial, target string) (net.Conn, *bufio.Reader) {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(material.ClientCertPath, material.ClientKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(material.MTLSCAPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse CA")
	}
	conn, err := tls.Dial("tcp", proxyAddr, &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(target, "http://")
	if _, err := fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", target, host); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status %d", resp.StatusCode)
	}
	return conn, reader
}

// testFrame is one final frame, masked as a client's are when mask is set.
func testFrame(opcode byte, payload []byte, mask bool) []byte {
	out := []byte{0x80 | opcode}
	bit := byte(0)
	if mask {
		bit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		out = append(out, bit|byte(n))
	case n < 1<<16:
		out = append(out, bit|126, byte(n>>8), byte(n))
	default:
		out = append(out, bit|127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	if !mask {
		return append(out, payload...)
	}
	var key [4]byte
	_, _ = rand.Read(key[:])
	out = append(out, key[:]...)
	for i, c := range payload {
		out = append(out, c^key[i%4])
	}
	return out
}

// readTestFrame reads one frame, unmasking it.
func readTestFrame(r io.Reader) (byte, []byte, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, nil, err
	}
	n := uint64(head[1] & 127)
	switch n {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext)
	}
	var key [4]byte
	masked := head[1]&0x80 != 0
	if masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return head[0] & 15, payload, nil
}
