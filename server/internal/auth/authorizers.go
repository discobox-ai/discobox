package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/store"
)

// Authorizer authorizes an authenticated request. It returns ok=false when it
// does not apply to the request, allowing the next authorizer to try.
//
// Authorizers should assert that a request is in a positively identified scope,
// not that it is outside another scope. Negative assertions such as "not a
// pool-agent route" can unintentionally authorize new or misspelled routes.
type Authorizer interface {
	Authorize(*http.Request) (bool, error)
}

// ProjectAuthorizer authorizes user membership for project-scoped routes and
// resolves /projects/default and /api/projects/default for downstream handlers.
type ProjectAuthorizer struct {
	Store *store.Store
}

func (a ProjectAuthorizer) Authorize(r *http.Request) (bool, error) {
	projectID, ok := projectIDFromPath(r.URL.Path)
	if !ok {
		return false, nil
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type != PrincipalTypeUser || principal.UserID == "" {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("project access requires a user")}
	}
	// A person discobot vouches for acts in the one project discobot signed,
	// and "default" means that one. Who is in which project is discobot's to
	// know, so no member row is read (ADR 26-10-07-005 §2).
	if principal.Asserted() {
		if principal.ProjectID == "" {
			return false, authorizationError{status: http.StatusForbidden, err: errors.New("the discobot assertion names no project")}
		}
		if projectID == "default" {
			*r = *r.WithContext(context.WithValue(r.Context(), defaultProjectIDContextKey{}, principal.ProjectID))
			return true, nil
		}
		if projectID != principal.ProjectID {
			return false, authorizationError{status: http.StatusForbidden, err: errors.New("the discobot assertion is for another project")}
		}
		return true, nil
	}
	if projectID == "default" {
		project, err := a.Store.GetDefaultProjectForUser(r.Context(), principal.UserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, authorizationError{status: http.StatusNotFound, err: errors.New("default project not found")}
			}
			return false, authorizationError{status: http.StatusInternalServerError, err: err}
		}
		projectID = project.ID
		*r = *r.WithContext(context.WithValue(r.Context(), defaultProjectIDContextKey{}, projectID))
	}
	member, err := a.Store.IsProjectMember(r.Context(), projectID, principal.UserID)
	if err != nil {
		return false, authorizationError{status: http.StatusInternalServerError, err: err}
	}
	if !member {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("project access denied")}
	}
	return true, nil
}

// SandboxRoleAuthorizer authorizes a sandbox's own calls against the sandbox
// role (ADR 0140 §4): a fixed list of routes, in the sandbox's own project, and
// nothing else. It is decided by the route, and for some routes by what the
// route names: source delivery only into a discobox the sandbox created (ADR
// 26-09-24-630 §2), its terminals read and typed into only for a discobox it
// created (ADR 26-10-01-397 §1), its power instructed only for a discobox it
// created (ADR 26-10-02-478 §1), and a secret request only when a discobox it
// created filed it (ADR 26-09-30-782 §2). No use's text is read here: what a
// call is for is the judge's question, asked in the pool.
//
// It answers every request a sandbox principal makes, refusing what the role
// does not list, rather than stepping aside. The authorizers after it answer
// any authenticated principal on some routes — enrolling peers, registering a
// pool — which a sandbox must never reach by being authenticated.
type SandboxRoleAuthorizer struct {
	Store *store.Store
}

// sandboxRoleRoute is one route the sandbox role allows: a method, and the
// path under /projects/{projectId}/ with "*" standing for one segment.
type sandboxRoleRoute struct {
	method string
	path   string
	// service, when set, is the only value the route's `service` query
	// parameter may have: Git's info/refs answers both push and fetch.
	service string
	// owner is what the route's first "*" must belong to the calling sandbox
	// as. The zero value asks nothing of it.
	owner ownership
}

// ownership is how what a route names must belong to the sandbox calling it.
type ownership int

