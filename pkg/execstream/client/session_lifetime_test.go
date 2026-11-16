package client

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// heldSizeConsole is a fakeConsole whose first Size call is held until the
// test lets it go, as a read of a terminal that is slow to answer would be.
type heldSizeConsole struct {
	*fakeConsole
	entered chan struct{}
	release chan struct{}
}

func (c *heldSizeConsole) Size() (int, int, bool) {
	select {
	case c.entered <- struct{}{}:
		<-c.release
	default:
	}
	return c.fakeConsole.Size()
}

// Run hands the console back when it returns: nothing it started is still
// reading the caller's terminal afterwards. A caller closes its stdin once
// the session ends, and a resize watcher still asking that file for its size
// would be reading a closed descriptor.
func TestRunReturnsOnlyOnceNothingItStartedStillReadsTheConsole(t *testing.T) {
	conn, remote := newPipe(t)
	console := &heldSizeConsole{fakeConsole: newFakeConsole(), entered: make(chan struct{}), release: make(chan struct{})}
	s := New(Options{
		Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Console: console, Terminal: true, Kind: "test",
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	select {
	case <-console.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the session never read the terminal's size")
	}
	payload, _ := frame.EncodeExit("exited", ptr(int64(0)), "")
	go func() { _ = frame.Write(remote, frame.Exit, payload) }()

	select {
	case <-done:
		close(console.release)
		t.Fatal("Run returned while its resize watcher was still reading the console")
	case <-time.After(300 * time.Millisecond):
	}
	close(console.release)
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}
