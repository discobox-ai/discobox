package cli

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/portforward"
)

// backendDialer stands in for the sandbox: every target it is asked for is
// answered by one local server, whose own port has nothing to do with the
// target's — so the forward's local port can be the target's number without
// colliding with the server standing in for it.
type backendDialer struct{ address string }

func (d backendDialer) DialPort(ctx context.Context, _ portforward.Target) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.address)
}

// greeter accepts connections and writes "hello" to each.
func greeter(t *testing.T) string {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("hello"))
			_ = conn.Close()
		}
	}()
	return listener.Addr().String()
}

// freePort is a port nothing holds, as a sandbox's listener would report it.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

// listing is a sandbox port listing the test changes as it goes.
type listing struct {
	mu      sync.Mutex
	targets []portforward.Target
	calls   atomic.Int32
}

func (l *listing) set(targets ...portforward.Target) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.targets = targets
}

func (l *listing) list(context.Context) ([]portforward.Target, error) {
	l.calls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]portforward.Target(nil), l.targets...), nil
}

func ephemeralPort(unavailable string) apimodel.HarnessConfigPort {
	port := apimodel.HarnessConfigPort{Ephemeral: apiclientgen.NewOptBool(true)}
	if unavailable != "" {
		port.Unavailable = apiclientgen.NewOptString(unavailable)
	}
	return port
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An ephemeral entry forwards a port the sandbox starts listening on after the
// flow began — the sign-in's callback server — at that same number, and leaves
// the sandbox's UDP ports and its image's declared services alone.
func TestConfigureForwardBindsADiscoveredPortAtItsOwnNumber(t *testing.T) {
	sandbox := &listing{}
	status := &syncBuffer{}
	forward := startConfigureForward(t.Context(), backendDialer{greeter(t)}, sandbox.list, 10*time.Millisecond,
		[]apimodel.HarnessConfigPort{ephemeralPort("")}, status)
	defer forward.Close()

	callback, udp, desktop := freePort(t), freePort(t), freePort(t)
	sandbox.set(
		portforward.Target{Network: portforward.TCP, Port: callback},
		portforward.Target{Network: portforward.UDP, Port: udp},
		portforward.Target{Network: portforward.TCP, Port: desktop, ServiceID: "ai.discobox.desktop"},
	)
	eventually(t, "the discovered port to be bound", func() bool { return len(forward.forwarder.Bindings()) > 0 })

	bindings := forward.forwarder.Bindings()
	if len(bindings) != 1 || bindings[0].Target.Port != callback || bindings[0].Local != callback {
		t.Fatalf("bindings = %+v, want TCP %d bound at %d and nothing else", bindings, callback, callback)
	}
	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(callback)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("localhost:%d answered %q, want the sandbox's server", callback, got)
	}
	if !strings.Contains(status.String(), "at the same number") {
		t.Fatalf("status = %q, want the ephemeral forward announced", status.String())
	}
}

// A discovered port this machine already holds is reported by number, with the
// image's words after it, on lines of its own a raw terminal shows, and left
// unbound — never moved to another number, which the redirect
// URI the browser holds would not name.
func TestConfigureForwardNeverMovesATakenDiscoveredPort(t *testing.T) {
	taken, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port

	sandbox := &listing{}
	sandbox.set(portforward.Target{Port: port})
	status := &syncBuffer{}
	forward := startConfigureForward(t.Context(), backendDialer{greeter(t)}, sandbox.list, 10*time.Millisecond,
		[]apimodel.HarnessConfigPort{ephemeralPort("sign in by device code instead")}, status)
	defer forward.Close()

	want := "\r\nwarning: port " + strconv.Itoa(port) + " is already in use on this machine, so it was not forwarded into the configure discobox. sign in by device code instead\r\n"
	eventually(t, "the taken port to be reported", func() bool {
		return strings.Contains(status.String(), want)
	})
	// A few more polls, each of which retries the bind.
	calls := sandbox.calls.Load()
	eventually(t, "more polls", func() bool { return sandbox.calls.Load() > calls+2 })
	if bindings := forward.forwarder.Bindings(); len(bindings) != 0 {
		t.Fatalf("bindings = %+v, want none: the port was taken and must not move", bindings)
	}
	if n := strings.Count(status.String(), "warning:"); n != 1 {
		t.Fatalf("status = %q, want the taken port reported once, not on every poll", status.String())
	}
}

// Numbered ports alone are static: nothing polls the listing, and a port the
// sandbox reports is not forwarded just because it is listening.
func TestConfigureForwardWithoutAnEphemeralPortDoesNotFollowTheListing(t *testing.T) {
	fixed := freePort(t)
	sandbox := &listing{}
	sandbox.set(portforward.Target{Port: freePort(t)})
	status := &syncBuffer{}
	forward := startConfigureForward(t.Context(), backendDialer{greeter(t)}, sandbox.list, 10*time.Millisecond,
		[]apimodel.HarnessConfigPort{{Port: int64(fixed)}}, status)
	defer forward.Close()

	time.Sleep(100 * time.Millisecond)
	if calls := sandbox.calls.Load(); calls != 0 {
		t.Fatalf("listing polled %d times, want never", calls)
	}
	bindings := forward.forwarder.Bindings()
	if len(bindings) != 1 || bindings[0].Local != fixed {
		t.Fatalf("bindings = %+v, want only the declared %d", bindings, fixed)
	}
	if want := "Forwarding localhost:" + strconv.Itoa(fixed); !strings.Contains(status.String(), want) {
		t.Fatalf("status = %q, want %q", status.String(), want)
	}
}

// An image's unavailable words are printed while the terminal is raw, so a
// control sequence in them is shown escaped rather than obeyed.
func TestConfigurePortWarningsEscapeTheImagesWords(t *testing.T) {
	advice := "run /login again\x1b]0;owned\x07\x1b[2J"
	for name, got := range map[string]string{
		"numbered":   configPortUnavailable(apimodel.HarnessConfigPort{Port: 1455, Unavailable: apiclientgen.NewOptString(advice)}),
		"discovered": discoveredPortUnavailable(ephemeralPort(advice), 43579),
	} {
		if strings.ContainsAny(got, "\x1b\x07") {
			t.Errorf("%s warning = %q, want its control characters escaped", name, got)
		}
		if !strings.Contains(got, `run /login again\x1b]0;owned\a\x1b[2J`) {
			t.Errorf("%s warning = %q, want the advice shown escaped", name, got)
		}
	}
}
