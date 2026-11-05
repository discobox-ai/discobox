package server

import (
	"context"
	"net/http"
	"strings"

	workerapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	"github.com/discobox-ai/discobox/pool-agent/createprotocol"
	"github.com/go-chi/chi/v5"
)

// The raw WebSocket adapter owns the upgrade; the generated scaffold retains
// the canonical route and authorization contract.
func (s *sandboxService) PoolDiscoverCreateProtocol(context.Context, workerapi.PoolDiscoverCreateProtocolParams) error {
	return newStatusError(http.StatusUpgradeRequired, "create discovery requires a WebSocket")
}

func registerCreateProtocol(router chi.Router, identity Identity, secured http.Handler) {
	path := createprotocol.Path(identity.ProjectID, identity.PoolID)
	router.Get(path, func(w http.ResponseWriter, r *http.Request) {
		claims, ok := SignedTokenClaimsFromContext(r.Context())
		if !ok || !claims.HasScope(ScopeSandboxWrite) {
			http.Error(w, "create discovery requires sandbox:write", http.StatusForbidden)
			return
		}
		// The stream is bound to this router and exposes only create. Every inner
		// request is authenticated again, including its sandbox identity and scope.
		createPath := strings.TrimSuffix(path, "/create-protocol") + "/sandboxes"
		createprotocol.Serve(w, r, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.EscapedPath()
			create := r.Method == http.MethodPost && path == createPath
			read := r.Method == http.MethodGet && strings.HasPrefix(path, createPath+"/") && !strings.Contains(strings.TrimPrefix(path, createPath+"/"), "/")
			if !create && !read {
				http.NotFound(w, r)
				return
			}
			secured.ServeHTTP(w, r)
		}))
	})
}
