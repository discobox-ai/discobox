package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	poolapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// registerSandboxAgentTerminalRoutes forwards the sandbox agent's exec and
// service API, and says of each route what a request on it is for
// (ADR 26-10-12-711 §1). This is the one table of which of them starts a
// stopped sandbox and which may repair one: the pool agent is told the answer
// on the request's token and holds no table of its own. The use is fixed where
// the route is registered, not read off the path, so an exec or a service whose
// id is spelled like an operation is still what its route says it is.
//
//   - observe: a terminal's screen or a wait on it, and the sandbox's own
//     audit data. Reading these must not start what an idle stop put down
//     (ADR 0108, ADR 0137 §2), and what polls them wants an answer now.
//   - inspect: listing execs, services or tools, and reading one exec or
//     service. It starts nothing either, and waits out a boot something else
//     began: a client that has just created or started a sandbox reads it
//     next.
//   - read: the reads with no last value — logs, events, resource samples,
//     and a file's stat, a directory's listing and a file's content
//     (ADR 26-10-22-499 §3). They start a stopped sandbox and repair nothing.
//   - action: everything that changes something or acts in the sandbox —
//     creating, starting, attaching to, typing into or deleting an exec,
//     starting, stopping or restarting a service, and writing, creating,
//     removing, moving or copying a file.
//
// The inspects and the screen are the reads a sandbox's last inventory
// answers while it is not running (sandboxInventoryRead, ADR 26-10-11-657 §4).
func registerSandboxAgentTerminalRoutes(router chi.Router, service services.SandboxService) {
	const base = "/api/projects/{projectId}/sandboxes/{sandboxId}"
	observe, inspect, read, action := services.SandboxObserve, services.SandboxInspect, services.SandboxRead, services.SandboxAction
	attach := action("exec attach")
	attach.Attach = true
	createExec := action("exec create")
	for _, route := range []struct {
		method, path string
		use          services.SandboxUse
	}{
		{http.MethodGet, "/harness-hooks", observe},
		{http.MethodGet, "/exec-events", observe},
		{http.MethodGet, "/execs", inspect},
		{http.MethodPost, "/execs", createExec},
		{http.MethodGet, "/execs/{execId}", inspect},
		{http.MethodDelete, "/execs/{execId}", action("exec delete")},
		{http.MethodGet, "/execs/{execId}/logs", read},
		{http.MethodGet, "/execs/{execId}/screen", observe},
		{http.MethodPost, "/execs/{execId}/input", action("exec input")},
		// A wait is a POST only because it carries a body; it reads (ADR 0137).
		{http.MethodPost, "/execs/{execId}/wait", observe},
		// An attach is a GET when it is a websocket upgrade.
		{http.MethodPost, "/execs/{execId}/attach", attach},
		{http.MethodGet, "/execs/{execId}/attach", attach},
		{http.MethodPost, "/execs/{execId}/start", action("exec start")},
		{http.MethodGet, "/execs/{execId}/events", read},
		{http.MethodGet, "/execs/{execId}/resources", read},
		{http.MethodGet, "/execs/{execId}/resources/history", read},
		{http.MethodGet, "/execs/{execId}/resources/stream", read},
		{http.MethodGet, "/services", inspect},
		{http.MethodGet, "/tools", inspect},
		{http.MethodGet, "/services/{serviceId}", inspect},
		{http.MethodGet, "/services/{serviceId}/logs", read},
		{http.MethodPost, "/services/{serviceId}/start", action("service start")},
		{http.MethodPost, "/services/{serviceId}/stop", action("service stop")},
		{http.MethodPost, "/services/{serviceId}/restart", action("service restart")},
		{http.MethodGet, "/files/stat", read},
		{http.MethodGet, "/files/list", read},
		{http.MethodGet, "/files/content", read},
		{http.MethodPut, "/files/content", action("file write")},
		{http.MethodPost, "/files/mkdir", action("file mkdir")},
		{http.MethodPost, "/files/remove", action("file remove")},
		{http.MethodPost, "/files/move", action("file move")},
		{http.MethodPost, "/files/copy", action("file copy")},
	} {
		router.Method(route.method, base+route.path, sandboxAgentTerminalProxyHandler(service, route.use, route.use == createExec))
	}
}

