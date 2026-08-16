package ports

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// procFixture writes a net/tcp table under a fresh procfs root and returns it.
type procFixture struct {
	root string
	t    *testing.T
}

func newProcFixture(t *testing.T) *procFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &procFixture{root: root, t: t}
}

func (f *procFixture) write(rows ...string) {
	f.t.Helper()
	table := procNetTCPHeader
	for _, row := range rows {
		table += row
	}
	if err := os.WriteFile(filepath.Join(f.root, "net", "tcp"), []byte(table), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// row renders one listening socket the way /proc/net/tcp does. addrHex is the
// address as the kernel prints it: the numeric value of each 32-bit word, so
// 127.0.0.1 reads as 0100007F on a little-endian machine.
func row(index int, addrHex string, portHex string, uid int, inode uint64) string {
	return "   " + strconv.Itoa(index) + ": " + addrHex + ":" + portHex +
		" 00000000:0000 0A 00000000:00000000 00:00000000 00000000  " +
		strconv.Itoa(uid) + "        0 " + strconv.FormatUint(inode, 10) + " 1 0000 100 0 0 10 0\n"
}

// recordingProbe answers with a canned protocol per port and remembers how many
// times each was asked, which is what "probed once per socket" is tested on.
type recordingProbe struct {
	mu      sync.Mutex
	answers map[int]Protocol
	calls   map[int]int
}

func newRecordingProbe(answers map[int]Protocol) *recordingProbe {
	return &recordingProbe{answers: answers, calls: map[int]int{}}
}

func (p *recordingProbe) probe(_ context.Context, target netip.AddrPort) Protocol {
	p.mu.Lock()
	defer p.mu.Unlock()
	port := int(target.Port())
	p.calls[port]++
	if answer, ok := p.answers[port]; ok {
		return answer
	}
	return ProtocolTCP
}

func (p *recordingProbe) callCount(port int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[port]
}

func (p *recordingProbe) setAnswer(port int, protocol Protocol) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answers[port] = protocol
}

func snapshotByPort(t *testing.T, watcher *Watcher, port int) Port {
	t.Helper()
	for _, entry := range watcher.Snapshot() {
		if entry.Port == port {
			return entry
		}
	}
	t.Fatalf("port %d is not in the snapshot: %+v", port, watcher.Snapshot())
	return Port{}
}

func TestWatcherClassifiesAndCachesPerSocket(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1435", 1000, 41001))
	probe := newRecordingProbe(map[int]Protocol{5173: ProtocolHTTP})
	watcher := New(Config{UID: 1000, ProcRoot: fixture.root, Probe: probe.probe})

	watcher.tick(context.Background())
	if got := snapshotByPort(t, watcher, 5173); got.Protocol != ProtocolHTTP {
		t.Fatalf("protocol = %q, want http", got.Protocol)
	}

	// The same socket on the next tick must not be asked again: the whole point
	// of caching is that the port is not reconnected to every interval.
	watcher.tick(context.Background())
	if calls := probe.callCount(5173); calls != 1 {
		t.Fatalf("probe called %d times for an unchanged socket, want 1", calls)
	}
}

func TestWatcherReprobesWhenTheSocketBehindAPortIsReplaced(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1435", 1000, 41001))
	probe := newRecordingProbe(map[int]Protocol{5173: ProtocolHTTP})
	watcher := New(Config{UID: 1000, ProcRoot: fixture.root, Probe: probe.probe})
	watcher.tick(context.Background())
	first := snapshotByPort(t, watcher, 5173)

	// Same port, new inode: the server restarted, this time over TLS.
	fixture.write(row(0, "0100007F", "1435", 1000, 41002))
	probe.setAnswer(5173, ProtocolHTTPS)
	watcher.tick(context.Background())

	got := snapshotByPort(t, watcher, 5173)
	if got.Protocol != ProtocolHTTPS {
		t.Errorf("protocol = %q after the socket was replaced, want https", got.Protocol)
	}
	if calls := probe.callCount(5173); calls != 2 {
		t.Errorf("probe called %d times across a socket replacement, want 2", calls)
	}
	// The port itself never stopped listening, so how long it has been up is
	// not reset by whatever restarted behind it.
	if !got.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Errorf("firstSeenAt = %v, want it preserved at %v", got.FirstSeenAt, first.FirstSeenAt)
	}
}

