// Package ports discovers the TCP ports the sandbox's own user processes are
// listening on and classifies each one as http, https, or something else, so
// the control plane can offer a forward onto a dev server without the user
// having to know its number or its protocol (see ADR 0046).
//
// Discovery is a procfs read filtered by uid, cheap enough to repeat on a short
// interval. Classification is not: the only way to learn what a socket speaks
// is to connect to it and write a request at it, so a socket is probed once and
// the answer is cached for its lifetime. That is why this is a standing watcher
// with a snapshot rather than a computation the status handler runs — everything
// else in that response is computed fresh per request.
package ports

import (
	"context"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultInterval is how often the watcher rescans procfs. Only newly appeared
// sockets cost anything beyond the two file reads, so this is set by how quickly
// a port should show up rather than by scan cost — well inside the 15s cadence
// pool-agent polls status on (ADR 0030), so a port is rarely more than one
// control-plane poll old.
const DefaultInterval = 5 * time.Second

// probeConcurrency bounds how many listeners are probed at once. A sandbox that
// just started a compose stack can bring up a dozen ports in one tick, and each
// probe is a connection into a process that may be slow to answer.
const probeConcurrency = 8

// Port is one TCP port the sandbox user is listening on, with what it turned
// out to speak.
type Port struct {
	Port int `json:"port"`
	// Addresses are every local address bound to this port, as
	// netip.Addr.String renders them: "0.0.0.0"/"::" for a wildcard bind,
	// "127.0.0.1" for a loopback-only one. A port is reachable from inside the
	// sandbox's network namespace in every case — which is where a forward
	// would dial from (ADR 0024's tcp/attach) — so this describes the bind, not
	// the reachability.
	Addresses []string `json:"addresses,omitempty"`
	Protocol  Protocol `json:"protocol"`
	// FirstSeenAt is when this port was first observed listening, and it
	// survives a restart of whatever is behind it as long as the port itself
	// never went away between two scans.
	FirstSeenAt time.Time `json:"firstSeenAt"`
}

// Config is what a Watcher needs to know about the sandbox it runs in.
type Config struct {
	// UID owns the sockets that count. This is the identity execs and terminals
	// resolve to, not a guess: pass what execs.Manager.ResolveUser returned, or
	// the agent's own uid when the manifest names nobody, since that is what an
	// exec then inherits (ADR 0025 §5).
	UID int64
	// ExcludePorts are ports never reported however they are bound. It exists
	// for sandbox-agent's own listener, which the uid filter does not exclude
	// when the sandbox user is root.
	ExcludePorts []int
	// ProcRoot defaults to /proc. Tests point it at a fixture directory.
	ProcRoot string
	// Interval defaults to DefaultInterval.
	Interval time.Duration
	// Probe defaults to probing the target for real. Tests replace it.
	Probe  func(context.Context, netip.AddrPort) Protocol
	Logger *slog.Logger
}

// Watcher keeps the current listening-port snapshot. The zero value is not
// usable; call New. A nil *Watcher answers Snapshot with nothing, so a server
// built without one (a test router) needs no nil check at the call site.
type Watcher struct {
	uid      int64
	exclude  map[int]struct{}
	procRoot string
	interval time.Duration
	probe    func(context.Context, netip.AddrPort) Protocol
	logger   *slog.Logger

	mu       sync.Mutex
	state    map[int]*portState
	snapshot []Port
}

// portState is what the watcher remembers about one port between ticks. The
// inode key is what makes the cached protocol safe to keep: it changes whenever
// the socket behind the port is replaced, which is the only event that can
// change the answer.
type portState struct {
	firstSeenAt time.Time
	inodeKey    string
	protocol    Protocol
	addresses   []string
	target      netip.AddrPort
}

func New(cfg Config) *Watcher {
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Probe == nil {
		cfg.Probe = Probe
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	exclude := make(map[int]struct{}, len(cfg.ExcludePorts))
	for _, port := range cfg.ExcludePorts {
		exclude[port] = struct{}{}
	}
	return &Watcher{
		uid:      cfg.UID,
		exclude:  exclude,
		procRoot: cfg.ProcRoot,
		interval: cfg.Interval,
		probe:    cfg.Probe,
		logger:   cfg.Logger,
		state:    map[int]*portState{},
	}
}

// Run scans immediately and then on the configured interval until ctx is done.
// It never returns an error: a failed scan is this tick's gap, not a fault, and
// on a platform with no procfs table to read it is simply always empty.
func (w *Watcher) Run(ctx context.Context) {
	if w == nil {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// Snapshot is the most recent observation, ordered by port.
func (w *Watcher) Snapshot() []Port {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Port(nil), w.snapshot...)
}

func (w *Watcher) tick(ctx context.Context) {
	listeners, err := scanListeners(w.procRoot, w.uid)
	if err != nil {
		w.logger.Debug("sandbox agent listening port scan failed", "error", err)
		return
	}
	pending := w.observe(listeners, time.Now().UTC())
	// The snapshot is published before the probes run so a port shows up in the
	// tick it appeared in, carrying "unknown" until its probe answers, rather
	// than being withheld for as long as a slow server takes to reply.
	w.publish()
	if len(pending) == 0 {
		return
	}
	w.runProbes(ctx, pending)
	w.publish()
}

// observe folds a scan into the remembered state and returns the ports whose
// protocol has to be established: newly appeared ones, ones whose socket was
// replaced, and ones whose last probe could not reach them.
func (w *Watcher) observe(listeners []listener, now time.Time) []int {
	grouped := map[int][]listener{}
	for _, entry := range listeners {
		if _, skip := w.exclude[entry.Port]; skip {
			continue
		}
		grouped[entry.Port] = append(grouped[entry.Port], entry)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	next := make(map[int]*portState, len(grouped))
	var pending []int
	for port, entries := range grouped {
		state := &portState{
			firstSeenAt: now,
			inodeKey:    inodeKey(entries),
			protocol:    ProtocolUnknown,
			addresses:   addressStrings(entries),
			target:      netip.AddrPortFrom(probeAddr(entries), uint16(port)),
		}
		if previous, ok := w.state[port]; ok {
			state.firstSeenAt = previous.firstSeenAt
			if previous.inodeKey == state.inodeKey {
				state.protocol = previous.protocol
			}
		}
		if state.protocol == ProtocolUnknown {
			pending = append(pending, port)
		}
		next[port] = state
	}
	w.state = next
	sort.Ints(pending)
	return pending
}

func (w *Watcher) runProbes(ctx context.Context, pending []int) {
	targets := make(map[int]probeTarget, len(pending))
	w.mu.Lock()
	for _, port := range pending {
		if state, ok := w.state[port]; ok {
			targets[port] = probeTarget{addr: state.target, inodeKey: state.inodeKey}
		}
	}
	w.mu.Unlock()

	slots := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for port, target := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			w.record(port, target, w.probe(ctx, target.addr))
		}()
	}
	wg.Wait()
}

type probeTarget struct {
	addr     netip.AddrPort
	inodeKey string
}

// record stores a probe result only if the socket it describes is still the one
// on that port: a server that restarted while its old socket was being probed
// must not inherit the old answer.
func (w *Watcher) record(port int, target probeTarget, protocol Protocol) {
	w.mu.Lock()
	defer w.mu.Unlock()
	state, ok := w.state[port]
	if !ok || state.inodeKey != target.inodeKey {
		return
	}
	state.protocol = protocol
}

func (w *Watcher) publish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	snapshot := make([]Port, 0, len(w.state))
	for port, state := range w.state {
		snapshot = append(snapshot, Port{
			Port:        port,
			Addresses:   state.addresses,
			Protocol:    state.protocol,
			FirstSeenAt: state.firstSeenAt,
		})
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].Port < snapshot[j].Port })
	w.snapshot = snapshot
}

