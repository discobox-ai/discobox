// Package portforward keeps a set of local TCP listeners in sync with the
// ports a remote announces, forwarding each one over a transport the caller
// supplies.
//
// It knows nothing about sandboxes or websockets: a caller hands it a Dialer
// that turns a Target into a net.Conn and a listing of targets whenever the
// listing changes, and it owns the rest — picking a local port near the remote
// one, accepting connections, splicing them, and reporting what it did. That
// is what lets the same forwarder back `discobox proxy` and the launcher's port
// list without either of them owning the mechanics.
package portforward

import (
	"context"
	"net"
	"sort"
	"sync"
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

// Target is one remote port to expose locally.
type Target struct {
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

// Dialer opens a connection to a target's port inside the remote.
//
// The returned conn is used as an ordinary net.Conn. Implementing CloseWrite
// on it lets a TCP half-close survive the trip; without it, a client that
// closes its write side only signals end-of-request when it closes outright.
type Dialer interface {
	DialPort(ctx context.Context, target Target) (net.Conn, error)
}

// Binding is one local listener standing in for a remote port.
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

// Forwarder owns the local listeners for a set of remote ports.
type Forwarder struct {
	ctx     context.Context
	cancel  context.CancelFunc
	dialer  Dialer
	address string
	search  int
	exact   bool
	observe func(Event)

	mu sync.Mutex
	// bound is keyed by remote port. Entries are sticky: see Set.
	bound map[int]*binding
	// bindFailed is the last bind error per remote port, kept so a retry that
	// keeps failing the same way does not report itself on every listing.
	bindFailed map[int]string
	closed     bool

	wg sync.WaitGroup
}

type binding struct {
	target   Target
	local    int
	listener net.Listener
	active   bool
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
		bound:      map[int]*binding{},
		bindFailed: map[int]string{},
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
	wanted := make(map[int]Target, len(targets))
	for _, target := range targets {
		if target.Port < 1 || target.Port > 65535 {
			continue
		}
		// The dialer is handed a host it can dial, not one it has to
		// interpret, so the default lands here rather than in each of them.
		target.Host = target.dialHost()
		wanted[target.Port] = target
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	var events []Event
	for port, bound := range f.bound {
		target, ok := wanted[port]
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
	for port := range f.bindFailed {
		if _, ok := wanted[port]; !ok {
			delete(f.bindFailed, port)
		}
	}
	for _, port := range sortedPorts(wanted) {
		if _, ok := f.bound[port]; ok {
			continue
		}
		target := wanted[port]
		listener, err := f.listen(port)
		if err != nil {
			// Retried on the next listing, but only reported when the reason
			// changes: a listing arrives on a poll interval, and a port that
			// cannot be bound would otherwise repeat itself forever.
			if f.bindFailed[port] != err.Error() {
				f.bindFailed[port] = err.Error()
				events = append(events, Event{Kind: BindFailed, Target: target, Err: err})
			}
			continue
		}
		delete(f.bindFailed, port)
		bound := &binding{target: target, local: listenerPort(listener), listener: listener, active: true}
		f.bound[port] = bound
		events = append(events, Event{Kind: Bound, Target: target, Local: bound.local})
		f.wg.Add(1)
		go f.accept(bound)
	}
	f.mu.Unlock()

	for _, event := range events {
		f.emit(event)
	}
}

// listen opens the local listener standing in for a remote port: its own
// number when the forwarder is exact, otherwise the nearest free one.
func (f *Forwarder) listen(port int) (net.Listener, error) {
	if f.exact {
		return listenExact(f.ctx, f.address, port)
	}
	return listenNearest(f.ctx, f.address, port, f.search)
}

// Bindings is what is bound right now, in remote port order.
func (f *Forwarder) Bindings() []Binding {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Binding, 0, len(f.bound))
	for _, bound := range f.bound {
		out = append(out, Binding{Target: bound.target, Local: bound.local, Active: bound.active})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target.Port < out[j].Target.Port })
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
		_ = bound.listener.Close()
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

func listenerPort(listener net.Listener) int {
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

func sortedPorts(targets map[int]Target) []int {
	ports := make([]int, 0, len(targets))
	for port := range targets {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}