const (
	// The zero value asks nothing of what a route names.
	_ ownership = iota
	// createdSandbox: the path names a discobox the caller created (ADR
	// 26-09-24-630 §2, 26-10-01-397 §1, 26-10-02-478 §1).
	createdSandbox
	// ownedRequest: the path names a secret request filed by a discobox the
	// caller created — the request's owner (ADR 26-09-30-782 §2).
	ownedRequest
)

// sandboxRole is the sandbox role. Adding a route here is widening what every
// sandbox holding the discobox credential may do; ADR 0140 §4 lists it, and
// its Deferred section says what is left out and when to revisit it.
var sandboxRole = []sandboxRoleRoute{
	{method: http.MethodGet, path: "sandboxes"},
	{method: http.MethodPost, path: "sandboxes"},
	{method: http.MethodGet, path: "sandboxes/*"},
	// Delivering a source into a discobox the sandbox created: the push into
	// its origin, and the report that ends its wait (ADR 26-09-24-630 §2). Fetching
	// from an origin is not delivery, and is not here.
	{method: http.MethodGet, path: "sandboxes/*/git-origins/*/info/refs", service: "git-receive-pack", owner: createdSandbox},
	{method: http.MethodPost, path: "sandboxes/*/git-origins/*/git-receive-pack", owner: createdSandbox},
	{method: http.MethodPost, path: "sandboxes/*/complete-source-push", owner: createdSandbox},
	// Reading and typing into the terminals of a discobox the sandbox created,
	// one judged call at a time (ADR 26-10-01-397 §1). Attaching, and creating
	// or ending an exec, are not here (§2).
	{method: http.MethodGet, path: "sandboxes/*/execs", owner: createdSandbox},
	{method: http.MethodGet, path: "sandboxes/*/execs/*/screen", owner: createdSandbox},
	{method: http.MethodPost, path: "sandboxes/*/execs/*/wait", owner: createdSandbox},
	{method: http.MethodPost, path: "sandboxes/*/execs/*/input", owner: createdSandbox},
	// Starting, stopping, and restarting a discobox the sandbox created (ADR
	// 26-10-02-478 §1). Archive, purge, repair, and upgrade change what exists,
	// not whether it runs, and are not here (§2).
	{method: http.MethodPost, path: "sandboxes/*/start", owner: createdSandbox},
	{method: http.MethodPost, path: "sandboxes/*/stop", owner: createdSandbox},
	{method: http.MethodPost, path: "sandboxes/*/restart", owner: createdSandbox},
	// The secrets it was delegated, and no others (ADR 26-09-30-782 §3): the
	// listing is filtered to them where it is served, so a discobox can name
	// what it may hand on and see nothing else of the project's credentials.
	// It carries names and bindings, never a value.
	{method: http.MethodGet, path: "secrets"},
	// The requests of the discoboxes it created, and no others (ADR
	// 26-09-30-782 §2). The listing is filtered to them where it is served,
	// since a route cannot filter what it answers with.
	{method: http.MethodGet, path: "secret-requests"},
	{method: http.MethodGet, path: "secret-requests/*", owner: ownedRequest},
	{method: http.MethodPost, path: "secret-requests/*/approve", owner: ownedRequest},
	{method: http.MethodPost, path: "secret-requests/*/deny", owner: ownedRequest},
}

func (a SandboxRoleAuthorizer) Authorize(r *http.Request) (bool, error) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type != PrincipalTypeSandbox {
		return false, nil
	}
	denied := authorizationError{status: http.StatusForbidden, err: errors.New("not in the sandbox role")}
	projectID, rest, ok := projectRoute(r.URL.Path)
	if !ok {
		return false, denied
	}
	switch projectID {
	case principal.ProjectID:
	case "default":
		// A sandbox's default project is its own.
		*r = *r.WithContext(context.WithValue(r.Context(), defaultProjectIDContextKey{}, principal.ProjectID))
	default:
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("a sandbox may only act in its own project")}
	}
	for _, route := range sandboxRole {
		if route.method != r.Method || !matchRolePath(route.path, rest) {
			continue
		}
		if route.service != "" && r.URL.Query().Get("service") != route.service {
			continue
		}
		named := strings.Split(rest, "/")[1:]
		switch route.owner {
		case createdSandbox:
			return a.authorizeCreated(r, principal, named[0])
		case ownedRequest:
			return a.authorizeOwnedRequest(r, principal, named[0])
		}
		return true, nil
	}
	return false, denied
}

