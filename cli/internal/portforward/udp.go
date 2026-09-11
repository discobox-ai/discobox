package portforward

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// UDP has no connections, so a forward has to invent them (ADR 0109 §5). A
// flow is every datagram between one local peer address and the binding: its
// first datagram opens a tunnel, replies come back to that peer alone, and it
// ends when it has been quiet long enough.
//
// A tunnel per peer rather than one per binding is what gives each local client
// a source port of its own inside the remote. Sharing one would make every
// client the same client to the server there, and leave no way to tell whose
// reply is whose.

// flowIdleTimeout is how long a flow may go with no datagram in either
// direction before its tunnel is closed. Longer than QUIC's common 30-second
// idle timeout, so a quiet QUIC connection dies of its own timer rather than of
// this one; a flow that outlives it is re-dialed on its next datagram, with a
// new source port in the remote.
const flowIdleTimeout = 60 * time.Second

// flowRedialDelay is how long a peer whose tunnel could not be opened goes
// without one before its next datagram tries again. Without it a sender would
// open a tunnel per datagram for as long as the remote refused them.
const flowRedialDelay = time.Second

// flowQueue is how many datagrams a flow holds while its tunnel opens, or while
// the tunnel is slower than the peer. Past it they are dropped — UDP's own
// answer to a receiver that cannot keep up, and better than the one alternative,
// which is stalling every other peer of the binding behind this one.
const flowQueue = 64

// datagramLimit is the largest datagram read in either direction: the most a
// UDP payload can be, rounded up, so nothing is truncated on the way through.
const datagramLimit = 64 * 1024

type flow struct {
	peer net.Addr
	// socket is the binding socket the peer's datagrams arrived on, and so the
	// one its replies leave by: a loopback binding has one per family.
	socket net.PacketConn
	queue  chan []byte
	active atomic.Int64 // UnixNano of the last datagram either way
}

func (fl *flow) touch() { fl.active.Store(time.Now().UnixNano()) }

func (fl *flow) idleFor() time.Duration {
	return time.Since(time.Unix(0, fl.active.Load()))
}

// flowTable is a binding's live flows, by peer address.
//
// Its lock is what makes ending a flow safe against the datagram that arrives
// as it ends. A datagram is queued under it, and a flow leaves the table under
// it — and an idle flow leaves only if nothing is queued. So a datagram either
// reaches a flow that will carry it, or finds no flow and starts one; it is
// never handed to a flow that is already on its way out.
type flowTable struct {
	mu    sync.Mutex
	flows map[string]*flow
}

// deliver queues a datagram for the peer's flow, and returns the flow when it
// had to be started for it, for the caller to run. A flow that is behind by a
// full queue loses the datagram (see flowQueue).
func (t *flowTable) deliver(socket net.PacketConn, peer net.Addr, datagram []byte) (started *flow) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := peer.String()
	current, ok := t.flows[key]
	if !ok {
		current = &flow{peer: peer, socket: socket, queue: make(chan []byte, flowQueue)}
		current.touch()
		t.flows[key] = current
		started = current
	}
	select {
	case current.queue <- datagram:
	default:
	}
	return started
}

// retire takes a flow out of the table, so the peer's next datagram starts a
// new one. With onlyIfQuiet it refuses while a datagram is still queued for the
// flow, which is the flow being asked to carry it rather than to end.
func (t *flowTable) retire(fl *flow, onlyIfQuiet bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if onlyIfQuiet && len(fl.queue) > 0 {
		return false
	}
	if key := fl.peer.String(); t.flows[key] == fl {
		delete(t.flows, key)
	}
	return true
}

// relay reads the binding's sockets and hands each datagram to its peer's
// flow, starting one for a peer it has not seen. It is the UDP binding's accept
// loop, and ends the same way: when the sockets are closed.
func (f *Forwarder) relay(bound *binding) {
	defer f.wg.Done()
	table := &flowTable{flows: map[string]*flow{}}
	var readers sync.WaitGroup
	for _, socket := range bound.packets {
		readers.Add(1)
		go func() {
			defer readers.Done()
			f.readPackets(bound, socket, table)
		}()
	}
	readers.Wait()
}

func (f *Forwarder) readPackets(bound *binding, socket net.PacketConn, table *flowTable) {
	buf := make([]byte, datagramLimit)
	for {
		n, peer, err := socket.ReadFrom(buf)
		if err != nil {
			// An unconnected socket is not told about ICMP errors — Linux
			// reports them only to connected ones, and Go turns Windows'
			// SIO_UDP_CONNRESET off — so a failed read is a closed socket.
			return
		}
		if started := table.deliver(socket, peer, append([]byte(nil), buf[:n]...)); started != nil {
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.carry(bound, started, table)
			}()
		}
	}
}

// carry runs one flow: it opens the tunnel, sends the peer's datagrams down it
// and writes what comes back to the peer, and returns when the flow has been
// idle for the forwarder's flowIdle, the tunnel ends, or the forwarder closes.
// A flow whose tunnel cannot be opened holds its peer off for the forwarder's
// flowRedial and then returns, so the next datagram tries again.
//
// However it ends, the flow leaves the table before its tunnel is torn down, so
// the peer's next datagram is not queued behind a close handshake.
func (f *Forwarder) carry(bound *binding, fl *flow, table *flowTable) {
	f.mu.Lock()
	target := bound.target
	f.mu.Unlock()
	peer := fl.peer.String()
	f.emit(Event{Kind: Accepted, Target: target, Local: bound.local, Peer: peer})

	remote, err := f.dialer.DialPort(f.ctx, target)
	if err != nil {
		f.emit(Event{Kind: DialFailed, Target: target, Local: bound.local, Peer: peer, Err: err})
		select {
		case <-time.After(f.flowRedial):
		case <-f.ctx.Done():
		}
		table.retire(fl, false)
		return
	}

	replies := make(chan error, 1)
	go func() {
		buf := make([]byte, datagramLimit)
		for {
			n, err := remote.Read(buf)
			if err != nil {
				replies <- err
				return
			}
			fl.touch()
			_, _ = fl.socket.WriteTo(buf[:n], fl.peer)
		}
	}()

	idle := time.NewTimer(f.flowIdle)
	defer idle.Stop()
	for err == nil {
		select {
		case datagram := <-fl.queue:
			fl.touch()
			if _, werr := remote.Write(datagram); werr != nil {
				err = werr
			}
		case rerr := <-replies:
			// The remote ended the flow. Put the error back so the wait below
			// does not block on a reader that has already finished.
			replies <- rerr
			err = rerr
		case <-idle.C:
			if quiet := fl.idleFor(); quiet < f.flowIdle {
				idle.Reset(f.flowIdle - quiet)
				continue
			}
			// A datagram queued as the timer fired is this flow's to carry,
			// not a reason to end it.
			if !table.retire(fl, true) {
				idle.Reset(f.flowIdle)
				continue
			}
			err = errFlowIdle
		case <-f.ctx.Done():
			err = f.ctx.Err()
		}
	}
	table.retire(fl, false)
	// Closing the tunnel is what unblocks the reader, which is waited for so
	// that nothing a flow started outlives the forwarder's Close.
	_ = remote.Close()
	<-replies
	if errors.Is(err, errFlowIdle) || isClosed(err) {
		err = nil
	}
	f.emit(Event{Kind: Closed, Target: target, Local: bound.local, Peer: peer, Err: err})
}

// errFlowIdle is how carry's loop says it ended because nothing was said, which
// is how a flow is supposed to end and is reported as a clean close.
var errFlowIdle = errors.New("flow idle")
