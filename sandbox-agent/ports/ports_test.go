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

// udpRow renders one bound, unconnected UDP socket the way /proc/net/udp does.
func udpRow(index int, addrHex string, portHex string, uid int, inode uint64) string {
	return "  " + strconv.Itoa(index) + ": " + addrHex + ":" + portHex +
		" 00000000:0000 07 00000000:00000000 00:00000000 00000000  " +
		strconv.Itoa(uid) + "        0 " + strconv.FormatUint(inode, 10) + " 2 0000000000000000 0\n"
}

func (f *procFixture) writeUDP(rows ...string) {
	f.t.Helper()
	table := procNetUDPHeader
	for _, row := range rows {
		table += row
	}
	if err := os.WriteFile(filepath.Join(f.root, "net", "udp"), []byte(table), 0o644); err != nil {
		f.t.Fatal(err)
	}
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
		UID:             1000,
		ExcludeTCPPorts: []int{3003},
		ProcRoot:        fixture.root,
		Probe:           func(context.Context, netip.AddrPort) Protocol { return ProtocolHTTP },
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
		Declared: func() ([]Declaration, error) { return []Declaration{{Port: 8080}}, nil },
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
	var declared []Declaration
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    newRecordingProbe(nil).probe,
		Declared: func() ([]Declaration, error) { return declared, nil },
	})

	watcher.tick(context.Background())
	if len(watcher.Snapshot()) != 0 {
		t.Fatalf("snapshot = %+v, want empty", watcher.Snapshot())
	}

	declared = []Declaration{{Port: 5432}}
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
		Declared: func() ([]Declaration, error) { return []Declaration{{Port: 8080}}, nil },
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
		Declared: func() ([]Declaration, error) { return []Declaration{{Port: 8080}}, nil },
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
		UID:             1000,
		ProcRoot:        fixture.root,
		ExcludeTCPPorts: []int{8558},
		Probe:           newRecordingProbe(nil).probe,
		Declared:        func() ([]Declaration, error) { return []Declaration{{Port: 8558}, {Port: 70000}, {Port: 0}}, nil },
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
		Declared: func() ([]Declaration, error) { return nil, errors.New("read .discobox/services: permission denied") },
	})

	watcher.tick(context.Background())
	if got := snapshotByPort(t, watcher, 5173); got.Declared {
		t.Errorf("declared = true, want false")
	}
}

// The reason ADR 0094 (image-declared services) exists. Classifying a port means connecting to it, and
// connecting to a socket-activated port is what starts the service behind it —
// for the desktop, an X server, a window manager and a VNC server, brought up
// by a classification probe in every sandbox whether or not anybody wanted one.
//
// So a stated protocol must not be a shortcut that still probes: the port has
// to be reported without ever being touched.
func TestAStatedProtocolIsReportedWithoutProbingThePort(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	probe := newRecordingProbe(map[int]Protocol{6900: ProtocolTCP})
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    probe.probe,
		Declared: func() ([]Declaration, error) {
			return []Declaration{{Port: 6900, ServiceID: "ai.discobox.desktop", ServiceName: "Desktop", Protocol: ProtocolHTTP}}, nil
		},
	})

	watcher.tick(context.Background())
	watcher.tick(context.Background())

	if got := probe.callCount(6900); got != 0 {
		t.Fatalf("the port was probed %d times; a stated protocol must be believed, not verified", got)
	}
	port := snapshotByPort(t, watcher, 6900)
	if port.Protocol != ProtocolHTTP {
		t.Fatalf("protocol = %q, want the declared %q", port.Protocol, ProtocolHTTP)
	}
	if port.ServiceID != "ai.discobox.desktop" {
		t.Fatalf("serviceId = %q; a client matches the desktop on this", port.ServiceID)
	}
	if port.ServiceName != "Desktop" {
		t.Fatalf("serviceName = %q, want the declared display name", port.ServiceName)
	}
	if !port.Declared {
		t.Fatalf("a declared port is not marked declared: %+v", port)
	}
}

// A declaration that states nothing is ADR 0076's behavior unchanged: the port
// is listed, and probed. Only an image may claim what a port speaks, and the
// unset field must not be read as a claim of "".
func TestADeclarationWithoutAProtocolIsStillProbed(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	probe := newRecordingProbe(map[int]Protocol{5432: ProtocolTCP})
	for _, test := range []struct {
		name        string
		declaration Declaration
	}{
		{"zero value", Declaration{Port: 5432, ServiceID: "db", ServiceName: "Database"}},
		{"explicitly unknown", Declaration{Port: 5432, ServiceID: "db", ServiceName: "Database", Protocol: ProtocolUnknown}},
	} {
		t.Run(test.name, func(t *testing.T) {
			watcher := New(Config{
				UID:      1000,
				ProcRoot: fixture.root,
				Probe:    probe.probe,
				Declared: func() ([]Declaration, error) { return []Declaration{test.declaration}, nil },
			})
			watcher.tick(context.Background())
			port := snapshotByPort(t, watcher, 5432)
			if port.Protocol != ProtocolTCP {
				t.Fatalf("protocol = %q, want the probed %q", port.Protocol, ProtocolTCP)
			}
			if port.ServiceID != "db" {
				t.Fatalf("serviceId = %q, want the declared id", port.ServiceID)
			}
		})
	}
}