// authorizeCreated allows a route on a discobox only when the calling sandbox
// is recorded as its creator. A discobox a person created records no creator,
// so no sandbox passes this for it.
func (a SandboxRoleAuthorizer) authorizeCreated(r *http.Request, principal Principal, sandboxID string) (bool, error) {
	target, err := a.Store.GetSandbox(r.Context(), principal.ProjectID, sandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, authorizationError{status: http.StatusNotFound, err: errors.New("sandbox not found")}
		}
		return false, authorizationError{status: http.StatusInternalServerError, err: err}
	}
	if target.CreatedBySandboxID == nil || *target.CreatedBySandboxID != principal.SandboxID {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("a discobox acts only on a discobox it created")}
	}
	return true, nil
}

// authorizeOwnedRequest allows a route on a secret request only when a
// discobox the calling sandbox created filed it. A request a person or a pool
// opened has no discobox, and one whose discobox is gone has no owner left:
// both are a person's to answer.
func (a SandboxRoleAuthorizer) authorizeOwnedRequest(r *http.Request, principal Principal, requestID string) (bool, error) {
	notYours := authorizationError{status: http.StatusForbidden, err: errors.New("a discobox answers only the requests of discoboxes it created")}
	req, err := a.Store.GetSecretRequest(r.Context(), principal.ProjectID, requestID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, authorizationError{status: http.StatusNotFound, err: errors.New("secret request not found")}
		}
		return false, authorizationError{status: http.StatusInternalServerError, err: err}
	}
	if req.SandboxID == "" {
		return false, notYours
	}
	requester, err := a.Store.GetSandbox(r.Context(), principal.ProjectID, req.SandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, notYours
		}
		return false, authorizationError{status: http.StatusInternalServerError, err: err}
	}
	if requester.CreatedBySandboxID == nil || *requester.CreatedBySandboxID != principal.SandboxID {
		return false, notYours
	}
	return true, nil
}

// projectRoute splits a project-scoped path into its project and the rest.
func projectRoute(path string) (string, string, bool) {
	projectID, ok := projectIDFromPath(path)
	if !ok {
		return "", "", false
	}
	path = strings.TrimPrefix(path, "/api")
	rest := strings.TrimPrefix(path, "/projects/"+projectID)
	return projectID, strings.Trim(rest, "/"), true
}

// matchRolePath matches a role route's path, where "*" is exactly one segment.
func matchRolePath(pattern, path string) bool {
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if got[i] == "" || (want[i] != "*" && want[i] != got[i]) {
			return false
		}
	}
	return true
}

// PoolRouteAuthorizer authorizes authenticated pool agents for pool-scoped
// API routes. Operation-specific handlers still verify resource identity, such
// as matching the authenticated pool principal to a path pool ID.
type PoolRouteAuthorizer struct{}

func (PoolRouteAuthorizer) Authorize(r *http.Request) (bool, error) {
	if !isPoolRuntimePath(r.URL.Path) {
		return false, nil
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type != PrincipalTypePool {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("pool agent access required")}
	}
	return true, nil
}

// SSHConnectPath carries an SSH connection over the API's own transport, the
// only way SSH reaches the server (ADR 0024, ADR 0057).
const SSHConnectPath = "/ssh/connect"

// SSHConnectAuthorizer admits SSHConnectPath for the CLI, its only client: a
// user principal that is not one discobot asserted. SSH then authenticates
// its key inside its own protocol as before; this is the HTTP credential in
// front of it, so SSH is reached only by someone the server already
// authenticated. A pool agent, a sandbox, or a person discobot vouches for
// is refused. With authRequired no such user exists, so SSH is unreachable.
type SSHConnectAuthorizer struct{}

