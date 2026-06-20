package daemon

import (
	"syscall"
	"testing"
)

func TestParseSignal(t *testing.T) {
	got, err := parseSignal("sigint")
	if err != nil {
		t.Fatalf("parse signal: %v", err)
	}
	if got != syscall.SIGINT {
		t.Fatalf("got %v, want SIGINT", got)
	}
}
