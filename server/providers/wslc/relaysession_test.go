package wslc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/cpmux"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/wslc/internal/wslcsession"
)

// The guest is handed megabytes over a pipe and there is no acknowledgement in
// the protocol itself, so the digest it echoes back is the only thing standing
// between a short write and a relay that fails later as a mux handshake that
// never completes.
func TestInstallGuestBinaryRejectsWhatTheGuestDidNotReceive(t *testing.T) {
	binary := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 4096)

	t.Run("digest matches", func(t *testing.T) {
		starter := &fakeStarter{}
		if err := installGuestBinary(t.Context(), starter, "/tmp/relay", binary); err != nil {
			t.Fatalf("installGuestBinary: %v", err)
		}
		if !bytes.Equal(starter.conn.received.Bytes(), binary) {
			t.Fatalf("guest received %d bytes, want the %d sent", starter.conn.received.Len(), len(binary))
		}
		if !strings.Contains(starter.argv(), "chmod 0755") {
			t.Errorf("install command does not make the program executable: %s", starter.argv())
		}
		assertStderrFolded(t, starter)
	})

	t.Run("digest differs", func(t *testing.T) {
		starter := &fakeStarter{reply: strings.Repeat("0", 64)}
		err := installGuestBinary(t.Context(), starter, "/tmp/relay", binary)
		if err == nil {
			t.Fatal("installGuestBinary accepted a program the guest reported back differently")
		}
		if !strings.Contains(err.Error(), "/tmp/relay") {
			t.Errorf("error = %v, want it to name the program that did not arrive", err)
		}
	})

	// A shell that failed writes its reason where the digest would be, and that
	// reason is the whole diagnosis, so it has to survive into the error.
	t.Run("guest reports an error instead", func(t *testing.T) {
		starter := &fakeStarter{reply: "/bin/sh: can't create /tmp/relay.tmp: Read-only file system"}
		err := installGuestBinary(t.Context(), starter, "/tmp/relay", binary)
		if err == nil {
			t.Fatal("installGuestBinary ignored a failing guest shell")
		}
		if !strings.Contains(err.Error(), "Read-only file system") {
			t.Errorf("error = %v, want it to carry what the guest said", err)
		}
		assertStderrFolded(t, starter)
	})
}

// The fake hands its reply back on stdout, but a real shell that fails writes
// its reason to stderr, which the relay never carries. Those tests only mean
// something if the script sends stderr to stdout for its whole body - a
// redirect on the last command alone leaves every earlier failure silent.
func assertStderrFolded(t *testing.T, starter *fakeStarter) {
	t.Helper()
	if !strings.HasPrefix(starter.argv(), "/bin/sh -c exec 2>&1; ") {
		t.Errorf("guest script does not fold stderr into stdout before it runs: %s", starter.argv())
	}
}

// A guest process that starts and then never answers is the case the timeout
// exists for: guestConn deadlines are no-ops, and every caller of this runs
// inside EnsureVM while it holds the driver mutex, so a wedged guest with no
// timeout takes the whole driver down with it and reports nothing.
func TestGuestExchangeGivesUpOnAWedgedGuest(t *testing.T) {
	starter := &fakeStarter{block: true}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- installGuestBinary(ctx, starter, "/tmp/relay", []byte("payload"))
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want it to name the deadline", err)
		}
		if !strings.Contains(err.Error(), "/tmp/relay") {
			t.Errorf("error = %v, want it to name what was being installed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("installGuestBinary never returned; a wedged guest still blocks its caller forever")
	}

	// Giving up has to close the connection, which is the only thing that ends
	// the guest process and unblocks the read still sitting on it.
	if !starter.conn.isClosed() {
		t.Error("the guest connection was left open, so the guest process outlives the call that gave up")
	}
}

type fakeStarter struct {
	reply string // what the guest answers; empty means the digest of what it received
	block bool   // stand in for a guest that starts and then never answers
	argvs []string
	conn  *fakeGuestConn
}

func (s *fakeStarter) StartProcess(_ string, argv []string) (net.Conn, error) {
	s.argvs = append(s.argvs, strings.Join(argv, " "))
	s.conn = &fakeGuestConn{reply: s.reply, wedged: s.block, unblocked: make(chan struct{})}
	return s.conn, nil
}

func (s *fakeStarter) argv() string { return strings.Join(s.argvs, "; ") }

// fakeGuestConn stands in for a guest process's stdio: what the host writes is
// what the process reads, and what the host reads is what the process wrote
// once the host was done writing - at its CloseWrite, or at its first read for
// a command that takes no input.
type fakeGuestConn struct {
	reply    string
	received bytes.Buffer
	answer   *strings.Reader

	// A wedged guest answers nothing until its connection is closed under it,
	// which is how a real guestConn behaves: its deadlines are no-ops, so
	// closing the socket handles is the only thing that ends a blocked read.
	wedged    bool
	unblocked chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
}

