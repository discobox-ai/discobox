package discovm

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// A pool machine's listener on a driver whose URLs answer only a credential
// (disco-vm's URLCapabilities.Credential: archil) is reached with one of two
// credentials the server holds, never the guest (ADR 26-11-16-619 §2): its
// own, for its calls to the pool's agent and the control-plane channel, kept
// in memory; and one the pool's sandboxes share, which it mints and places
// beside the bootstrap (bundle.SandboxEdgeCredentialFile) for the agent to
// hand each sandbox with its tunnel endpoint. On such a driver every URL
// lookup mints a token the driver neither keeps nor revokes until it expires,
// so the server asks for each only as one it holds passes half its life
// (serviceEndpoints, ensureSandboxEdge), never on every reconcile; a token is
// never kept past its machine, since the driver's tokens reach one machine and
// expire.

// sandboxEdgeRecord is the credential placed for the pool's sandboxes, as the
// provider's record keeps it: never the token.
type sandboxEdgeRecord struct {
	PlacedAt time.Time `json:"placedAt"`
	Expires  time.Time `json:"expires"`
}

// renewAt is when the credential is replaced: half its life.
func (r sandboxEdgeRecord) renewAt() time.Time {
	return halfLife(r.PlacedAt, r.Expires)
}

func halfLife(from, expires time.Time) time.Time {
	return from.Add(expires.Sub(from) / 2)
}

// ensureSandboxEdge mints the credential the pool's sandboxes reach its
// listener with, a pool URL of its own, and places it, unless the one
// recorded is still in its first half-life, or force (a repair, which places
// everything again, so a file lost from the machine's disk is put back); and
// records it. A driver whose
// URLs need none places nothing and keeps no record. A renewal that fails
// keeps the pool on the credential it holds while that still works, and is
// tried again on the next reconcile: a pool that works is never failed over a
// credential it does not need yet.
func (h *poolMachine) ensureSandboxEdge(ctx context.Context, pool *model.Pool, inst engine.Instance, guest machine.Guest, record *poolRuntimeState, force bool) error {
	if !h.engine.Driver.Capabilities().URLs.Credential {
		record.SandboxEdge = nil
		return nil
	}
	now := time.Now()
	current := record.SandboxEdge
	if current != nil && now.Before(current.renewAt()) && !force {
		return nil
	}
	endpoint, err := h.engine.URL(ctx, inst, machine.URLPool)
	if err == nil {
		err = heldCredential(endpoint, now)
	}
	if err == nil {
		var data []byte
		data, err = json.Marshal(endpoint.Credential) //nolint:gosec // G117: the credential file the pool's agent reads, placed root's alone
		if err == nil {
			err = poolruntime.PlaceBoxFiles(ctx, guest, poolruntime.BoxFile{Path: bundle.SandboxEdgeCredentialFile, Data: data})
		}
	}
	if err != nil {
		if current != nil && now.Before(current.Expires) {
			slog.WarnContext(ctx, "keeping the pool's sandboxes' listener credential until it expires: its renewal failed", "pool", pool.ID, "expires", current.Expires, "error", err)
			return nil
		}
		return fmt.Errorf("place the credential pool %s's sandboxes reach its listener with: %w", pool.ID, notReachable(err))
	}
	record.SandboxEdge = &sandboxEdgeRecord{PlacedAt: now, Expires: endpoint.Credential.Expires}
	return nil
}

// heldCredential refuses an endpoint on a credentialed driver that came
// without a credential a request can carry, or with one already expired.
func heldCredential(endpoint machine.Endpoint, now time.Time) error {
	credential := endpoint.Credential
	switch {
	case credential == nil:
		return errors.New("the driver gave the machine's URL without the credential its URLs answer")
	case !now.Before(credential.Expires):
		return errors.New("the driver's credential has already expired")
	}
	return sandboxconfig.ValidateEdgeCredential(credential.Header, credential.Token)
}

// serviceEndpoints is the server's own view of each pool machine's URLs, by
// the machine's ID at the driver and the URL's scope, on a driver whose URLs
// answer only a credential: each held with the credential it came with, and
// the client that sends it, until half that credential's life has passed. A
// machine made again under the pool's name is asked anew. Each entry has its
// own lock, so one pool's lookup that the driver is slow to answer holds up
// no other pool's.
type serviceEndpoints struct {
	mu   sync.Mutex
	held map[serviceKey]*heldEndpoint
}

type serviceKey struct {
	instance string
	scope    machine.URLScope
}

