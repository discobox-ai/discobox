// Package portforward keeps a set of local TCP listeners and UDP sockets in
// sync with the ports a remote announces, forwarding each one over a transport
// the caller supplies.
//
// It knows nothing about sandboxes or websockets: a caller hands it a Dialer
// that turns a Target into a net.Conn and a listing of targets whenever the
// listing changes, and it owns the rest — picking a local port near the remote
// one, accepting connections (or, for UDP, telling flows apart), splicing
// them, and reporting what it did. That is what lets the same forwarder back
// `discobox proxy` and the launcher's port list without either of them owning
// the mechanics.
package portforward

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"
)

// DefaultBindAddress is the address bindings listen on. Loopback is the
// default deliberately: a forwarded port is an unauthenticated door onto
// something inside a sandbox, and it should not be reachable off-host unless
// the caller says so.
const DefaultBindAddress = "127.0.0.1"

// DefaultDialHost is the host a target is dialed at from inside the remote
// when it names none, and the host any loopback or wildcard bind is dialed at.
//
// It is a name, not an address, on purpose: a listener bound only to ::1 does
// not answer on 127.0.0.1, and one bound only to 127.0.0.1 does not answer on
// ::1. Dialing "localhost" hands the choice to the remote's dialer, which
// tries both families and keeps whichever connects.
const DefaultDialHost = "localhost"

// Network is the transport a port is forwarded over. A TCP port and a UDP port
// with the same number are two targets, bound locally in two port spaces.
type Network string

const (
	TCP Network = "tcp"
	UDP Network = "udp"
)

// Target is one remote port to expose locally.
type Target struct {
	// Network is the port's transport. Empty means TCP.
	Network Network
	// Host is dialed from inside the remote's network namespace, not from
	// here. Empty means DefaultDialHost.
	Host string
	// Port is the remote port, and the local port the search starts from.
	Port int
	// Protocol is what the port was observed to speak ("http", "tcp",
	// "unknown"). It is carried through to events and bindings for display
	// only; forwarding is the same either way.
	Protocol string
}

func (t Target) dialHost() string {
	if t.Host == "" {
		return DefaultDialHost
	}
	return t.Host
}

func (t Target) network() Network {
	if t.Network == "" {
		return TCP
	}
	return t.Network
}

// key is what a target is bound under: a TCP 53 and a UDP 53 are two bindings.
func (t Target) key() targetKey {
	return targetKey{network: t.network(), port: t.Port}
}

type targetKey struct {
	network Network
	port    int
}

func (k targetKey) less(other targetKey) bool {
	if k.port != other.port {
		return k.port < other.port
	}
	return k.network == TCP && other.network == UDP
}

// Dialer opens a connection to a target's port inside the remote.
//
// For a TCP target the returned conn is used as an ordinary net.Conn.
// Implementing CloseWrite on it lets a TCP half-close survive the trip;
// without it, a client that closes its write side only signals end-of-request
// when it closes outright.
//
// For a UDP target it is used the way a connected UDP socket is: each Write is
// one datagram to the target, and each Read returns one datagram from it. A
// conn that merged or split datagrams would corrupt every protocol that relies
// on their boundaries, which is all of them.
type Dialer interface {
	DialPort(ctx context.Context, target Target) (net.Conn, error)
}

// Binding is one local port standing in for a remote port.
type Binding struct {
	Target Target
	// Local is the local port that was actually bound, which is Target.Port
	// when it was free and the nearest one above it otherwise.
	Local int
	// Active reports whether the remote still announces this port. A binding
	// outlives the port going away — see Forwarder.Set.
	Active bool
}

// Options configure a Forwarder. Only Dialer is required.
type Options struct {
	Dialer Dialer
	// BindAddress is the local address bindings listen on. Empty means
	// DefaultBindAddress.
	BindAddress string
	// Search is how many ports above a remote port the search for a free local
	// one covers. Zero means DefaultSearch. Ignored when Exact is set.
	Search int
	// Exact binds every target at its own number or not at all.
	//
	// The nearest-free search exists because a forwarded dev server is useful
	// at whatever number it lands on — the caller prints it and you open that.
	// A port an outside service sends a browser back to is not: the redirect
	// URI names one number, so a forward that quietly moved to the next free
	// port would answer nothing while reporting itself as bound. A target that
	// cannot take its own port reports BindFailed and stays unbound, which is
	// the honest answer and the one a caller can tell the user about.
	Exact bool
	// Observe receives every status change. It is called from the forwarder's
	// own goroutines and from Set, possibly concurrently, so an observer that
	// writes anywhere shared must serialize itself.
	Observe func(Event)
}

