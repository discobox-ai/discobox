package poolagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// poolEdgeResolveEvery is how long the pool's machine goes between lookups of
// its own host for its sandboxes. A lookup is made only as a sandbox's
// document is decided, so a pool with no sandboxes makes none.
const poolEdgeResolveEvery = time.Minute

// poolEdgeResolveTimeout bounds one lookup, which a document's decision
// waits on.
const poolEdgeResolveTimeout = 5 * time.Second

// poolEdge is how this pool's sandboxes reach the edge it sits behind (ADR
// 26-11-16-619 §§2–3): the address of the pool's host as this machine last
// resolved it, which a sandbox dials in place of the name, since its fence
// may close DNS; and the pool listener's credential, which the server mints,
// shares among the pool's sandboxes, and places beside the bootstrap, renewing
// it before it expires (bundle.SandboxEdgeCredentialFile).
type poolEdge struct {
	// host is the pool's host at the edge, its pool URL's.
	host string
	// credentialFile is where the server places the credential.
	credentialFile string
	lookup         func(ctx context.Context, host string) ([]netip.Addr, error)
	now            func() time.Time

	mu         sync.Mutex
	address    string
	resolvedAt time.Time
	// resolving is a lookup under way, outside mu, closed when it ends.
	resolving chan struct{}
	warnedAt  time.Time
	// credential is the last one read whole from credentialFile, handed out
	// while unexpired when a read fails: the server replaces the file in
	// place, so a read can catch it missing or half written.
	credential *machine.Credential
}

func newPoolEdge(host, credentialFile string) *poolEdge {
	return &poolEdge{
		host:           host,
		credentialFile: credentialFile,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		now: time.Now,
	}
}

// edge is the edge as it is now: the credential read from its file, and the
// address resolved again once poolEdgeResolveEvery has passed. A lookup that
// fails keeps the address last resolved, which still reaches the pool, since
// the edge routes by name; with none yet the sandbox's bridges resolve the
// name. A credential that does not read is the last one that did, while that
// is unexpired; one never read, or expired, is left out and warned of, at
// most once a poolEdgeResolveEvery: the rest of each sandbox's document (its
// renewed certificate, its sources) is still delivered, and the credential
// follows in a newer revision once the server places one.
func (e *poolEdge) edge() *sandboxconfig.RuntimeEdge {
	out := &sandboxconfig.RuntimeEdge{Address: e.resolve()}
	credential, err := e.currentCredential()
	if err != nil {
		e.warnCredential(err)
		return out
	}
	out.Header, out.Token, out.Expires = credential.Header, credential.Token, credential.Expires
	return out
}

// currentCredential is the credential file's, or the last one read whole
// while the file does not read, as long as it is unexpired.
func (e *poolEdge) currentCredential() (machine.Credential, error) {
	read, err := readEdgeCredential(e.credentialFile)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if err == nil && now.Before(read.Expires) {
		e.credential = &read
		return read, nil
	}
	if held := e.credential; held != nil && now.Before(held.Expires) {
		return *held, nil
	}
	if err == nil {
		err = fmt.Errorf("the credential for the pool's listener at %s expired at %s, and the server has not renewed it", e.credentialFile, read.Expires.Format(time.RFC3339))
	}
	return machine.Credential{}, err
}

// warnCredential says why the pool's sandboxes are handed no credential, once
// a poolEdgeResolveEvery rather than once a sandbox a poll.
func (e *poolEdge) warnCredential(err error) {
	e.mu.Lock()
	now := e.now()
	due := e.warnedAt.IsZero() || now.Sub(e.warnedAt) >= poolEdgeResolveEvery || now.Before(e.warnedAt)
	if due {
		e.warnedAt = now
	}
	e.mu.Unlock()
	if due {
		slog.Warn("the pool's sandboxes are handed no credential for its listener, so their tunnels through its edge are refused until the server places one", "error", err)
	}
}

// resolve is the address held, looked up again once poolEdgeResolveEvery has
// passed. The lookup runs outside the lock, by one caller at a time: while one
// is under way the others are answered with the address held, so a slow
// resolver holds up no sandbox's document but the one that asked; only with
// none held yet (the agent just started) do they wait for it, since an empty
// address has the sandbox resolve a name its fence may not let it. A lookup
// that finds nothing with none held is tried again by the next caller.
func (e *poolEdge) resolve() string {
	e.mu.Lock()
	now := e.now()
	held := e.address
	if inflight := e.resolving; inflight != nil {
		e.mu.Unlock()
		if held != "" {
			return held
		}
		<-inflight
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.address
	}
	if !e.resolvedAt.IsZero() && now.Sub(e.resolvedAt) < poolEdgeResolveEvery && !now.Before(e.resolvedAt) {
		e.mu.Unlock()
		return held
	}
	done := make(chan struct{})
	e.resolving = done
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), poolEdgeResolveTimeout)
	addresses, err := e.lookup(ctx, e.host)
	cancel()
	next := pickAddress(addresses, held)
	if err == nil && next == "" {
		err = errors.New("no address")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resolving = nil
	close(done)
	if err != nil {
		if e.address != "" {
			e.resolvedAt = now
		}
		slog.Warn("the pool's host did not resolve for its sandboxes; keeping the address last resolved", "host", e.host, "address", e.address, "error", err)
		return e.address
	}
	e.address, e.resolvedAt = next, now
	return e.address
}

// pickAddress is held while the host's answer still holds it: a name that
// rotates among several would otherwise make every sandbox a new document at
// each lookup, for nothing. Otherwise the first IPv4 address, since a
// sandbox's machine may have no IPv6 route, or else the first; empty for no
// answer.
func pickAddress(addresses []netip.Addr, held string) string {
	if len(addresses) == 0 {
		return ""
	}
	for _, address := range addresses {
		if address.Unmap().String() == held {
			return held
		}
	}
	addresses = slices.Clone(addresses)
	slices.SortStableFunc(addresses, func(a, b netip.Addr) int {
		switch {
		case a.Unmap().Is4() == b.Unmap().Is4():
			return 0
		case a.Unmap().Is4():
			return -1
		default:
			return 1
		}
	})
	return addresses[0].Unmap().String()
}

// readEdgeCredential is the credential the server placed for the pool's
// sandboxes. Nothing of the file is put in an error.
func readEdgeCredential(path string) (machine.Credential, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return machine.Credential{}, fmt.Errorf("the credential for the pool's listener, which the server places at %s, is not there", path)
	}
	if err != nil {
		return machine.Credential{}, fmt.Errorf("read the credential for the pool's listener: %w", err)
	}
	var credential machine.Credential
	if json.Unmarshal(data, &credential) != nil || credential.Header == "" || credential.Token == "" {
		return machine.Credential{}, fmt.Errorf("the credential for the pool's listener at %s is not a disco-vm credential", path)
	}
	return credential, nil
}
