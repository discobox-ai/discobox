package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// The credential reporter: what this proxy says about a credential the upstream
// refused (ADR 0132).
//
// A 401 on a swapped request is the only evidence anywhere that a credential
// has stopped working — the sandbox holds a sentinel, and everything behind it
// belongs to the control plane, which cannot see a use it did not make. The
// response path sees it and nothing else does, so this is where the fact leaves
// the pool.
//
// It lives on the proxy rather than on the Swapper because the Swapper is
// rebuilt on every ApplyConfig — which is every time a pool agent mints an
// ephemeral sentinel — and everything here is memory that has to outlive that:
// which keys have been reported, and how recently.

const (
	// reportCooldown is how often one (client, sentinel, host) may be reported.
	// A harness that has decided it is logged out retries hard, and every one
	// of those retries is the same fact; the control plane needs it once. It
	// also bounds the forced refresh a report triggers there, which spends a
	// refresh token that rotates on use.
	reportCooldown = time.Minute
	// reportQueueSize is how many reports may be waiting to go out. Reporting
	// is off the request path and must stay off it, so the queue drops rather
	// than blocks — the same rule the audit recorder follows. A drop costs the
	// timeliness of a report the next rejection will make again.
	reportQueueSize = 64
	// reportTimeout bounds one report so an unreachable control plane cannot
	// wedge the sender.
	reportTimeout = 10 * time.Second
	// reportMemory is how long a reported rejection is remembered, which is
	// what an acceptance is measured against: a credential that starts working
	// again retracts what was said about it only if this proxy still remembers
	// saying it.
	//
	// It is longer than the cooldown because the two answer different
	// questions — how often to repeat a rejection, against how long a
	// clearance is worth sending — and bounded because the keys are not a small
	// set: a pool agent mints an ephemeral sentinel per credential use, and
	// each of those is a key. Nothing else would ever remove one, since the
	// case this feature is about is a credential that stays refused.
	reportMemory = 15 * time.Minute
)

// credentialReporter sends swap verdicts to the resolver, off the request path.
type credentialReporter struct {
	send func(ctx context.Context, req secrets.ReportRequest) error
	now  func() time.Time

	queue chan secrets.ReportRequest
	done  chan struct{}
	once  sync.Once
	// closeMu and closed are the guard the audit recorder has for the same
	// shape of queue: the sender is shut down while requests may still be in
	// flight — Server.Close gives the connections 30 seconds and then stops
	// waiting — and a report enqueued after the channel closed would panic the
	// proxy rather than be dropped.
	closeMu sync.RWMutex
	closed  bool

	mu sync.Mutex
	// reported is when each key was last reported rejected. A key in here is a
	// credential believed dead, which is what makes an acceptance worth
	// sending: a credential that works is otherwise silent. Entries age out
	// after reportMemory; see sweepLocked.
	reported map[string]time.Time
}

func newCredentialReporter(send func(context.Context, secrets.ReportRequest) error) *credentialReporter {
	r := &credentialReporter{
		send:     send,
		now:      time.Now,
		queue:    make(chan secrets.ReportRequest, reportQueueSize),
		done:     make(chan struct{}),
		reported: map[string]time.Time{},
	}
	go r.run()
	return r
}

func (r *credentialReporter) run() {
	for req := range r.queue {
		ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
		_ = r.send(ctx, req)
		cancel()
	}
	close(r.done)
}

// close stops the sender and waits for what is already queued.
func (r *credentialReporter) close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.closeMu.Lock()
		r.closed = true
		close(r.queue)
		r.closeMu.Unlock()
	})
	select {
	case <-r.done:
	case <-time.After(reportTimeout):
	}
}

// rejected reports every sentinel in a swapped request the upstream refused,
// at most once per cooldown each.
func (r *credentialReporter) rejected(clientID, host string, sentinels []string, outcome secrets.Outcome) {
	if r == nil {
		return
	}
	now := r.now()
	for _, sentinel := range sentinels {
		if !r.claim(clientID, sentinel, host, now) {
			continue
		}
		r.enqueue(secrets.ReportRequest{ClientID: clientID, Sentinel: sentinel, Host: host, Outcome: outcome})
	}
}

// accepted clears the rejections this proxy reported for the sentinels in a
// request the upstream took.
//
// It is deliberately not the mirror of rejected: a credential that works is the
// ordinary case and says nothing, so only a sentinel standing in the reported
// set produces a call. That set is also the reason this is cheap enough to ask
// on every swapped response — a map lookup per sentinel, and nothing else until
// something has actually been reported.
func (r *credentialReporter) accepted(clientID, host string, sentinels []string) {
	if r == nil {
		return
	}
	for _, sentinel := range sentinels {
		if !r.release(clientID, sentinel, host) {
			continue
		}
		r.enqueue(secrets.ReportRequest{ClientID: clientID, Sentinel: sentinel, Host: host, Outcome: secrets.OutcomeAccepted})
	}
}

// claim reports whether this key is due a rejection report, and records that it
// was taken. A key already reported inside the cooldown is not sent again.
func (r *credentialReporter) claim(clientID, sentinel, host string, now time.Time) bool {
	key := reportKey(clientID, sentinel, host)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	if last, ok := r.reported[key]; ok && now.Sub(last) < reportCooldown {
		return false
	}
	r.reported[key] = now
	return true
}

// release reports whether this key had an outstanding rejection, and forgets
// it. The forgetting is what lets the next rejection be reported immediately
// rather than waiting out a cooldown that belongs to a credential since fixed.
func (r *credentialReporter) release(clientID, sentinel, host string) bool {
	key := reportKey(clientID, sentinel, host)
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	if _, ok := r.reported[key]; !ok {
		return false
	}
	delete(r.reported, key)
	return true
}

// sweepLocked drops what is too old to be worth an acceptance. It runs on the
// two paths that touch the map at all, so the set is bounded by what has been
// refused in the last reportMemory rather than by everything ever refused.
func (r *credentialReporter) sweepLocked(now time.Time) {
	for key, at := range r.reported {
		if now.Sub(at) >= reportMemory {
			delete(r.reported, key)
		}
	}
}

func (r *credentialReporter) enqueue(req secrets.ReportRequest) {
	r.closeMu.RLock()
	defer r.closeMu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.queue <- req:
	default:
	}
}

func reportKey(clientID, sentinel, host string) string {
	return clientID + "\x00" + sentinel + "\x00" + host
}