// createsExec marks the exec create route, which is the one that takes this
// server's own `wait` option.
func sandboxAgentTerminalProxyHandler(service services.SandboxService, use services.SandboxUse, createsExec bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if service == nil {
			writeSandboxAgentProxyError(w, http.StatusServiceUnavailable, "sandbox service is not configured")
			return
		}
		projectID := chi.URLParam(r, "projectId")
		sandboxID := chi.URLParam(r, "sandboxId")
		scopes := sandboxAgentTerminalProxyScopes(r)
		if len(scopes) == 0 {
			writeSandboxAgentProxyError(w, http.StatusMethodNotAllowed, "unsupported sandbox agent-terminal operation")
			return
		}

		// A read the sandbox's inventory answers never starts the sandbox
		// (ADR 26-10-11-657 §4): its pool agent refuses it for a sandbox that
		// is not running, and that refusal is answered from the inventory
		// below. So is a stopped sandbox whose pool cannot be asked at all.
		acquire := service.AcquireSandboxHTTPClient
		inventoryRead, storedRead := sandboxInventoryRead(r)
		if storedRead {
			acquire = service.AcquireRunningSandboxHTTPClient
		}
		lease, sandboxModel, err := acquire(r.Context(), projectID, sandboxID, scopes, use)
		if storedRead && errors.Is(err, sandbox.ErrNotRunning) {
			serveSandboxInventoryRead(w, r, service, projectID, sandboxID, scopes, inventoryRead)
			return
		}
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

		target, err := sandboxAgentTerminalProxyTargetURL(lease.BaseURL, projectID, sandboxModel.ProjectID, strings.TrimSpace(sandboxModel.PoolID), sandboxID, sandboxModel.ID, r.URL.Path)
		if err != nil {
			writeSandboxAgentProxyError(w, http.StatusInternalServerError, err.Error())
			return
		}
		proxy := sandboxPoolReverseProxy(target, lease)
		if storedRead {
			// The pool agent found the sandbox not running — stopped, or not
			// yet done starting — and refused rather than start it, saying
			// so in a header.
			proxy.ModifyResponse = func(resp *http.Response) error {
				if resp.StatusCode == http.StatusConflict && resp.Header.Get(poolapimodel.SandboxNotRunningHeader) != "" {
					return errSandboxNotRunning
				}
				return nil
			}
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
				if errors.Is(err, errSandboxNotRunning) {
					serveSandboxInventoryRead(w, r, service, projectID, sandboxID, scopes, inventoryRead)
					return
				}
				sandboxPoolProxyError(w, r, err)
			}
		}
		if createsExec && r.URL.Query().Get("wait") == "ready" {
			// `wait=ready` asked an exec create to wait for its sandbox,
			// before every action did. It was this server's option and never
			// the sandbox agent's, so it is still taken here and not
			// forwarded. Any other value is not this server's to accept: it
			// goes on as it came, for the sandbox agent to refuse.
			query := r.URL.Query()
			query.Del("wait")
			r.URL.RawQuery = query.Encode()
		}
		proxy.ServeHTTP(w, r)
	})
}

var errSandboxNotRunning = errors.New("the pool agent refused a read of a sandbox that is not running")

// sandboxInventoryRead reports which of the reads a sandbox's inventory
// answers this request is, if any: listing execs, services or tools, reading
// one exec or service, or a terminal's screen (ADR 26-10-11-657 §4). Every
// other route — the actions, and the reads with no last value to answer with
// — is proxied as it always was.
func sandboxInventoryRead(r *http.Request) (services.InventoryRead, bool) {
	if r.Method != http.MethodGet {
		return services.InventoryRead{}, false
	}
	// chi has matched the route, so the ids are read from it rather than cut
	// from the path: an exec named "screen" is still an exec.
	execID, serviceID := chi.URLParam(r, "execId"), chi.URLParam(r, "serviceId")
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	switch {
	case strings.HasSuffix(pattern, "/sandboxes/{sandboxId}/execs"):
		return services.InventoryRead{Part: services.InventoryExecs}, true
	case strings.HasSuffix(pattern, "/sandboxes/{sandboxId}/services"):
		return services.InventoryRead{Part: services.InventoryServices}, true
	case strings.HasSuffix(pattern, "/sandboxes/{sandboxId}/tools"):
		return services.InventoryRead{Part: services.InventoryTools}, true
	case strings.HasSuffix(pattern, "/execs/{execId}"):
		return services.InventoryRead{Part: services.InventoryExec, ID: execID}, true
	case strings.HasSuffix(pattern, "/execs/{execId}/screen"):
		return services.InventoryRead{Part: services.InventoryScreen, ID: execID}, true
	case strings.HasSuffix(pattern, "/services/{serviceId}"):
		return services.InventoryRead{Part: services.InventoryService, ID: serviceID}, true
	}
	return services.InventoryRead{}, false
}

