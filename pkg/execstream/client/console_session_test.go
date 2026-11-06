package client

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

func TestSessionWithNonFileStdinDisablesConsoleControl(t *testing.T) {
	conn, remote := newPipe(t)
	stdin := strings.NewReader("")
	var stdout strings.Builder
	session := New(Options{
		Conn: conn, Stdin: stdin, Stdout: &stdout, Stderr: io.Discard,
		Console: NewOSConsole(stdin, &stdout), RawMode: true, Terminal: true,
	})
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("non-file stdin must disable console control without panicking: %v", p)
		}
	}()
	if err := session.WriteInitialResize(); err != nil {
		t.Fatalf("initial resize: %v", err)
	}
	// No resize or signal frame should be sent when console control is off.
	sent := make(chan int, 1)
	go func() {
		count := 0
		for {
			if _, err := frame.Read(remote); err != nil {
				sent <- count
				return
			}
			count++
		}
	}()
	peerErr := make(chan error, 1)
	go func() {
		if err := frame.Write(remote, frame.Stdout, []byte("done")); err != nil {
			peerErr <- err
			return
		}
		peerErr <- frame.Write(remote, frame.Exit, []byte(`{"exitCode":0}`))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := session.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := <-peerErr; err != nil {
		t.Fatalf("remote: %v", err)
	}
	if count := <-sent; count != 0 {
		t.Fatalf("sent %d frames without a console, want none", count)
	}
	if got := stdout.String(); got != "done" {
		t.Fatalf("stdout = %q, want done", got)
	}
}
