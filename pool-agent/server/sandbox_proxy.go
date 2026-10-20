package server

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
	"github.com/go-chi/chi/v5"
)

const sandboxAgentAuthorizationHeader = "X-Discobox-Sandbox-Agent-Authorization"

func registerSandboxProxyRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/http/{port}", service.onUse(needsSandbox, service.sandboxHTTPProxyHandler()))
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/http/{port}/*", service.onUse(needsSandbox, service.sandboxHTTPProxyHandler()))

	// Whether each route below starts a stopped sandbox is not said here: the
	// control plane marks every request's token with its use, and onUse obeys
	// it (ADR 26-10-12-711 §2). As it marks them today: the sandbox's own
	// audit data, a terminal's screen and a wait on it are observes; listing
	// execs, services or tools, and reading one exec or service, are inspects,
	// which it answers from the sandbox's last inventory when this agent
	// refuses them (ADR 26-10-11-657 §4); the unbounded reads — logs, events,
	// resource samples — start the sandbox, as every action does.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/harness-hooks", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/exec-events", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	// Reading a terminal's screen, or waiting on it, is not use (ADR 0137 §2):
	// an orchestrator polling its workers must not start one idle stop put
	// down, or keep one from ever stopping. Typing into it is use, and does.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/screen", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/wait", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodDelete, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/logs", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/input", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/attach", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/attach", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/start", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/events", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources/history", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources/stream", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))

	// The judge answers from a discobox that is meant to be ready, but a pool
	// restart leaves it stopped, and the first ask is what starts it. A judge
	// that cannot come up refuses now rather than holding the request that is
	// waiting on it (ADR 26-09-22-838 §1).
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/judge", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	// The same runtime answers the questions about a request judged against
	// a policy (ADR 26-10-20-782 §8), and writes the wording of an approval
	// rule a person asked to be proposed (ADR 26-10-12-330 §7), on the same
	// scope.
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/judge/policy", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/judge/proposal", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))

	// A service is an exec (ADR 0070), reached the same way and gated by the
	// same scopes. It needs its own registrations because this router names
	// every sandbox-agent path it forwards rather than forwarding a prefix.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/tools", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/logs", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/start", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/stop", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/restart", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))

	// The meta file (ADR 0136). Writing it starts a stopped sandbox like any
	// other use: the sandbox is where its name, description and tags live, so a
	// change cannot be taken while it is down.
	router.Method(http.MethodPatch, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/meta", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))

	// direct-tcpip tunnel (ADR 0024 §3). Reuses sandboxAgentProxyHandler and
	// onUse unchanged: the handler already generically forwards any
	// /api/project/.../sandboxes/{sandboxId}/* suffix to the sandbox-agent,
	// and sandboxAgentRequiredScope below already knows this path needs
	// tcp:connect — this registration is the entire start-on-use inheritance
	// the ADR asks for, achieved by literally reusing onUse rather than
	// reimplementing it.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/tcp/attach", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
	// Its datagram twin (ADR 0109 §4), registered the same way for the same
	// reasons, and gated by udp:connect below.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/udp/attach", service.onUse(needsSandbox, service.sandboxAgentProxyHandler()))
}

func (s *sandboxService) sandboxHTTPProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		if err := authorizeProxyScope(r, ScopeSandboxHTTP); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		port, ok := parsePort(chi.URLParam(r, "port"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		dial, err := s.runtime.SandboxDialer(r.Context(), chi.URLParam(r, "sandboxId"), port)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(mapRuntimeError(err)))
			return
		}
		path := ""
		if suffix := chi.URLParam(r, "*"); suffix != "" {
			path = "/" + suffix
		}
		sandboxProxy(dial, sandboxruntime.HTTPURL(port, path), "").ServeHTTP(w, r)
	})
}

func (s *sandboxService) sandboxAgentProxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.authorize(chi.URLParam(r, "projectId"), chi.URLParam(r, "poolId")); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		if err := authorizeProxyScope(r, sandboxAgentRequiredScope(r)); err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(err))
			return
		}
		s.forwardToSandboxAgent(w, r)
	})
}

// forwardToSandboxAgent sends an authorized request on to the sandbox's agent,
// at the same path below the sandbox, carrying the sandbox-agent token the
// control plane forwarded. The agent checks that token itself: what the pool
// checked authorizes the hop to the pool, never what the sandbox serves.
func (s *sandboxService) forwardToSandboxAgent(w http.ResponseWriter, r *http.Request) {
	downstreamAuth := strings.TrimSpace(r.Header.Get(sandboxAgentAuthorizationHeader))
	if downstreamAuth == "" {
		http.Error(w, "sandbox-agent authorization is required", http.StatusUnauthorized)
		return
	}
	dial, err := s.runtime.SandboxDialer(r.Context(), chi.URLParam(r, "sandboxId"), sandboxruntime.SandboxAgentPort)
	if err != nil {
		http.Error(w, err.Error(), statusCodeForGitError(mapRuntimeError(err)))
		return
	}
	target := sandboxruntime.HTTPURL(sandboxruntime.SandboxAgentPort, sandboxAgentPath(
		chi.URLParam(r, "projectId"),
		chi.URLParam(r, "sandboxId"),
		strings.TrimPrefix(r.URL.Path, fmt.Sprintf(
			"/api/project/%s/pool/%s/sandboxes/%s",
			chi.URLParam(r, "projectId"),
			chi.URLParam(r, "poolId"),
			chi.URLParam(r, "sandboxId"),
		)),
	))
	sandboxProxy(dial, target, downstreamAuth).ServeHTTP(w, r)
}

func authorizeProxyScope(r *http.Request, scope string) error {
	claims, ok := SignedTokenClaimsFromContext(r.Context())
	if !ok {
		return newStatusError(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	}
	if scope != "" && !claims.HasScope(scope) {
		return newStatusError(http.StatusForbidden, http.StatusText(http.StatusForbidden))
	}
	return nil
}

func sandboxAgentRequiredScope(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/harness-hooks") {
		if r.Method == http.MethodGet {
			return ScopeExecRead
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/tcp/attach") {
		if r.Method == http.MethodGet {
			return ScopeTCPConnect
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/udp/attach") {
		if r.Method == http.MethodGet {
			return ScopeUDPConnect
		}
		return ""
	}
	if strings.HasSuffix(r.URL.Path, "/meta") {
		if r.Method == http.MethodPatch {
			return ScopeExecWrite
		}
		return ""
	}
	if strings.HasSuffix(r.URL.Path, "/tools") {
		if r.Method == http.MethodGet {
			return ScopeExecRead
		}
		return ""
	}
	if strings.Contains(r.URL.Path, "/services") {
		switch r.Method {
		case http.MethodGet:
			return ScopeExecRead
		case http.MethodPost:
			return ScopeExecWrite
		default:
			return ""
		}
	}
	if strings.Contains(r.URL.Path, "/execs") {
		if strings.HasSuffix(r.URL.Path, "/attach") {
			return ScopeExecWrite
		}
		// A wait is a POST only because it carries a body; it reads (ADR 0137).
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wait") {
			return ScopeExecRead
		}
		switch r.Method {
		case http.MethodGet:
			return ScopeExecRead
		case http.MethodPost, http.MethodDelete:
			return ScopeExecWrite
		default:
			return ""
		}
	}
	// Judging is its own authority and shares none: a token that may ask the
	// judge may neither read nor write anything in the sandbox (ADR 26-09-22-838 §2).
	// Below the exec block, as the sandbox agent has it: an exec called
	// "judge" is an exec, and the two tables answering differently is how one
	// hop ends up checking a scope the other does not.
	if strings.HasSuffix(r.URL.Path, "/judge") || strings.HasSuffix(r.URL.Path, "/judge/policy") || strings.HasSuffix(r.URL.Path, "/judge/proposal") {
		if r.Method == http.MethodPost {
			return ScopeJudgeRun
		}
		return ""
	}
	return ""
}

func parsePort(value string) (int, bool) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

func sandboxAgentPath(projectID, sandboxID, suffix string) string {
	if suffix == "" {
		suffix = "/"
	}
	return fmt.Sprintf(
		"/api/projects/%s/sandboxes/%s%s",
		url.PathEscape(projectID),
		url.PathEscape(sandboxID),
		suffix,
	)
}

// sandboxProxy forwards to target over dial, the connection the runtime
// supplies (ADR 0126 §5); target names no address, only the path and the Host
// the sandbox sees. An upgrade — an exec attach, a tunnel — is forwarded on
// the same connection.
func sandboxProxy(dial sandboxruntime.Dialer, target *url.URL, downstreamAuth string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: dial.Transport(),
		Rewrite: func(req *httputil.ProxyRequest) {
			rawQuery := req.In.URL.RawQuery
			req.SetURL(target)
			req.Out.URL.Path = target.Path
			req.Out.URL.RawPath = ""
			req.Out.URL.RawQuery = rawQuery
			req.Out.Host = target.Host
			req.Out.Header.Del(sandboxAgentAuthorizationHeader)
			if strings.TrimSpace(downstreamAuth) != "" {
				req.Out.Header.Set("Authorization", downstreamAuth)
			}
			req.SetXForwarded()
		},
		ErrorHandler: sandboxProxyError,
	}
}

// sandboxProxyError answers a request the sandbox could not be asked, as the
// default handler does, except for a request whose client has already gone.
// The control plane cancels its request here whenever its own client leaves, so
// that one is not a failure, and logging it would print a line for every
// closed terminal and abandoned fetch.
func sandboxProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	log.Printf("http: proxy error: %v", err)
	w.WriteHeader(http.StatusBadGateway)
}
