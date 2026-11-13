package sshdocker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParseURL(t *testing.T) {
	target, err := ParseURL("ssh://ubuntu@203.0.113.9:2222")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if target.Host != "203.0.113.9" || target.User != "ubuntu" || target.Port != 2222 {
		t.Fatalf("target = %#v", target)
	}

	target, err = ParseURL("ssh://203.0.113.9")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	if target.Host != "203.0.113.9" || target.User != "" || target.Port != 0 {
		t.Fatalf("target = %#v", target)
	}

	if _, err := ParseURL("tcp://203.0.113.9"); err == nil {
		t.Fatal("non-ssh scheme accepted")
	}
	if _, err := ParseURL("ssh://"); err == nil {
		t.Fatal("hostless url accepted")
	}
}

func TestDialRequiresConfiguredKey(t *testing.T) {
	dialer, err := New("", "")
	if err != nil {
		t.Fatalf("new dialer: %v", err)
	}
	_, err = dialer.Dial(context.Background(), Target{Host: "203.0.113.9"})
	if err == nil || !strings.Contains(err.Error(), "sshPrivateKey") {
		t.Fatalf("dial err = %v, want missing key error", err)
	}
}

func TestNewRejectsInvalidKey(t *testing.T) {
	if _, err := New("root", "not-a-key"); err == nil || !strings.Contains(err.Error(), "ssh private key") {
		t.Fatalf("new err = %v, want parse error", err)
	}
}

// A host that accepts the connection and never sends its SSH banner must not
// hold a dial past its context: the engine's readiness wait bounds every
// attempt by one.
func TestDialGivesUpOnAHostThatNeverSpeaks(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := New("root", string(pem.EncodeToMemory(block)))
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := dialer.Dial(ctx, Target{Host: host, Port: portNumber})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dial error = %v, want deadline exceeded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Dial still in the SSH handshake 10s after its 200ms context ended")
	}
}
