package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/server/internal/auth/discobot"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Authenticator authenticates a request and returns the matched principal.
type Authenticator interface {
	Authenticate(*http.Request) (Principal, bool, error)
}

// PoolAuthenticator authenticates pool agent runtime requests from signed
// agent assertions verified against the pool's registered public key.
type PoolAuthenticator struct {
	Store *store.Store
}

func (a PoolAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	if !isPoolRuntimePath(r.URL.Path) {
		return Principal{}, false, nil
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return Principal{}, false, errors.New("pool agent assertion required")
	}
	routePoolID, err := poolIDFromRuntimePath(r.URL.Path)
	if err != nil {
		return Principal{}, false, err
	}
	pool, err := a.Store.GetPoolByID(r.Context(), routePoolID)
	if err != nil {
		return Principal{}, false, errors.New("pool not found")
	}
	if pool.RevokedAt != nil {
		return Principal{}, false, errors.New("pool is revoked")
	}
	if pool.KeyType != poolauth.KeyType {
		return Principal{}, false, errors.New("unsupported pool key type")
	}
	claims, err := poolauth.VerifyToken(pool.PublicKey, token)
	if err != nil {
		// Two unrelated faults arrive here, and they must not read
		// identically: that would leave the only visible symptom — a bare 401
		// the pool agent exits on — pointing at nothing.
		//
		// A signature that does not verify means the control plane holds a
		// different key than the agent signs with: a restored database, or a
		// pool row recreated under an agent that kept its stored key.
		//
		// A token outside its validity window means the two machines disagree
		// about the time. On a Mac that slept, that is the guest's clock hours
		// behind the host's, minting assertions the control plane reads as long
		// expired (ADR 0062). Naming the control plane's own clock is what makes
		// the gap legible from the agent's log, which is the side that reports
		// the failure.
		return Principal{}, false, fmt.Errorf("invalid pool agent assertion: %w (control plane clock %s)",
			err, time.Now().UTC().Format(time.RFC3339))
	}
	if claims.PoolID != pool.ID || claims.PoolID != routePoolID || claims.ProjectID != pool.ProjectID {
		return Principal{}, false, errors.New("pool agent assertion identity does not match route")
	}
	return Principal{Type: PrincipalTypePool, PoolID: claims.PoolID, Scopes: claims.Scopes}, true, nil
}

// SandboxForwardAuthenticator authenticates a sandbox's own call to the
// discobox API, which the pool hosting it forwarded from the reserved host
// (ADR 0140 §§2–3). It takes the pool's word for which sandbox is calling, and
// checks only what makes that word good: the pool's assertion verifies, carries
// the forwarding scope, comes from a pool that is not revoked, and names a
// sandbox the control plane placed on that pool.
//
// It fails rather than stepping aside once a request names a forwarded
// sandbox. A forwarded call that fell through would be answered by the next
// authenticator, and the next one answers every request as the default user.
type SandboxForwardAuthenticator struct {
	Store *store.Store
}

func (a SandboxForwardAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	sandboxID := strings.TrimSpace(r.Header.Get(poolauth.ForwardedSandboxHeader))
	if sandboxID == "" {
		return Principal{}, false, nil
	}
	refuse := func(reason string) (Principal, bool, error) {
		return Principal{}, false, errors.New("forwarded sandbox call refused: " + reason)
	}
	token := bearerToken(r.Header.Get("Authorization"))
	poolID := strings.TrimSpace(r.Header.Get(poolauth.ForwardingPoolHeader))
	if token == "" || poolID == "" {
		return refuse("its pool's assertion is required")
	}
	pool, err := a.Store.GetPoolByID(r.Context(), poolID)
	if err != nil {
		return refuse("pool not found")
	}
	if pool.RevokedAt != nil {
		return refuse("pool is revoked")
	}
	if pool.KeyType != poolauth.KeyType {
		return refuse("unsupported pool key type")
	}
	claims, err := poolauth.VerifyToken(pool.PublicKey, token)
	if err != nil {
		return refuse("invalid pool assertion: " + err.Error())
	}
	if claims.PoolID != pool.ID || claims.ProjectID != pool.ProjectID || !claims.HasScope(poolauth.ScopeSandboxForward) {
		return refuse("the pool's assertion does not allow forwarding")
	}
	sandbox, err := a.Store.GetSandboxByID(r.Context(), sandboxID)
	if err != nil || sandbox.PoolID != pool.ID || sandbox.ProjectID != pool.ProjectID {
		return refuse("the pool does not host that sandbox")
	}
	return Principal{
		Type:      PrincipalTypeSandbox,
		SandboxID: sandbox.ID,
		ProjectID: sandbox.ProjectID,
		PoolID:    pool.ID,
		UserID:    sandbox.CreatedByUserID,
	}, true, nil
}