// Forwarder owns the local sockets for a set of remote ports.
type Forwarder struct {
	ctx     context.Context
	cancel  context.CancelFunc
	dialer  Dialer
	address string
	search  int
	exact   bool
	observe func(Event)
	// flowIdle and flowRedial are flowIdleTimeout and flowRedialDelay, held
	// here so a test can shorten them.
	flowIdle   time.Duration
	flowRedial time.Duration

	mu sync.Mutex
	// bound is keyed by remote port and network. Entries are sticky: see Set.
	bound map[targetKey]*binding
	// bindFailed is the last bind error per remote port, kept so a retry that
	// keeps failing the same way does not report itself on every listing.
	bindFailed map[targetKey]string
	closed     bool

	wg sync.WaitGroup
}

// binding is one local port standing in for a remote one: a listener for a
// TCP target, packet sockets for a UDP one, and never both.
//
// A UDP binding on loopback is a socket per loopback family, on the same port
// (see listenPackets). TCP needs no such pair: a TCP client of "localhost"
// tries each address until one connects, and a UDP client has no connect to
// fail, so it would send to ::1 and hear nothing.
type binding struct {
	target   Target
	local    int
	listener net.Listener
	packets  []net.PacketConn
	active   bool
}

func (b *binding) close() {
	if b.listener != nil {
		_ = b.listener.Close()
	}
	for _, socket := range b.packets {
		_ = socket.Close()
	}
}

// New starts a forwarder. It holds no listeners until Set names some, and it
// stops when ctx is canceled or Close is called.
func New(ctx context.Context, opts Options) *Forwarder {
	ctx, cancel := context.WithCancel(ctx)
	forwarder := &Forwarder{
		ctx:        ctx,
		cancel:     cancel,
		dialer:     opts.Dialer,
		address:    opts.BindAddress,
		search:     opts.Search,
		exact:      opts.Exact,
		observe:    opts.Observe,
		flowIdle:   flowIdleTimeout,
		flowRedial: flowRedialDelay,
		bound:      map[targetKey]*binding{},
		bindFailed: map[targetKey]string{},
	}
	if forwarder.address == "" {
		forwarder.address = DefaultBindAddress
	}
	if forwarder.search <= 0 {
		forwarder.search = DefaultSearch
	}
	// A canceled context has to reach the accept loops, which are blocked in
	// Accept and cannot select on it.
	go func() {
		<-ctx.Done()
		forwarder.closeListeners()
	}()
	return forwarder
}

