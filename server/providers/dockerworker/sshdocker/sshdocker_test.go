package sshdocker

import (
	"context"
	"strings"
	"testing"
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
