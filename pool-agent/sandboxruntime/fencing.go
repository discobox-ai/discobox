package sandboxruntime

import (
	"errors"
	"fmt"
	"slices"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Fencing is how a driver holds a sandbox's machine to its pool, and so how
// it carries a pool at all (ADR 26-11-11-963 §5, ADR 26-11-16-619 §1). It is
// decided from what disco-vm reports of the driver, never from its name, by
// PoolFencing, which the server's pool create and machine, and the pool
// agent's hop and runtime, all ask.
type Fencing string

const (
	// FencedOnNetwork isolates a sandbox's machine on a network it shares
	// only with its pool's machine, stripped of what the driver gives a
	// machine (boxd).
	FencedOnNetwork Fencing = "network"
	// FencedAtEdge is the third way: a machine on no network of the
	// caller's, holding nothing of the account, its egress fenced to the
	// pool's public host with no resolver answering, and its URL answering
	// only a credential the provider's edge checks (archil).
	FencedAtEdge Fencing = "edge"
)

// PoolFencing is how a driver whose capabilities are caps fences a pool's
// sandboxes, or why it carries no pool: what it lacks for each way. A driver
// that does both is taken on its networks. A held driver's way, a sandbox
// with no network device and the server's peer forward (963 §5), is not
// asked here: no pool reaches one yet.
func PoolFencing(driver string, caps machine.Capabilities) (Fencing, error) {
	// Every way strips the account from the machine, and fences its egress
	// to a list: the host its hop is reached at.
	var both []error
	if !caps.Bare.Strips() {
		both = append(both, errors.New("cannot make a sandbox's machine hold nothing of its provider's account"))
	}
	if !slices.Contains(caps.Egress, machine.EgressAllow) {
		both = append(both, errors.New("cannot fence a sandbox's machine's egress to its pool"))
	}
	if len(both) == 0 && caps.Networks {
		return FencedOnNetwork, nil
	}
	var edge []error
	if !slices.Contains(caps.Egress, machine.EgressCloseDNS) {
		edge = append(edge, errors.New("no resolver closed to its guest"))
	}
	if !caps.URLs.Credential {
		edge = append(edge, errors.New("no URL that answers only a credential its provider checks"))
	}
	if len(both) == 0 && len(edge) == 0 {
		return FencedAtEdge, nil
	}
	missing := both
	if len(both) == 0 {
		// Only the way-specific lacks remain: one for each way.
		missing = []error{
			errors.New("cannot place a sandbox's machine on a network of its pool's"),
			fmt.Errorf("cannot fence one at its provider's edge: %w", errors.Join(edge...)),
		}
	}
	return "", fmt.Errorf("the %s driver cannot carry a machine pool: %w", driver, errors.Join(missing...))
}