// serveSandboxInventoryRead answers a read from the sandbox's last inventory.
func serveSandboxInventoryRead(w http.ResponseWriter, r *http.Request, service services.SandboxService, projectID, sandboxID string, scopes []string, read services.InventoryRead) {
	body, err := service.ReadSandboxInventory(r.Context(), projectID, sandboxID, scopes, read)
	if err != nil {
		writeSandboxAgentProxyError(w, statusCodeForProxyError(err), err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeSandboxAgentProxyError(w http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		return
	}
}

func sandboxAgentTerminalProxyScopes(r *http.Request) []string {
	// The files API, told by the route chi matched rather than by a part of
	// the path: an exec or a service named "files" is not one. Its scopes are
	// its own, so a role may read a discobox's files and run nothing in it
	// (ADR 26-10-22-499 §3).
	if pattern := chi.RouteContext(r.Context()).RoutePattern(); strings.Contains(pattern, "/sandboxes/{sandboxId}/files/") {
		if r.Method == http.MethodGet {
			return []string{poolagentauth.ScopeFilesRead}
		}
		return []string{poolagentauth.ScopeFilesWrite}
	}
	// The events of every exec, read across them: the exec event trail.
	if strings.HasSuffix(r.URL.Path, "/exec-events") {
		if r.Method == http.MethodGet {
			return []string{poolagentauth.ScopeExecRead}
		}
		return nil
	}
	if strings.Contains(r.URL.Path, "/harness-hooks") {
		if r.Method == http.MethodGet {
			return []string{poolagentauth.ScopeExecRead}
		}
		return nil
	}
	// A service is an exec (ADR 0070), so the exec scopes are what gate it:
	// reading one reads an exec record and its transcript, and starting,
	// stopping or restarting one is creating or ending an exec.
	// Listing tools reads declaration files, and what it says is how to start
	// an exec, so it is read like one (ADR 0125 §7).
	if strings.HasSuffix(r.URL.Path, "/tools") && r.Method == http.MethodGet {
		return []string{poolagentauth.ScopeExecRead}
	}
	if strings.Contains(r.URL.Path, "/services") {
		switch r.Method {
		case http.MethodGet:
			return []string{poolagentauth.ScopeExecRead}
		case http.MethodPost:
			return []string{poolagentauth.ScopeExecWrite}
		default:
			return nil
		}
	}
	if strings.Contains(r.URL.Path, "/execs") {
		if strings.HasSuffix(r.URL.Path, "/attach") {
			return []string{poolagentauth.ScopeExecWrite, poolagentauth.ScopeExecRead}
		}
		// A wait is a POST only because it carries a body; it reads (ADR 0137).
		if strings.HasSuffix(r.URL.Path, "/wait") {
			return []string{poolagentauth.ScopeExecRead}
		}
		switch r.Method {
		case http.MethodGet:
			return []string{poolagentauth.ScopeExecRead}
		case http.MethodPost, http.MethodDelete:
			return []string{poolagentauth.ScopeExecWrite}
		default:
			return nil
		}
	}
	return nil
}

func sandboxAgentTerminalProxyTargetURL(baseURL, routeProjectID, projectID, poolID, routeSandboxID, sandboxID, path string) (*url.URL, error) {
	if !strings.HasPrefix(path, "/api/projects/") {
		return nil, fmt.Errorf("sandbox agent-terminal proxy path %q must start with /api/projects/", path)
	}
	suffix := strings.TrimPrefix(path, "/api/projects/"+routeProjectID+"/sandboxes/"+routeSandboxID)
	if suffix == path {
		return nil, fmt.Errorf("sandbox agent-terminal proxy path identity does not match route")
	}
	return sandboxagentclient.TargetURL(baseURL, projectID, poolID, sandboxID, suffix)
}
