package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

var sandboxTCPProxyScopes = []string{poolagentauth.ScopeTCPConnect}

// registerSandboxTCPRoutes exposes the sandbox-agent's TCP tunnel at the
// control-plane edge, so a client that is not speaking SSH can open the same
// direct-tcpip byte pipe `ssh -L` gets (ADR 0024 §3). The SSH ingress reaches
// the tunnel in-process through the same lease chain; this is the route for
// everything else, `discobox proxy` first among them.
func registerSandboxTCPRoutes(router chi.Router, service services.SandboxService) {
	router.Method(http.MethodGet, "/api/projects/{projectId}/sandboxes/{sandboxId}/tcp/attach", sandboxTCPProxyHandler(service))
}

// sandboxTCPProxyHandler forwards the upgrade to the sandbox-agent, which dials
// host:port from inside the sandbox's network namespace and speaks
// execstream/frame over the websocket. Everything past the handshake is the
// tunnel's own framing, so the server owns only project authorization, scope
// selection, and lease injection — the same division the other hand-wired
// sandbox proxies keep.
func sandboxTCPProxyHandler(service services.SandboxService) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox service is not configured")
			return
		}
		host := strings.TrimSpace(r.URL.Query().Get("host"))
		if _, ok := parseSandboxHTTPPort(r.URL.Query().Get("port")); host == "" || !ok {
			writeSandboxAgentProxyError(w, http.StatusBadRequest, "host and a valid port query parameter are required")
			return
		}

		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		// Fail fast rather than wait for a sandbox that is still coming up
		// (ADR 0039 tier 3): a forwarded connection is a browser or a client
		// library on the other end, and one that hangs for minutes is worse
		// than one that is refused now and retried when the user reloads.
		lease, sandboxModel, err := service.AcquireSandboxHTTPClient(r.Context(), projectID, sandboxID, sandboxTCPProxyScopes)
		if err != nil {
			writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
			return
		}
		if lease == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox HTTP client is not available")
			return
		}
		defer lease.Release()
		if sandboxModel == nil || strings.TrimSpace(sandboxModel.PoolID) == "" {
			writeSandboxAgentProxyError(w, http.StatusConflict, "sandbox pool is not assigned")
			return
		}

		target, err := sandboxTCPProxyTargetURL(lease.BaseURL, sandboxModel.ProjectID, strings.TrimSpace(sandboxModel.PoolID), sandboxModel.ID)
		if err != nil {
			writeSandboxAgentProxyError(w, http.StatusInternalServerError, err.Error())
			return
		}
		proxy := sandboxPoolReverseProxy(target, lease)
		proxy.ServeHTTP(w, r)
	})
}

// sandboxTCPProxyTargetURL builds the pool-agent URL for the tunnel. It carries
// no query: sandboxPoolReverseProxy forwards the incoming request's raw query
// through, which is the host and port this handler just validated.
func sandboxTCPProxyTargetURL(baseURL, projectID, poolID, sandboxID string) (*url.URL, error) {
	return sandboxagentclient.TargetURL(baseURL, projectID, poolID, sandboxID, "/tcp/attach")
}