// DiscobotAuthenticator authenticates a request discobot signed for a person
// (ADR 26-10-07-005 §2): the assertion in discobot.AssertionHeader, verified
// against discobot's public key. The principal is that person, acting in the
// project the assertion names, with discobot as its issuer.
//
// Like the sandbox authenticator it fails rather than stepping aside once a
// request carries an assertion: one that fell through would be answered as
// the default user. A server not configured with discobot's key refuses
// every assertion for the same reason.
type DiscobotAuthenticator struct {
	PublicKey ed25519.PublicKey
}

func (a DiscobotAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	token := strings.TrimSpace(r.Header.Get(discobot.AssertionHeader))
	if token == "" {
		return Principal{}, false, nil
	}
	if len(a.PublicKey) == 0 {
		return Principal{}, false, errors.New("discobot assertion refused: this server is not configured to trust discobot")
	}
	claims, err := discobot.Verify(a.PublicKey, token)
	if err != nil {
		return Principal{}, false, fmt.Errorf("discobot assertion refused: %w (server clock %s)", err, time.Now().UTC().Format(time.RFC3339))
	}
	return Principal{
		Type:      PrincipalTypeUser,
		UserID:    claims.UserID,
		ProjectID: claims.ProjectID,
		Issuer:    discobot.Issuer,
		Scopes:    []string{ScopeAll},
	}, true, nil
}

// PoolRegisterPath is where a starting pool agent redeems its bootstrap token.
const PoolRegisterPath = "/api/pools/register"

// PoolBootstrapAuthenticator authenticates a request to PoolRegisterPath that
// no earlier authenticator claimed, as PrincipalTypePoolBootstrap. The
// bootstrap token in its body is the credential, and the pools service
// redeems it: short-lived, one-time, stored hashed. This only lets the
// request reach that check, whether or not the default user is served
// (authRequired); a sandbox's forwarded call is still the sandbox's, and is
// refused by its role.
type PoolBootstrapAuthenticator struct{}

func (PoolBootstrapAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	if r.URL.Path != PoolRegisterPath {
		return Principal{}, false, nil
	}
	return Principal{Type: PrincipalTypePoolBootstrap}, true, nil
}

// IrohAdmission is the enrolled-peer gate an iroh connection was admitted by
// (irohd.Admission): authorized_ids and the managed enrollments.
type IrohAdmission interface {
	Authorize(ctx context.Context, peer endpoint.IrohID) error
}

// IrohPeerAuthenticator authenticates a request that arrived over iroh by the
// enrolled identity its peer proved (WithIrohPeer), when nothing before it
// claimed the request. That identity is checked against the enrollments again
// for every request, not only when the connection was admitted, so revoking
// a peer ends its access on its next request. A peer that is no longer
// enrolled is refused rather than passed on, since the next authenticator
// may be the default user.
//
// An enrolled peer is the operator's own client and acts as the default user
// (ADR 0095 §1), with IrohPeer recording which one. It is what lets the CLI
// reach a server that requires authentication (authRequired) over iroh.
type IrohPeerAuthenticator struct {
	Admission IrohAdmission
	UserID    string
}

func (a IrohPeerAuthenticator) Authenticate(r *http.Request) (Principal, bool, error) {
	peer, ok := irohPeerFromContext(r.Context())
	if !ok {
		return Principal{}, false, nil
	}
	if a.Admission == nil {
		return Principal{}, false, errors.New("this server has no iroh enrollments to check the peer against")
	}
	if err := a.Admission.Authorize(r.Context(), peer); err != nil {
		return Principal{}, false, fmt.Errorf("iroh peer %s refused: %w", peer.Short(), err)
	}
	return Principal{
		Type:     PrincipalTypeUser,
		UserID:   a.UserID,
		IrohPeer: peer.String(),
		Scopes:   []string{ScopeAll},
	}, true, nil
}

// DefaultUserAuthenticator authenticates every request as the configured user.
type DefaultUserAuthenticator struct {
	UserID string
}