func TestWatcherRetriesPortsItCouldNotReach(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1435", 1000, 41001))
	probe := newRecordingProbe(map[int]Protocol{5173: ProtocolUnknown})
	watcher := New(Config{UID: 1000, ProcRoot: fixture.root, Probe: probe.probe})

	watcher.tick(context.Background())
	if got := snapshotByPort(t, watcher, 5173); got.Protocol != ProtocolUnknown {
		t.Fatalf("protocol = %q, want unknown", got.Protocol)
	}

	// A port that would not answer may simply not have been ready; the next
	// tick asks again rather than leaving it unknown forever.
	probe.setAnswer(5173, ProtocolHTTP)
	watcher.tick(context.Background())
	if got := snapshotByPort(t, watcher, 5173); got.Protocol != ProtocolHTTP {
		t.Fatalf("protocol = %q after a retry, want http", got.Protocol)
	}
	if calls := probe.callCount(5173); calls != 2 {
		t.Fatalf("probe called %d times, want 2", calls)
	}
}

func TestWatcherGroupsOneWildcardPortAcrossFamilies(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, table string) {
		if err := os.WriteFile(filepath.Join(root, "net", name), []byte(procNetTCPHeader+table), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tcp", row(0, "00000000", "1F90", 1000, 41001))
	write("tcp6", row(0, "00000000000000000000000000000000", "1F90", 1000, 41002))

	var probed netip.AddrPort
	watcher := New(Config{UID: 1000, ProcRoot: root, Probe: func(_ context.Context, target netip.AddrPort) Protocol {
		probed = target
		return ProtocolHTTP
	}})
	watcher.tick(context.Background())

	snapshot := watcher.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot = %+v, want one entry for the dual-stack port", snapshot)
	}
	if got := snapshot[0].Addresses; len(got) != 2 || got[0] != "0.0.0.0" || got[1] != "::" {
		t.Errorf("addresses = %v, want both wildcard binds", got)
	}
	// A wildcard address is not somewhere to dial; loopback is.
	if probed.String() != "127.0.0.1:8080" {
		t.Errorf("probed %s, want 127.0.0.1:8080", probed)
	}
}

func TestWatcherIgnoresOtherUsersAndExcludedPorts(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(
		row(0, "00000000", "0BBB", 1000, 41001), // 3003: sandbox-agent's own listener
		row(1, "00000000", "0016", 0, 41002),    // sshd, owned by root
		row(2, "0100007F", "1435", 1000, 41003), // the sandbox user's dev server
	)
	watcher := New(Config{
		UID:          1000,
		ExcludePorts: []int{3003},
		ProcRoot:     fixture.root,
		Probe:        func(context.Context, netip.AddrPort) Protocol { return ProtocolHTTP },
	})
	watcher.tick(context.Background())

	snapshot := watcher.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Port != 5173 {
		t.Fatalf("snapshot = %+v, want only the sandbox user's port 5173", snapshot)
	}
}

func TestWatcherDropsPortsThatStopListening(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1435", 1000, 41001))
	watcher := New(Config{UID: 1000, ProcRoot: fixture.root, Probe: func(context.Context, netip.AddrPort) Protocol { return ProtocolHTTP }})
	watcher.tick(context.Background())

	fixture.write()
	watcher.tick(context.Background())
	if snapshot := watcher.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("snapshot = %+v, want empty after the port stopped listening", snapshot)
	}
}

func TestWatcherPublishesANewPortBeforeItsProbeAnswers(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1435", 1000, 41001))
	release := make(chan struct{})
	watcher := New(Config{UID: 1000, ProcRoot: fixture.root, Probe: func(context.Context, netip.AddrPort) Protocol {
		<-release
		return ProtocolHTTP
	}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.tick(context.Background())
	}()

	// A slow server must not withhold the fact that the port exists.
	deadline := time.After(2 * time.Second)
	for {
		if got := watcher.Snapshot(); len(got) == 1 && got[0].Protocol == ProtocolUnknown {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("port was not published before its probe answered: %+v", watcher.Snapshot())
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	<-done
	if got := snapshotByPort(t, watcher, 5173); got.Protocol != ProtocolHTTP {
		t.Fatalf("protocol = %q once the probe answered, want http", got.Protocol)
	}
}

func TestWatcherSnapshotIsNilSafe(t *testing.T) {
	var watcher *Watcher
	if got := watcher.Snapshot(); got != nil {
		t.Fatalf("Snapshot on a nil watcher = %+v, want nil", got)
	}
}
