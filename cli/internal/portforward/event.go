package portforward

import "fmt"

// Kind is what a status Event reports.
type Kind string

const (
	// Bound is a new remote port now reachable at a local one.
	Bound Kind = "bound"
	// BindFailed is a remote port that could not be given a local port at all.
	BindFailed Kind = "bind-failed"
	// Gone is a bound port the remote stopped announcing. The local port stays
	// bound; connections to it fail until the port comes Back.
	Gone Kind = "gone"
	// Back is a Gone port the remote announces again, at the same local port.
	Back Kind = "back"
	// Accepted is a local connection taken on a binding.
	Accepted Kind = "accepted"
	// DialFailed is an accepted connection the remote end would not take.
	DialFailed Kind = "dial-failed"
	// Closed is a forwarded connection that ended, with the error that ended
	// it when it was not a clean close.
	Closed Kind = "closed"
)

// Event is one status change. Local is the local port involved, which is zero
// only for BindFailed; Peer is the local client's address and is set for the
// per-connection kinds.
type Event struct {
	Kind   Kind
	Target Target
	Local  int
	Peer   string
	Err    error
}

// String is the one-line form the CLI prints. It is here rather than in the
// command so every frontend describes the same event the same way.
//
// A UDP port is written `5353/udp` on both sides, and the per-connection kinds
// say "flow" for it: a flow is what the forward made of datagrams from one
// peer (ADR 0109 §5), and calling it a connection would claim a handshake
// that never happened.
func (e Event) String() string {
	remote := portLabel(e.Target, e.Target.Port)
	local := portLabel(e.Target, e.Local)
	from := "connection from"
	if e.Target.network() == UDP {
		from = "flow from"
	}
	switch e.Kind {
	case Bound:
		if e.Local == e.Target.Port {
			return fmt.Sprintf("listening on %s -> discobox %s%s", local, remote, protocolSuffix(e.Target))
		}
		return fmt.Sprintf("listening on %s -> discobox %s%s (%s was taken)", local, remote, protocolSuffix(e.Target), remote)
	case BindFailed:
		return fmt.Sprintf("discobox %s could not be bound locally: %v", remote, e.Err)
	case Gone:
		return fmt.Sprintf("discobox %s stopped listening; %s is held open", remote, local)
	case Back:
		return fmt.Sprintf("discobox %s is listening again on %s", remote, local)
	case Accepted:
		return fmt.Sprintf("%s -> discobox %s: %s %s", local, remote, from, e.Peer)
	case DialFailed:
		return fmt.Sprintf("%s -> discobox %s: %v", local, remote, e.Err)
	case Closed:
		if e.Err != nil {
			return fmt.Sprintf("%s -> discobox %s: %s %s ended: %v", local, remote, from, e.Peer, e.Err)
		}
		return fmt.Sprintf("%s -> discobox %s: %s %s ended", local, remote, from, e.Peer)
	default:
		return fmt.Sprintf("%s discobox %s", e.Kind, remote)
	}
}

// portLabel is a port number as an event names it: bare for TCP, the default
// everyone reads a bare number as, and `/udp` for the other one.
func portLabel(target Target, port int) string {
	if target.network() == UDP {
		return fmt.Sprintf("%d/udp", port)
	}
	return fmt.Sprintf("%d", port)
}

// protocolSuffix names what a port speaks when that says more than its label
// already does. tcp and unknown say nothing a bare number does not, and udp is
// already in the label.
func protocolSuffix(target Target) string {
	switch target.Protocol {
	case "", "unknown", "tcp", "udp":
		return ""
	default:
		return " (" + target.Protocol + ")"
	}
}
