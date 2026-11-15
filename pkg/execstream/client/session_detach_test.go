package client

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// A session that Detaches is an attach to a terminal that keeps running, so a
// signal addressed to this process ends the attach, hands the terminal back,
// and reaches the remote not at all.
func TestDetachingSessionEndsOnSignalsWithoutForwardingThem(t *testing.T) {
	for _, tc := range []struct {
		sig  os.Signal
		name string
	}{{syscall.SIGTERM, "TERM"}, {os.Interrupt, "INT"}} {
		t.Run(tc.name, func(t *testing.T) {
			conn, remote := newPipe(t)
			forwarded := make(chan frame.Frame, 4)
			go func() {
				for {
					next, err := frame.Read(remote)
					if err != nil {
						return
					}
					forwarded <- next
				}
			}()
			console := newFakeConsole()
			s := New(Options{
				Conn: conn, Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
				Console: console, RawMode: true, Terminal: true, Detaches: true, Kind: "test",
			})
			done := make(chan error, 1)
			go func() { done <- s.Run(context.Background()) }()

			console.deliver(t, tc.sig)
			var err error
			select {
			case err = <-done:
			case <-time.After(3 * time.Second):
				t.Fatalf("session still running after SIG%s", tc.name)
			}
			var signaled SignalError
			if !errors.As(err, &signaled) || signaled.Signal != tc.sig {
				t.Fatalf("Run = %v, want ended by SIG%s", err, tc.name)
			}
			if events := console.Events(); !ordered(events, "makeraw", "reset", "restore") {
				t.Fatalf("events = %v, want the terminal handed back", events)
			}
			for {
				select {
				case next := <-forwarded:
					if next.Type == frame.Signal {
						t.Fatalf("forwarded %s to the remote", next.Payload)
					}
				default:
					return
				}
			}
		})
	}
}
