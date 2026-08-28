package proxyagent

import (
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/secretformat"
)

// activationTTL is how long one ephemeral sentinel stays usable. It is the
// "use clock" of ADR 0031 §3, owned entirely here: the control plane's grant
// TTL is the consent clock and knows nothing about activations.
//
// It is minutes rather than hours because the whole point of an ephemeral
// sentinel is that a leaked one is worth almost nothing. A command that takes
// longer than this and needs the credential again asks for it again — `get`
// returns expiresAt precisely so an agent re-gets instead of hoarding.
const activationTTL = 5 * time.Minute

// activation is one recorded use: an ephemeral sentinel the sandbox was handed,
// bound to the stable sentinel it translates back to, the approved use it was
// taken under, and the command the caller declared it was about to run.
//
// The declaration is context and audit trail, never a trust anchor. Enforcement
// happens against the actual outbound request at swap time, so a sandbox that
// lies about its command gains nothing by it.
type activation struct {
	SandboxID string
	Sentinel  string // the ephemeral sentinel handed to the sandbox
	Stable    string // the sandbox's stable sentinel, which the control plane knows
	UseID     string
	Host      string
	Command   []string
	ExpiresAt time.Time
}

func (a activation) live(now time.Time) bool { return now.Before(a.ExpiresAt) }

// activations is the pool-local registry of live ephemeral sentinels.
//
// It is deliberately in-memory and deliberately not control-plane state
// (ADR 0031, rejected alternatives): activations are minutes-scale, high-churn,
// per-sandbox records, and this process already owns both the sentinel registry
// the proxy matches against and the resolver the proxy calls. Losing them on a
// restart costs a dead sentinel and one fresh `get` — the failure mode is
// closed, which is the right direction for something that authorizes a
// credential swap.
type activations struct {
	mu sync.Mutex
	// byEphemeral is keyed by the ephemeral sentinel because that is what the
	// resolver has in hand: the proxy matched a string in an outbound request
	// and asks who it belongs to.
	byEphemeral map[string]activation
	// onChange republishes the proxy's sentinel set. An ephemeral sentinel the
	// proxy has not been told about is never matched at all, so registration
	// has to happen before `get` returns.
	onChange func()
	now      func() time.Time
}

func newActivations() *activations {
	return &activations{byEphemeral: map[string]activation{}, now: time.Now}
}

// setChangeHandler installs the callback that republishes the proxy config.
func (a *activations) setChangeHandler(onChange func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onChange = onChange
}

// mint records a fresh activation and returns it. format shapes the ephemeral
// sentinel so it byte-mimics a real key of the same provider — the same rule
// the stable sentinel was minted under, so nothing downstream can tell the two
// apart by looking.
func (a *activations) mint(sandboxID, stableSentinel, useID, host, format string, command []string) (activation, error) {
	sentinel, err := secretformat.MintSentinel(format)
	if err != nil {
		return activation{}, err
	}
	record := activation{
		SandboxID: sandboxID,
		Sentinel:  sentinel,
		Stable:    stableSentinel,
		UseID:     useID,
		Host:      strings.TrimSpace(host),
		Command:   append([]string(nil), command...),
	}

	a.mu.Lock()
	now := a.now()
	record.ExpiresAt = now.Add(activationTTL)
	a.pruneLocked(now)
	a.byEphemeral[sentinel] = record
	onChange := a.onChange
	a.mu.Unlock()

	// Publish before returning: the caller hands this sentinel straight to a
	// process that may use it immediately, and the proxy only swaps strings it
	// has been told to watch for.
	if onChange != nil {
		onChange()
	}
	return record, nil
}

// lookup returns the live activation for an ephemeral sentinel.
func (a *activations) lookup(sentinel string) (activation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.byEphemeral[sentinel]
	if !ok || !record.live(a.now()) {
		return activation{}, false
	}
	return record, true
}

// lookupAny reports whether a sentinel was minted here at all, live or not. It
// is what separates "an activation that has lapsed or is being used against the
// wrong host" from "an ordinary injected sentinel", so the first is refused
// rather than forwarded as if it were a stable binding.
func (a *activations) lookupAny(sentinel string) (activation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.byEphemeral[sentinel]
	return record, ok
}

// sentinelsByClient returns the live ephemeral sentinels per sandbox, for
// merging into the proxy's per-client sentinel set.
func (a *activations) sentinelsByClient() map[string][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(a.now())
	out := make(map[string][]string, len(a.byEphemeral))
	for sentinel, record := range a.byEphemeral {
		out[record.SandboxID] = append(out[record.SandboxID], sentinel)
	}
	return out
}

// A sandbox that is deleted needs no explicit cleanup here. Its client
// certificate goes with its proxy material, so it can no longer reach either
// endpoint, and its activations lapse within activationTTL and are swept. That
// reclamation is deliberately not wired to sandbox deletion: deletion is
// handled in the pool-agent process and this registry lives in the proxy unit,
// so a direct call would be a cross-process one — and the only thing it would
// buy is dropping dead entries a few minutes sooner.

// sweep drops expired activations and republishes if anything went. It backs
// the periodic tick, so an idle sandbox's sentinel set empties on its own
// rather than only when the next `get` prunes it.
func (a *activations) sweep() {
	a.mu.Lock()
	changed := a.pruneLocked(a.now())
	onChange := a.onChange
	a.mu.Unlock()
	if changed && onChange != nil {
		onChange()
	}
}

func (a *activations) pruneLocked(now time.Time) bool {
	changed := false
	for sentinel, record := range a.byEphemeral {
		if !record.live(now) {
			delete(a.byEphemeral, sentinel)
			changed = true
		}
	}
	return changed
}
