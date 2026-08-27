package ports

import (
	"context"
	"errors"
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
	targets map[int]netip.AddrPort
}

func newRecordingProbe(answers map[int]Protocol) *recordingProbe {
	if answers == nil {
		answers = map[int]Protocol{}
	}
	return &recordingProbe{answers: answers, calls: map[int]int{}, targets: map[int]netip.AddrPort{}}
}

func (p *recordingProbe) lastTarget(port int) netip.AddrPort {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.targets[port]
}

func (p *recordingProbe) probe(_ context.Context, target netip.AddrPort) Protocol {
	p.mu.Lock()
	defer p.mu.Unlock()
	port := int(target.Port())
	p.calls[port]++
	p.targets[port] = target
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

// A declared port is reported whatever the scan found, which is the whole
// point: the socket belongs to root, so the uid filter never sees it (ADR 0076).
func TestWatcherReportsADeclaredPortNothingIsListeningOn(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	probe := newRecordingProbe(map[int]Protocol{8080: ProtocolHTTP})
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    probe.probe,
		Declared: func() ([]int, error) { return []int{8080}, nil },
	})

	watcher.tick(context.Background())

	got := snapshotByPort(t, watcher, 8080)
	if !got.Declared {
		t.Errorf("declared = false, want true")
	}
	if len(got.Addresses) != 0 {
		t.Errorf("addresses = %v, want none: nothing visible is bound there", got.Addresses)
	}
	// Probing works where discovery does not — connecting does not care which
	// uid owns the far end — so the port still classifies.
	if got.Protocol != ProtocolHTTP {
		t.Errorf("protocol = %q, want http", got.Protocol)
	}
	if target := probe.lastTarget(8080); target.Addr().String() != "127.0.0.1" {
		t.Errorf("probed %v, want loopback: a declared port has no observed bind to aim at", target)
	}
}

// The declared set is read every tick, so a service file written while the
// sandbox is up takes effect without a restart (ADR 0070 §5).
func TestWatcherFollowsTheDeclaredSetAsItChanges(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	var declared []int
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    newRecordingProbe(nil).probe,
		Declared: func() ([]int, error) { return declared, nil },
	})

	watcher.tick(context.Background())
	if len(watcher.Snapshot()) != 0 {
		t.Fatalf("snapshot = %+v, want empty", watcher.Snapshot())
	}

	declared = []int{5432}
	watcher.tick(context.Background())
	snapshotByPort(t, watcher, 5432)

	declared = nil
	watcher.tick(context.Background())
	if len(watcher.Snapshot()) != 0 {
		t.Fatalf("snapshot = %+v after the declaration went away, want empty", watcher.Snapshot())
	}
}

// A declared port that is also listening is an ordinary observed port that
// happens to be declared: it keeps its binds and its socket-keyed probe cache.
func TestWatcherKeepsTheObservationOfADeclaredPortThatIsAlsoListening(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1F90", 1000, 41001))
	probe := newRecordingProbe(map[int]Protocol{8080: ProtocolHTTP})
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    probe.probe,
		Declared: func() ([]int, error) { return []int{8080}, nil },
	})

	watcher.tick(context.Background())
	got := snapshotByPort(t, watcher, 8080)
	if !got.Declared {
		t.Errorf("declared = false, want true")
	}
	if len(got.Addresses) != 1 || got.Addresses[0] != "127.0.0.1" {
		t.Errorf("addresses = %v, want the observed bind", got.Addresses)
	}

	// Same socket, still declared: the cache is the socket's, so no second probe.
	watcher.tick(context.Background())
	if calls := probe.callCount(8080); calls != 1 {
		t.Errorf("probe called %d times for an unchanged socket, want 1", calls)
	}
}

// The declaration is the identity a declared port's classification is cached
// against, since it has no socket to key on — re-probing it every tick is the
// standing scan ADR 0046 refused.
func TestWatcherProbesADeclaredPortOnceItAnswers(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	probe := newRecordingProbe(map[int]Protocol{8080: ProtocolUnknown})
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    probe.probe,
		Declared: func() ([]int, error) { return []int{8080}, nil },
	})

	// Nothing is up yet: unknown, and retried, the way an unreachable observed
	// port is.
	watcher.tick(context.Background())
	watcher.tick(context.Background())
	if calls := probe.callCount(8080); calls != 2 {
		t.Fatalf("probe called %d times while the port was unreachable, want 2", calls)
	}

	probe.setAnswer(8080, ProtocolHTTP)
	watcher.tick(context.Background())
	if got := snapshotByPort(t, watcher, 8080); got.Protocol != ProtocolHTTP {
		t.Fatalf("protocol = %q once the service answered, want http", got.Protocol)
	}
	watcher.tick(context.Background())
	if calls := probe.callCount(8080); calls != 3 {
		t.Errorf("probe called %d times, want 3: an established answer is not asked for again", calls)
	}
}

// Declaring the agent's own port does not make it a service.
func TestWatcherExcludesADeclaredPortItMustNotReport(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	watcher := New(Config{
		UID:          1000,
		ProcRoot:     fixture.root,
		ExcludePorts: []int{8558},
		Probe:        newRecordingProbe(nil).probe,
		Declared:     func() ([]int, error) { return []int{8558, 70000, 0}, nil },
	})

	watcher.tick(context.Background())
	if snapshot := watcher.Snapshot(); len(snapshot) != 0 {
		t.Fatalf("snapshot = %+v, want empty", snapshot)
	}
}

// A declared set that cannot be read is this tick's gap, not a fault: the scan
// still reports what it found.
func TestWatcherSurvivesADeclaredSetItCannotRead(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1435", 1000, 41001))
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    newRecordingProbe(nil).probe,
		Declared: func() ([]int, error) { return nil, errors.New("read .discobox/services: permission denied") },
	})

	watcher.tick(context.Background())
	if got := snapshotByPort(t, watcher, 5173); got.Declared {
		t.Errorf("declared = true, want false")
	}
}
