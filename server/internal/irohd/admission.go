package irohd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
)

// storeWait is how long an admission check waits for the store before giving
// up on it. It bounds the cost of the wait: until the store arrives the gate
// cannot tell an unenrolled peer from one that might be enrolled, so every
// non-file peer parks, and the endpoint ID is not a secret (ADR 0095 §4).
//
// It is generous relative to what it covers — opening the database, migrating
// it, building the services — because expiring early turns a slow start into a
// wrong answer, which is the failure this wait exists to prevent.
const storeWait = 60 * time.Second

// PeerStore is the managed half of the allowlist: the peers ADR 0095 made a
// resource. It is an interface rather than *store.Store because admission
// needs exactly one question answered, and naming that question is what keeps
// the gate auditable.
//
// It takes the identity, not a string. Admission compares an identity proven
// by a TLS handshake against a row, and the two must not be able to differ by
// spelling.
type PeerStore interface {
	PeerExists(ctx context.Context, peer endpoint.IrohID) (bool, error)
}

// Admission decides which peers may connect to this server's iroh listener.
//
// It exists as a value with a settable store because of the order the server
// starts in: it binds before it initializes and answers while initializing, so
// the listener — and this gate — are built before there is a database to ask
// (ADR 0095 §4). The file layer answers immediately; the managed layer waits
// for SetStore.
type Admission struct {
	dataDir string

	mu    sync.Mutex
	store PeerStore
	ready chan struct{}
}

// NewAdmission builds the gate for a server whose data directory is dataDir.
// It is usable at once, admitting whatever is in authorized_ids.
func NewAdmission(dataDir string) *Admission {
	return &Admission{dataDir: dataDir, ready: make(chan struct{})}
}

// SetStore installs the managed layer, releasing any admission check waiting
// for it. Calling it twice is a programming error rather than a race to
// tolerate: there is one store per server.
func (a *Admission) SetStore(store PeerStore) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		return
	}
	a.store = store
	close(a.ready)
}

// Authorize is the accept-time policy, matching endpoint.IrohConfig.Authorize.
// A nil error admits the peer; the error it returns otherwise is the close
// reason that peer reads, so a refusal says which refusal it is.
//
// The layers are checked file-first, which is ADR 0024 §5's ordering: the
// recovery layer is the one that answers when both would. Today both grant the
// same principal, so the order arbitrates nothing — it is kept so that it
// already means the right thing if the grants ever diverge.
func (a *Admission) Authorize(ctx context.Context, id endpoint.IrohID) error {
	authorized, err := LoadAuthorizedIDs(a.dataDir)
	if err != nil {
		// Fail closed. An unreadable allowlist is not a reason to admit
		// everyone.
		log.Printf("iroh: read authorized IDs: %v", err)
		return errors.New("this server cannot read its allowlist")
	}
	if authorized.Allows(id) {
		return nil
	}

	store, err := a.awaitStore(ctx)
	if err != nil {
		return err
	}
	enrolled, err := store.PeerExists(ctx, id)
	if err != nil {
		log.Printf("iroh: look up enrolled peer %s: %v", id.Short(), err)
		return errors.New("this server cannot read its enrollments")
	}
	if !enrolled {
		log.Printf("iroh: refused peer %s: not enrolled", id.Short())
		return fmt.Errorf("peer %s is not authorized on this server", id)
	}
	return nil
}

// awaitStore returns the managed layer, waiting for startup to install it.
//
// Waiting rather than refusing is the decision in ADR 0095 §4: refusing would
// tell a correctly enrolled peer it is not enrolled because the server was
// still opening its database, and send its operator looking in the wrong file.
// The wait ends three ways, and each says something different to the peer.
func (a *Admission) awaitStore(ctx context.Context) (PeerStore, error) {
	a.mu.Lock()
	store := a.store
	a.mu.Unlock()
	if store != nil {
		return store, nil
	}

	timeout := time.NewTimer(storeWait)
	defer timeout.Stop()
	select {
	case <-a.ready:
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.store, nil
	case <-ctx.Done():
		// The listener is going away: startup failed, or the server is
		// shutting down. Answering beats dying unanswered.
		return nil, errors.New("this server is shutting down")
	case <-timeout.C:
		return nil, errors.New("this server has not finished starting")
	}
}
