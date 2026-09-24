package server

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pool-agent/internalhttp"
	"github.com/go-chi/chi/v5"
)

const sandboxAgentAuthorizationHeader = "X-Discobox-Sandbox-Agent-Authorization"

func registerSandboxProxyRoutes(router chi.Router, service *sandboxService) {
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/http/{port}", service.autoStart(failFast, service.sandboxHTTPProxyHandler()))
	router.Handle("/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/http/{port}/*", service.autoStart(failFast, service.sandboxHTTPProxyHandler()))

	// The sandbox's own audit data is read only from a running sandbox and never
	// starts one (requireRunning).
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/harness-hooks", service.requireRunning(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/exec-events", service.requireRunning(service.sandboxAgentProxyHandler()))
	// Reading a terminal's screen, or waiting on it, is not use (ADR 0137 §2):
	// an orchestrator polling its workers must not start one idle stop put
	// down, or keep one from ever stopping. Typing into it is use, and does.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/screen", service.requireRunning(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/wait", service.requireRunning(service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodDelete, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/logs", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/input", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/attach", service.autoStart(awaitContainer, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/attach", service.autoStart(awaitContainer, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/start", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/events", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources/history", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/execs/{execId}/resources/stream", service.autoStart(failFast, service.sandboxAgentProxyHandler()))

	// The judge answers from a discobox that is meant to be ready, but a pool
	// restart leaves it stopped, and the first ask is what starts it: failFast
	// so a judge that cannot come up refuses now rather than holding the
	// request that is waiting on it (ADR 0149 §1).
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/judge", service.autoStart(failFast, service.sandboxAgentProxyHandler()))

	// A service is an exec (ADR 0070), reached the same way and gated by the
	// same scopes. It needs its own registrations because this router names
	// every sandbox-agent path it forwards rather than forwarding a prefix.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/tools", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/logs", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/start", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/stop", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	router.Method(http.MethodPost, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/services/{serviceId}/restart", service.autoStart(failFast, service.sandboxAgentProxyHandler()))

	// The meta file (ADR 0136). Writing it starts a stopped sandbox like any
	// other use: the sandbox is where its description and tags live, so a
	// change cannot be taken while it is down.
	router.Method(http.MethodPatch, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/meta", service.autoStart(failFast, service.sandboxAgentProxyHandler()))

	// direct-tcpip tunnel (ADR 0024 §3). Reuses sandboxAgentProxyHandler and
	// autoStart unchanged: the handler already generically forwards any
	// /api/project/.../sandboxes/{sandboxId}/* suffix to the sandbox-agent,
	// and sandboxAgentRequiredScope below already knows this path needs
	// tcp:connect — this registration is the entire auto-start inheritance
	// the ADR asks for, achieved by literally reusing autoStart rather than
	// reimplementing it.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/tcp/attach", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
	// Its datagram twin (ADR 0109 §4), registered the same way for the same
	// reasons, and gated by udp:connect below.
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/sandboxes/{sandboxId}/udp/attach", service.autoStart(failFast, service.sandboxAgentProxyHandler()))
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
		base, err := s.runtime.HTTPBaseURL(r.Context(), chi.URLParam(r, "sandboxId"), port)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(mapRuntimeError(err)))
			return
		}
		suffix := chi.URLParam(r, "*")
		target := *base
		if suffix != "" {
			target.Path = "/" + suffix
		}
		sandboxProxy(&target, "").ServeHTTP(w, r)
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
		downstreamAuth := strings.TrimSpace(r.Header.Get(sandboxAgentAuthorizationHeader))
		if downstreamAuth == "" {
			http.Error(w, "sandbox-agent authorization is required", http.StatusUnauthorized)
			return
		}
		base, err := s.runtime.HTTPBaseURL(r.Context(), chi.URLParam(r, "sandboxId"), 3003)
		if err != nil {
			http.Error(w, err.Error(), statusCodeForGitError(mapRuntimeError(err)))
			return
		}
		target := *base
		target.Path = sandboxAgentPath(
			chi.URLParam(r, "projectId"),
			chi.URLParam(r, "sandboxId"),
			strings.TrimPrefix(r.URL.Path, fmt.Sprintf(
				"/api/project/%s/pool/%s/sandboxes/%s",
				chi.URLParam(r, "projectId"),
				chi.URLParam(r, "poolId"),
				chi.URLParam(r, "sandboxId"),
			)),
		)
		sandboxProxy(&target, downstreamAuth).ServeHTTP(w, r)
	})
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
	// judge may neither read nor write anything in the sandbox (ADR 0149 §2).
	// Below the exec block, as the sandbox agent has it: an exec called
	// "judge" is an exec, and the two tables answering differently is how one
	// hop ends up checking a scope the other does not.
	if strings.HasSuffix(r.URL.Path, "/judge") {
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

func sandboxProxy(target *url.URL, downstreamAuth string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// Not the default transport: it honors HTTP_PROXY, and a pool running
		// inside a Discobox sandbox has proxy env injected for its egress.
		// This request goes to a sandbox on the pool's own network, so it must
		// never leave through the egress proxy -- and must not depend on
		// NO_PROXY being right for that.
		Transport: internalhttp.Transport(),
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