func (c *fakeGuestConn) Write(b []byte) (int, error) { return c.received.Write(b) }

func (c *fakeGuestConn) Read(b []byte) (int, error) {
	if c.wedged {
		<-c.unblocked
	}
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.finishInput()
	return c.answer.Read(b)
}

func (c *fakeGuestConn) CloseWrite() error {
	c.finishInput()
	return nil
}

func (c *fakeGuestConn) finishInput() {
	if c.answer != nil {
		return
	}
	reply := c.reply
	if reply == "" {
		sum := sha256.Sum256(c.received.Bytes())
		reply = hex.EncodeToString(sum[:])
	}
	c.answer = strings.NewReader(reply + "\n")
}

func (c *fakeGuestConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.unblocked)
	})
	return nil
}

func (c *fakeGuestConn) isClosed() bool { return c.closed.Load() }

func (c *fakeGuestConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeGuestConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeGuestConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeGuestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeGuestConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// A guest that cannot exec the relay has no Docker path left, and the mux it
// would otherwise be judged by is a different process that keeps running - so
// without this the pool retries a broken guest on a backoff forever.
func TestGuestExecFailureCondemnsTheVM(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantState sandbox.Status
	}{
		{
			name:      "failed exec",
			err:       fmt.Errorf("start guest process: %w", &wslcsession.GuestExecError{Err: errors.New("HRESULT 0x80004005"), Errno: 2}),
			wantState: sandbox.StatusStopped,
		},
		{
			// The service reports its own failures in the same type with the
			// errno it started at; those say nothing about the guest.
			name:      "service failure with no guest errno",
			err:       fmt.Errorf("start guest process: %w", &wslcsession.GuestExecError{Err: errors.New("HRESULT 0x800706BA"), Errno: -1}),
			wantState: sandbox.StatusRunning,
		},
		{
			// An ordinary dial failure is not evidence about the guest, and
			// replacing a working VM over one is worse than the failure.
			name:      "any other failure",
			err:       errors.New("connection reset"),
			wantState: sandbox.StatusRunning,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const poolID = "condemn-test"
			driver, err := NewDriver(DriverConfig{})
			if err != nil {
				t.Fatalf("NewDriver: %v", err)
			}
			relay, stop := liveRelaySession(t)
			defer stop()
			driver.sessions[poolID] = nil // present, which is all InspectVM asks of it
			driver.relays[poolID] = relay

			driver.condemnOnGuestExecFailure(poolID, tc.err)

			info, err := driver.InspectVM(t.Context(), poolID)
			if err != nil {
				t.Fatalf("InspectVM: %v", err)
			}
			if info.Status != tc.wantState {
				t.Fatalf("VM reported %q after %s, want %q", info.Status, tc.name, tc.wantState)
			}
		})
	}
}

// liveRelaySession builds a relaySession whose mux is genuinely up, so
// healthy() answers for real rather than for a stub.
func liveRelaySession(t *testing.T) (*relaySession, func()) {
	t.Helper()
	host, guest := net.Pipe()
	go func() {
		server, err := cpmux.Server(guest)
		if err != nil {
			return
		}
		<-t.Context().Done()
		_ = server.Close()
	}()
	session, err := cpmux.Client(host)
	if err != nil {
		t.Fatalf("cpmux.Client: %v", err)
	}
	r := &relaySession{session: session, conn: host, stopped: make(chan struct{})}
	if !r.healthy() {
		t.Fatal("the test's own relay session is not healthy to begin with")
	}
	return r, r.close
}

// The shell's exit status does not cross the stdio relay, so the guest's
// answer is the only signal the mount happened - and when it did not, that
// answer is the reason, which the log line is useless without.
func TestMountGuestSharedMemoryReadsTheGuestsAnswer(t *testing.T) {
	t.Run("mounted", func(t *testing.T) {
		starter := &fakeStarter{reply: "mounted"}
		if err := mountGuestSharedMemory(t.Context(), starter); err != nil {
			t.Fatalf("mountGuestSharedMemory: %v", err)
		}
		// A WSL that starts shipping the mount must be left alone, not given a
		// second tmpfs over the first.
		if !strings.Contains(starter.argv(), "mountpoint -q /dev/shm ||") {
			t.Errorf("script mounts without checking for an existing mount: %s", starter.argv())
		}
	})

	t.Run("guest refused", func(t *testing.T) {
		starter := &fakeStarter{reply: "mount: /dev/shm: permission denied."}
		err := mountGuestSharedMemory(t.Context(), starter)
		if err == nil {
			t.Fatal("mountGuestSharedMemory reported success for a mount the guest refused")
		}
		if !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("error = %v, want it to carry what the guest said", err)
		}
		assertStderrFolded(t, starter)
	})
}