// Set reconciles the bindings against the ports the remote now announces.
//
// A port that appears is bound; a port that goes away keeps its binding and is
// marked inactive rather than unbound. That is deliberate: a dev server
// restarting drops off the listing for a moment, and a local port that moved
// while the user had the URL open would be worse than one that briefly refuses
// to connect. Bindings are released by Close.
func (f *Forwarder) Set(targets []Target) {
	wanted := make(map[targetKey]Target, len(targets))
	for _, target := range targets {
		if target.Port < 1 || target.Port > 65535 {
			continue
		}
		// The dialer is handed a host it can dial and a network it does not
		// have to default, so the defaults land here rather than in each of
		// them.
		target.Host = target.dialHost()
		target.Network = target.network()
		wanted[target.key()] = target
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	var events []Event
	for key, bound := range f.bound {
		target, ok := wanted[key]
		if !ok {
			if bound.active {
				bound.active = false
				events = append(events, Event{Kind: Gone, Target: bound.target, Local: bound.local})
			}
			continue
		}
		bound.target = target
		if !bound.active {
			bound.active = true
			events = append(events, Event{Kind: Back, Target: target, Local: bound.local})
		}
	}
	for key := range f.bindFailed {
		if _, ok := wanted[key]; !ok {
			delete(f.bindFailed, key)
		}
	}
	for _, key := range sortedKeys(wanted) {
		if _, ok := f.bound[key]; ok {
			continue
		}
		target := wanted[key]
		bound, err := f.bind(target)
		if err != nil {
			// Retried on the next listing, but only reported when the reason
			// changes: a listing arrives on a poll interval, and a port that
			// cannot be bound would otherwise repeat itself forever.
			if f.bindFailed[key] != err.Error() {
				f.bindFailed[key] = err.Error()
				events = append(events, Event{Kind: BindFailed, Target: target, Err: err})
			}
			continue
		}
		delete(f.bindFailed, key)
		f.bound[key] = bound
		events = append(events, Event{Kind: Bound, Target: target, Local: bound.local})
		f.wg.Add(1)
		if len(bound.packets) > 0 {
			go f.relay(bound)
		} else {
			go f.accept(bound)
		}
	}
	f.mu.Unlock()

	for _, event := range events {
		f.emit(event)
	}
}

// bind opens the local socket standing in for a remote port — a listener for
// TCP, a packet socket for UDP — at its own number when the forwarder is exact,
// otherwise at the nearest free one.
func (f *Forwarder) bind(target Target) (*binding, error) {
	bound := &binding{target: target, active: true}
	var err error
	switch {
	case target.Network == UDP && f.exact:
		bound.packets, err = listenPacketExact(f.ctx, f.address, target.Port)
	case target.Network == UDP:
		bound.packets, err = listenPacketNearest(f.ctx, f.address, target.Port, f.search)
	case f.exact:
		bound.listener, err = listenExact(f.ctx, f.address, target.Port)
	default:
		bound.listener, err = listenNearest(f.ctx, f.address, target.Port, f.search)
	}
	if err != nil {
		return nil, err
	}
	if len(bound.packets) > 0 {
		bound.local = addrPort(bound.packets[0].LocalAddr())
	} else {
		bound.local = addrPort(bound.listener.Addr())
	}
	return bound, nil
}

// Bindings is what is bound right now, in remote port order, with a number's
// TCP binding before its UDP one.
func (f *Forwarder) Bindings() []Binding {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Binding, 0, len(f.bound))
	for _, bound := range f.bound {
		out = append(out, Binding{Target: bound.target, Local: bound.local, Active: bound.active})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target.key().less(out[j].Target.key()) })
	return out
}

// Close releases every listener and waits for the connections still in flight
// to finish being torn down.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.closeListeners()
	f.cancel()
	f.wg.Wait()
	return nil
}

func (f *Forwarder) closeListeners() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, bound := range f.bound {
		bound.close()
	}
}

func (f *Forwarder) accept(bound *binding) {
	defer f.wg.Done()
	for {
		conn, err := bound.listener.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.serve(bound, conn)
		}()
	}
}

func (f *Forwarder) serve(bound *binding, local net.Conn) {
	defer local.Close()

	f.mu.Lock()
	target := bound.target
	f.mu.Unlock()

	peer := local.RemoteAddr().String()
	f.emit(Event{Kind: Accepted, Target: target, Local: bound.local, Peer: peer})

	remote, err := f.dialer.DialPort(f.ctx, target)
	if err != nil {
		f.emit(Event{Kind: DialFailed, Target: target, Local: bound.local, Peer: peer, Err: err})
		return
	}
	defer remote.Close()

	err = splice(f.ctx, local, remote)
	f.emit(Event{Kind: Closed, Target: target, Local: bound.local, Peer: peer, Err: err})
}

func (f *Forwarder) emit(event Event) {
	if f.observe == nil {
		return
	}
	f.observe(event)
}

func addrPort(addr net.Addr) int {
	switch addr := addr.(type) {
	case *net.TCPAddr:
		return addr.Port
	case *net.UDPAddr:
		return addr.Port
	default:
		return 0
	}
}

func sortedKeys(targets map[targetKey]Target) []targetKey {
	keys := make([]targetKey, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
	return keys
}