func (a DefaultUserAuthenticator) Authenticate(*http.Request) (Principal, bool, error) {
	return Principal{
		Type:   PrincipalTypeUser,
		UserID: a.UserID,
		Scopes: []string{ScopeAll},
	}, true, nil
}

func bearerToken(authorization string) string {
	authorization = strings.TrimSpace(authorization)
	if authorization == "" {
		return ""
	}
	parts := strings.Fields(authorization)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

// poolRuntimeActions allowlists the actions a pool agent may reach under
// /api/pools/{poolId}/. It is an allowlist rather than a pattern on purpose:
// a route that is not named here is not authenticated as a pool route at all,
// so a new or misspelled one fails closed instead of inheriting pool access.
//
// The cost of that is a step every new pool route must remember, and forgetting
// it is invisible to any test that calls a service directly — the route simply
// answers 403 to the real agent. Add the action here in the same change that
// adds the route.
//
// The value is how many path segments after the action its routes take at
// most: none for most, one for an action addressing a resource by its ID.
var poolRuntimeActions = map[string]int{
	"sandbox-states":         0,
	"status":                 0,
	"resolve-sandbox-secret": 0,
	// A gateway's header values from the server's environment (ADR
	// 26-10-16-470 §4). One call names its gateway in the body.
	"resolve-gateway-headers": 0,
	// What an upstream made of a swapped credential (ADR 0132). One call
	// reports one verdict, so it takes no trailing ID.
	"sandbox-secret-rejections": 0,
	// Putting a job to the project's judge (ADR 26-09-22-838 §2). One call is one
	// job, so it takes no trailing ID.
	"judge": 0,
	// Judging a command before its credential is minted (ADR 26-09-22-838
	// §3). One call is one command, so it takes no trailing ID.
	"judge-commands":              0,
	"sandbox-agent-status-tokens": 0,
	"sandbox-agent-status":        0,
	// What each sandbox's list reads answer, posted when it changes
	// (ADR 26-10-11-657 §2). One call reports a batch, so it takes no
	// trailing ID.
	"sandbox-inventories": 0,
	// The pool's resource report (ADR 0071, resource accounting). It addresses the pool itself, so
	// it takes no trailing ID.
	"resources": 0,
	// Every sandbox the control plane holds on the pool, which bounds what its
	// volume reaper may collect (ADR 26-10-01-876). It lists, so it takes no
	// trailing ID.
	"sandboxes": 0,
	// The agent credentials broker (ADR 0031).
	"sandbox-credentials":         0,
	"sandbox-credential-requests": 1, // .../{requestId} polls one request
	// A sandbox's policy facts, read for the protocol's facts verb (ADR
	// 26-10-20-782 §4). The sandbox is a query parameter, so it takes no
	// trailing ID.
	"sandbox-policy-facts": 0,
	// Host trust (ADR 0149): an agent's ask, polled by its ID, and the pool's
	// read of every live trust its proxy enforces.
	"sandbox-trust-requests": 1, // .../{requestId} polls one request
	"sandbox-host-trusts":    0,
	// The asset API (ADR 26-10-16-423 §13), addressed as OCI distribution
	// addresses content: assets/{name}, assets/{name}/manifests/{digest} and
	// assets/blobs/{digest}. Every route under it is the asset API's, which
	// answers only what the pool's scope pins.
	"assets": 3,
}

func isPoolRuntimePath(path string) bool {
	_, ok := poolRuntimeAction(path)
	return ok
}

// poolRuntimeAction returns the allowlisted action a pool runtime path names.
func poolRuntimeAction(path string) (string, bool) {
	if !strings.HasPrefix(path, "/api/pools/") {
		return "", false
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 4 {
		return "", false
	}
	trailing, ok := poolRuntimeActions[segments[3]]
	if !ok {
		return "", false
	}
	// Trailing segments belong only to the actions that take them, and no more
	// of them than the action's routes have. Accepting them everywhere would
	// let an unlisted subroute of a listed action inherit pool access.
	if len(segments)-4 > trailing {
		return "", false
	}
	return segments[3], true
}

func poolIDFromRuntimePath(path string) (string, error) {
	if _, ok := poolRuntimeAction(path); !ok {
		return "", errors.New("pool runtime path is invalid")
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	poolID, err := url.PathUnescape(segments[2])
	if err != nil {
		return "", err
	}
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return "", errors.New("pool ID is required")
	}
	return poolID, nil
}