type heldEndpoint struct {
	mu       sync.Mutex
	endpoint machine.Endpoint
	asked    time.Time
	// client reaches the endpoint with its credential: one per credential,
	// so every lease of the pool's agent shares its connections.
	client *http.Client
}

// endpoint is the machine's URL in scope, with the client that reaches it:
// the one held while its credential is in its first half-life, and otherwise
// one asked for anew. A driver whose URLs need no credential is asked every
// time, as its lookup mints nothing and a local driver's URLs move with a
// start; its client is the default.
func (c *serviceEndpoints) endpoint(ctx context.Context, e *engine.Engine, inst engine.Instance, scope machine.URLScope) (machine.Endpoint, *http.Client, error) {
	if !e.Driver.Capabilities().URLs.Credential {
		endpoint, err := e.URL(ctx, inst, scope)
		return endpoint, http.DefaultClient, err
	}
	key := serviceKey{instance: inst.ID, scope: scope}
	c.mu.Lock()
	if c.held == nil {
		c.held = map[serviceKey]*heldEndpoint{}
	}
	held, ok := c.held[key]
	if !ok {
		held = &heldEndpoint{}
		c.held[key] = held
	}
	c.mu.Unlock()

	held.mu.Lock()
	defer held.mu.Unlock()
	now := time.Now()
	if held.client != nil && now.Before(halfLife(held.asked, held.endpoint.Credential.Expires)) {
		return held.endpoint, held.client, nil
	}
	endpoint, err := e.URL(ctx, inst, scope)
	if err != nil {
		return machine.Endpoint{}, nil, err
	}
	if err := heldCredential(endpoint, now); err != nil {
		return machine.Endpoint{}, nil, fmt.Errorf("machine %s's %s URL: %w", inst.Name, scope, err)
	}
	if held.client != nil {
		// The renewed credential's client replaces it: connections already
		// open on the old one stay until their leases end, and none is kept
		// idle.
		held.client.CloseIdleConnections()
	}
	client, err := edgeHTTPClient(defaultTransport(), endpoint)
	if err != nil {
		return machine.Endpoint{}, nil, fmt.Errorf("machine %s's %s URL: %w", inst.Name, scope, err)
	}
	held.endpoint, held.asked, held.client = endpoint, now, client
	return held.endpoint, held.client, nil
}

// forget drops what is held for a machine that is gone, closing its
// connections.
func (c *serviceEndpoints) forget(instanceID string) {
	c.mu.Lock()
	var gone []*heldEndpoint
	for key, held := range c.held {
		if key.instance == instanceID {
			delete(c.held, key)
			gone = append(gone, held)
		}
	}
	c.mu.Unlock()
	for _, held := range gone {
		held.mu.Lock()
		if held.client != nil {
			held.client.CloseIdleConnections()
		}
		held.mu.Unlock()
	}
}

// edgeHTTPClient is the client the server reaches a pool's agent through an
// edge that answers only a credential: base, over HTTP/1.1 alone, since a
// port credential is checked on HTTP/1.1 and the control-plane channel opens
// with an HTTP/1.1 Upgrade, sending credential on every request (ADR
// 26-11-16-619 §2).
//
// The credential is sent only to the endpoint's own scheme and host: a
// redirect, which the client follows, or any request the transport carries
// elsewhere, goes without it.
func edgeHTTPClient(base *http.Transport, endpoint machine.Endpoint) (*http.Client, error) {
	u, err := url.Parse(endpoint.URL)
	if err != nil || u.Host == "" {
		return nil, errors.New("the driver gave a URL with no host to send its credential to")
	}
	transport := base.Clone()
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	transport.Protocols = &protocols
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &http.Client{Transport: edgeCredential{scheme: u.Scheme, host: u.Host, credential: *endpoint.Credential, next: transport}}, nil
}

// defaultTransport is the process's default transport, the one every other
// call to a pool's agent uses, or one of its settings where something has
// replaced it with a transport of another kind.
func defaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// edgeCredential sends the edge's credential on every request to scheme and
// host, and on no other: not to another host, and not in the clear to the
// same one.
type edgeCredential struct {
	scheme     string
	host       string
	credential machine.Credential
	next       http.RoundTripper
}

// CloseIdleConnections closes the transport's idle connections, which
// http.Client.CloseIdleConnections reaches only through this method.
func (c edgeCredential) CloseIdleConnections() {
	if closer, ok := c.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (c edgeCredential) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Scheme, c.scheme) || !strings.EqualFold(req.URL.Host, c.host) {
		return c.next.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	req.Header.Set(c.credential.Header, c.credential.Token)
	return c.next.RoundTrip(req)
}