// A port both an image and the repository declare takes the image's protocol.
// The server orders the two sources for this, and it is what keeps a repository
// service that happens to name 6900 from putting the desktop back in the probe
// queue.
func TestTheFirstDeclarationOfAPortWins(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	probe := newRecordingProbe(nil)
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    probe.probe,
		Declared: func() ([]Declaration, error) {
			return []Declaration{
				{Port: 6900, ServiceID: "ai.discobox.desktop", ServiceName: "Desktop", Protocol: ProtocolHTTP},
				{Port: 6900, ServiceID: "something-else"},
			}, nil
		},
	})

	watcher.tick(context.Background())

	if got := probe.callCount(6900); got != 0 {
		t.Fatalf("the port was probed %d times; the image's declaration should have settled it", got)
	}
	if port := snapshotByPort(t, watcher, 6900); port.ServiceID != "ai.discobox.desktop" || port.Protocol != ProtocolHTTP {
		t.Fatalf("port = %+v, want the first declaration's id and protocol", port)
	}
}

// A bound UDP port is reported the tick it appears, as udp, and never
// connected to: there is no question a probe could safely ask it (ADR 0109).
func TestWatcherReportsABoundUDPPortWithoutProbingIt(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	fixture.writeUDP(udpRow(0, "00000000", "14E9", 1000, 51001))
	probe := newRecordingProbe(nil)
	watcher := New(Config{UID: 1000, ProcRoot: fixture.root, Probe: probe.probe})

	watcher.tick(context.Background())
	watcher.tick(context.Background())

	got := snapshotByPort(t, watcher, 5353)
	if got.Protocol != ProtocolUDP {
		t.Fatalf("protocol = %q, want udp", got.Protocol)
	}
	if len(got.Addresses) != 1 || got.Addresses[0] != "0.0.0.0" {
		t.Errorf("addresses = %v, want the wildcard bind", got.Addresses)
	}
	if calls := probe.callCount(5353); calls != 0 {
		t.Errorf("a UDP port was probed %d times, want never", calls)
	}
}

// DNS is the everyday case of a number serving on both transports. They are two
// ports: each is reported, each keeps its own protocol, and the TCP one is
// still classified by its probe.
func TestWatcherReportsTheTCPAndUDPPortsOfOneNumberSeparately(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "0035", 1000, 41001))
	fixture.writeUDP(udpRow(0, "0100007F", "0035", 1000, 51001))
	probe := newRecordingProbe(map[int]Protocol{53: ProtocolTCP})
	watcher := New(Config{UID: 1000, ProcRoot: fixture.root, Probe: probe.probe})

	watcher.tick(context.Background())

	snapshot := watcher.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("snapshot = %+v, want the TCP and UDP ports of 53", snapshot)
	}
	// UDP first, so a client that keys ports by number alone — every CLI
	// older than UDP discovery — keeps the TCP entry it read last.
	if snapshot[0].Port != 53 || snapshot[0].Protocol != ProtocolUDP {
		t.Errorf("first = %+v, want udp 53 first", snapshot[0])
	}
	if snapshot[1].Port != 53 || snapshot[1].Protocol != ProtocolTCP {
		t.Errorf("second = %+v, want tcp 53 second", snapshot[1])
	}
	if calls := probe.callCount(53); calls != 1 {
		t.Errorf("probe called %d times, want once, for the TCP port", calls)
	}

	// The UDP socket going away takes only the UDP port with it.
	fixture.writeUDP()
	watcher.tick(context.Background())
	if snapshot := watcher.Snapshot(); len(snapshot) != 1 || snapshot[0].Protocol != ProtocolTCP {
		t.Fatalf("snapshot = %+v, want only tcp 53 left", snapshot)
	}
}

// A declaration stating udp names the UDP port of its number — the remedy for
// a UDP server discovery cannot see, whether root holds it or it bound a
// number inside the ephemeral range.
func TestAUDPDeclarationDeclaresTheUDPPort(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "1F90", 1000, 41001))
	probe := newRecordingProbe(map[int]Protocol{8080: ProtocolHTTP})
	watcher := New(Config{
		UID:      1000,
		ProcRoot: fixture.root,
		Probe:    probe.probe,
		Declared: func() ([]Declaration, error) {
			return []Declaration{{Port: 8080, ServiceID: "game", ServiceName: "Game", Protocol: ProtocolUDP}}, nil
		},
	})

	watcher.tick(context.Background())

	snapshot := watcher.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("snapshot = %+v, want tcp 8080 observed and udp 8080 declared", snapshot)
	}
	udp, tcp := snapshot[0], snapshot[1]
	if tcp.Protocol != ProtocolHTTP || tcp.Declared || tcp.ServiceID != "" {
		t.Errorf("tcp 8080 = %+v, want the observed http port, undeclared", tcp)
	}
	if udp.Protocol != ProtocolUDP || !udp.Declared || udp.ServiceID != "game" || len(udp.Addresses) != 0 {
		t.Errorf("udp 8080 = %+v, want the declared udp port with no observed bind", udp)
	}
	if calls := probe.callCount(8080); calls != 1 {
		t.Errorf("probe called %d times, want once, for the TCP port only", calls)
	}
}

// The agent's own listener is a TCP socket. A sandbox process's UDP socket on
// the same number is not the agent, and is reported like any other.
func TestTheExcludedPortIsExcludedForTCPOnly(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "00000000", "0BBB", 1000, 41001))
	fixture.writeUDP(udpRow(0, "00000000", "0BBB", 1000, 51001))
	watcher := New(Config{
		UID:             1000,
		ExcludeTCPPorts: []int{3003},
		ProcRoot:        fixture.root,
		Probe:           newRecordingProbe(nil).probe,
		Declared: func() ([]Declaration, error) {
			return []Declaration{{Port: 3003, Protocol: ProtocolUDP, ServiceID: "game"}}, nil
		},
	})

	watcher.tick(context.Background())

	snapshot := watcher.Snapshot()
	if len(snapshot) != 1 || snapshot[0].Protocol != ProtocolUDP || snapshot[0].ServiceID != "game" {
		t.Fatalf("snapshot = %+v, want only the declared, observed udp 3003", snapshot)
	}
}
