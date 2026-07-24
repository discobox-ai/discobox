package main

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestDecodeTmuxEscapes(t *testing.T) {
	got := decodeTmuxEscapes(`plain\040text\015\012\\`)
	want := []byte("plain text\r\n\\")
	if !bytes.Equal(got, want) {
		t.Fatalf("decodeTmuxEscapes() = %q, want %q", got, want)
	}
}

func TestTmuxOutputPayload(t *testing.T) {
	got, ok := tmuxOutputPayload(`%output %3 DBXPONG:00000001\015\012`)
	if !ok {
		t.Fatal("tmuxOutputPayload() did not recognize output notification")
	}
	want := []byte("DBXPONG:00000001\r\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("tmuxOutputPayload() = %q, want %q", got, want)
	}
	if _, ok := tmuxOutputPayload("%begin 1 2 3"); ok {
		t.Fatal("tmuxOutputPayload() recognized a non-output notification")
	}

	got, ok = tmuxOutputPayload(`%extended-output %3 15 ignored : split\040payload`)
	if !ok || string(got) != "split payload" {
		t.Fatalf("extended tmuxOutputPayload() = %q, %v", got, ok)
	}
}

func TestStreamBufferFindsMarkerAcrossWrites(t *testing.T) {
	buffer := newStreamBuffer(1024)
	buffer.append([]byte("DBXP"))
	buffer.append([]byte("ONG:00000001"))
	if err := buffer.wait([]byte("DBXPONG:00000001"), time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSummarize(t *testing.T) {
	got := summarize([]float64{100, 10, 50, 20, 30})
	if got.Count != 5 || got.MinUS != 10 || got.P50US != 30 || got.P95US != 100 || got.MaxUS != 100 {
		t.Fatalf("summarize() = %#v", got)
	}
}

func TestTmuxControllerRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("tmux process integration")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	controller, err := startTmux(ctx, []string{
		"sh", "-c",
		`printf DBXREADY; stty raw -echo; value=$(dd bs=1 count=4 2>/dev/null); printf 'ECHO:%s' "$value"; sleep 10`,
	}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	if err := controller.WaitOutput("DBXREADY", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := controller.SendBytes([]byte("PING")); err != nil {
		t.Fatal(err)
	}
	if err := controller.WaitOutput("ECHO:PING", 5*time.Second); err != nil {
		t.Fatal(err)
	}
}