// inodeKey identifies the set of sockets currently behind a port. A port bound
// on both IPv4 and IPv6 has two, and either being replaced is a reason to probe
// again.
func inodeKey(entries []listener) string {
	inodes := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		inodes = append(inodes, entry.Inode)
	}
	sort.Slice(inodes, func(i, j int) bool { return inodes[i] < inodes[j] })
	parts := make([]string, 0, len(inodes))
	for _, inode := range inodes {
		parts = append(parts, strconv.FormatUint(inode, 10))
	}
	return strings.Join(parts, ",")
}

func addressStrings(entries []listener) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		text := entry.Addr.String()
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
	}
	sort.Strings(out)
	return out
}

// probeAddr picks where to dial a port that may be bound several times over. A
// wildcard bind is probed on loopback rather than on the wildcard address
// itself, which is not a destination; a port bound only to some specific
// interface address is probed there, since loopback would not answer.
func probeAddr(entries []listener) netip.Addr {
	var (
		wildcard4, wildcard6 bool
		loopback, specific   netip.Addr
	)
	for _, entry := range entries {
		switch {
		case entry.Addr.IsUnspecified() && entry.Addr.Is4():
			wildcard4 = true
		case entry.Addr.IsUnspecified():
			wildcard6 = true
		case entry.Addr.IsLoopback():
			if !loopback.IsValid() {
				loopback = entry.Addr
			}
		default:
			if !specific.IsValid() {
				specific = entry.Addr
			}
		}
	}
	switch {
	case wildcard4:
		return netip.AddrFrom4([4]byte{127, 0, 0, 1})
	case wildcard6:
		return netip.IPv6Loopback()
	case loopback.IsValid():
		return loopback
	default:
		return specific
	}
}
