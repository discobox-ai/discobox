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
func (e Event) String() string {
	remote := fmt.Sprintf("%d", e.Target.Port)
	switch e.Kind {
	case Bound:
		if e.Local == e.Target.Port {
			return fmt.Sprintf("listening on %d -> sandbox %s%s", e.Local, remote, protocolSuffix(e.Target))
		}
		return fmt.Sprintf("listening on %d -> sandbox %s%s (%s was taken)", e.Local, remote, protocolSuffix(e.Target), remote)
	case BindFailed:
		return fmt.Sprintf("sandbox %s could not be bound locally: %v", remote, e.Err)
	case Gone:
		return fmt.Sprintf("sandbox %s stopped listening; %d is held open", remote, e.Local)
	case Back:
		return fmt.Sprintf("sandbox %s is listening again on %d", remote, e.Local)
	case Accepted:
		return fmt.Sprintf("%d -> sandbox %s: connection from %s", e.Local, remote, e.Peer)
	case DialFailed:
		return fmt.Sprintf("%d -> sandbox %s: %v", e.Local, remote, e.Err)
	case Closed:
		if e.Err != nil {
			return fmt.Sprintf("%d -> sandbox %s: connection from %s ended: %v", e.Local, remote, e.Peer, e.Err)
		}
		return fmt.Sprintf("%d -> sandbox %s: connection from %s ended", e.Local, remote, e.Peer)
	default:
		return fmt.Sprintf("%s sandbox %s", e.Kind, remote)
	}
}

func protocolSuffix(target Target) string {
	switch target.Protocol {
	case "", "unknown", "tcp":
		return ""
	default:
		return " (" + target.Protocol + ")"
	}
}