func (SSHConnectAuthorizer) Authorize(r *http.Request) (bool, error) {
	if r.URL.Path != SSHConnectPath {
		return false, nil
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type != PrincipalTypeUser || principal.UserID == "" || principal.Asserted() {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("SSH is reached by the CLI's own user")}
	}
	return true, nil
}

// AuthenticatedAuthorizer authorizes explicitly listed routes for any
// authenticated principal. It is intentionally allow-list based; routes not
// listed here must be authorized by a more specific authorizer.
type AuthenticatedAuthorizer struct{}

func (AuthenticatedAuthorizer) Authorize(r *http.Request) (bool, error) {
	if !isAuthenticatedAllowedPath(r.URL.Path) {
		return false, nil
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Type == "" {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("authenticated access required")}
	}
	// A person discobot vouches for holds a project, not this server: of the
	// server-scoped routes they reach only listing and creating projects and
	// the read-only catalogs, never shutdown, peer enrollment, or pool
	// registration.
	if principal.Asserted() && !isAssertedAllowedPath(r.URL.Path) {
		return false, authorizationError{status: http.StatusForbidden, err: errors.New("a discobot assertion does not reach this route")}
	}
	return true, nil
}

type authorizationError struct {
	status int
	err    error
}

func (e authorizationError) Error() string {
	return e.err.Error()
}

func (e authorizationError) Unwrap() error {
	return e.err
}

func (e authorizationError) StatusCode() int {
	return e.status
}

var authenticatedAllowedPaths = []string{
	"/harness-definitions",
	"/harness-definitions/",
	PoolRegisterPath,
	// Enrolling and revoking peers (ADR 0095 §1, enrolled iroh IDs). There is no
	// resource-specific authorizer for it: the resource is server-scoped, so
	// there is no project membership to check, and an enrolled peer
	// authenticates as the default user rather than as a principal of its own.
	//
	// This authorizes any authenticated principal, which today is every caller
	// the pipeline sees, on every listener the router serves — the carrier hub
	// included. ADR 0095 §1 (enrolled iroh IDs) accepts that knowingly and says why: such a caller
	// already holds ScopeAll, so this grants no new scope, but it does let
	// transient reach become a durable external credential, and it lets that
	// caller revoke every enrollment. Narrow this the moment a connection has
	// provenance to authorize on.
	"/peers",
	"/peers/",
	// This server's own peer ID (ADR 0098). Server-scoped like /peers, and
	// for the same reason unauthorized by anything narrower: there is no
	// project membership to check and the value is one an authenticated caller
	// is entitled to know — it is the address they reached this server at, or
	// the one they would.
	"/peer",
	// What this server calls itself (ADR 0116 §2), for the same reason as
	// /peer: server-scoped, and nothing an authenticated caller is not
	// entitled to.
	"/server",
	"/projects",
	"/providers/catalog",
	"/shutdown",
}

// assertedAllowedPaths is the part of authenticatedAllowedPaths a person
// discobot vouches for may reach.
var assertedAllowedPaths = []string{
	"/harness-definitions",
	"/harness-definitions/",
	"/projects",
	"/providers/catalog",
}

func isAuthenticatedAllowedPath(path string) bool {
	return matchesAllowedPath(authenticatedAllowedPaths, path)
}

func isAssertedAllowedPath(path string) bool {
	return matchesAllowedPath(assertedAllowedPaths, path)
}

// matchesAllowedPath reports whether path is in an allow-list, where an entry
// ending in "/" is a prefix and any other entry must match exactly.
func matchesAllowedPath(list []string, path string) bool {
	for _, allowed := range list {
		if strings.HasSuffix(allowed, "/") {
			if strings.HasPrefix(path, allowed) {
				return true
			}
			continue
		}
		if path == allowed {
			return true
		}
	}
	return false
}
