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
	// credentialFile is where the server places the credential, and empty on
	// a driver whose URLs answer without one.
	credentialFile string
	lookup         func(ctx context.Context, host string) ([]netip.Addr, error)
	now            func() time.Time

	mu         sync.Mutex
	address    string
	resolvedAt time.Time
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
// name. A credential that is not placed, or has expired, is an error: a
// sandbox could not reach its pool with it.
func (e *poolEdge) edge() (*sandboxconfig.RuntimeEdge, error) {
	out := &sandboxconfig.RuntimeEdge{Address: e.resolve()}
	if e.credentialFile == "" {
		return out, nil
	}
	credential, err := readEdgeCredential(e.credentialFile)
	if err != nil {
		return nil, err
	}
	if !e.now().Before(credential.Expires) {
		return nil, fmt.Errorf("the credential for the pool's listener at %s expired at %s, and the server has not renewed it", e.credentialFile, credential.Expires.Format(time.RFC3339))
	}
	out.Header, out.Token, out.Expires = credential.Header, credential.Token, credential.Expires
	return out, nil
}

func (e *poolEdge) resolve() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if !e.resolvedAt.IsZero() && now.Sub(e.resolvedAt) < poolEdgeResolveEvery && !now.Before(e.resolvedAt) {
		return e.address
	}
	e.resolvedAt = now
	ctx, cancel := context.WithTimeout(context.Background(), poolEdgeResolveTimeout)
	defer cancel()
	addresses, err := e.lookup(ctx, e.host)
	if err == nil && len(addresses) == 0 {
		err = errors.New("no address")
	}
	if err != nil {
		slog.Warn("the pool's host did not resolve for its sandboxes; keeping the address last resolved", "host", e.host, "address", e.address, "error", err)
		return e.address
	}
	// The address held while the host still answers it: a name that rotates
	// among several would otherwise make every sandbox a new document at each
	// lookup, for nothing.
	for _, address := range addresses {
		if address.Unmap().String() == e.address {
			return e.address
		}
	}
	// IPv4 first: a sandbox's machine may have no IPv6 route.
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
	e.address = addresses[0].Unmap().String()
	return e.address
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
