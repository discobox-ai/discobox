package server

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// PprofTarget is where one of the pool's processes serves its pprof
// endpoints: its fixed loopback address, and the file holding the token that
// listener requires, which the process rewrites on every start.
type PprofTarget struct {
	Address   string
	TokenFile string
}

// pprofProfiles is what the relay fetches: the standard set's profiles, by
// the name net/http/pprof serves each under. The index, cmdline and symbol
// lookup are not profiles, and are not relayed.
var pprofProfiles = map[string]bool{
	"profile":      true,
	"trace":        true,
	"heap":         true,
	"allocs":       true,
	"goroutine":    true,
	"block":        true,
	"mutex":        true,
	"threadcreate": true,
}

// registerPprofRoutes wires the relay of a pool process's profile (ADR
// 26-11-24-418 §3), hand-wired because a profile is opaque bytes that answer
// only after the seconds they sample. targets names each process the pool
// profiles; none is a pool with profiling off.
//
// The pprof handlers themselves are never on this router: each process serves
// them on its own loopback listener, and this relays one request there, for
// the agent's own profile as for the proxy's and the mediator's.
func registerPprofRoutes(router chi.Router, identity Identity, targets map[string]PprofTarget) {
	relay := &pprofRelay{
		identity: identity,
		targets:  targets,
		// Loopback, with no proxy: the agent's environment may name one, and
		// a profile must never leave the pool. No timeout but the request's
		// own, since a CPU profile or trace answers after its seconds.
		client: &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}},
	}
	router.Method(http.MethodGet, "/api/project/{projectId}/pool/{poolId}/pprof/{process}/{profile}", relay)
}

type pprofRelay struct {
	identity Identity
	targets  map[string]PprofTarget
	client   *http.Client
}

func (p *pprofRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "projectId") != p.identity.ProjectID || chi.URLParam(r, "poolId") != p.identity.PoolID {
		writeProblem(w, http.StatusNotFound, "pool route not found")
		return
	}
	claims, ok := SignedTokenClaimsFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
		return
	}
	if !claims.HasScope(ScopePoolPprof) {
		writeProblem(w, http.StatusForbidden, http.StatusText(http.StatusForbidden))
		return
	}
	// Refused as problem+json rather than the router's text/plain 404, which
	// is what an agent too old for the route answers: the control plane tells
	// the two apart by it.
	if len(p.targets) == 0 {
		writeProblem(w, http.StatusNotFound, "profiling is off for pool "+p.identity.PoolID)
		return
	}
	process, profile := chi.URLParam(r, "process"), chi.URLParam(r, "profile")
	target, ok := p.targets[process]
	if !ok {
		writeProblem(w, http.StatusNotFound, fmt.Sprintf("pool %s profiles no process %q", p.identity.PoolID, process))
		return
	}
	if !pprofProfiles[profile] {
		writeProblem(w, http.StatusNotFound, fmt.Sprintf("no profile %q; one of profile, trace, heap, allocs, goroutine, block, mutex or threadcreate", profile))
		return
	}
	query := url.Values{}
	if raw := strings.TrimSpace(r.URL.Query().Get("seconds")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			writeProblem(w, http.StatusBadRequest, "seconds must be a positive whole number")
			return
		}
		query.Set("seconds", strconv.Itoa(seconds))
	}

	// Read on every request: the process writes a new token each time it
	// starts. A missing file is a process that is not serving its listener.
	token, err := os.ReadFile(target.TokenFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeProblem(w, http.StatusBadGateway, fmt.Sprintf("the pool's %s is not serving profiles: it has written no token, so it is not running or started before profiling was turned on; the BuildKit mediator is not restarted with the pool's setting, and takes it at its next start", process))
			return
		}
		writeProblem(w, http.StatusBadGateway, fmt.Sprintf("read the pool's %s profiling token: %v", process, err))
		return
	}
	upstream := url.URL{Scheme: "http", Host: target.Address, Path: "/debug/pprof/" + profile, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.String(), nil)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.SetBasicAuth("pprof", strings.TrimSpace(string(token)))
	resp, err := p.client.Do(req)
	if err != nil {
		writeProblem(w, http.StatusBadGateway, fmt.Sprintf("the pool's %s does not answer on %s: %v", process, target.Address, err))
		return
	}
	defer resp.Body.Close()
	// Unchanged: the status, the type, and the bytes.
	for _, name := range []string{"Content-Type", "Content-Disposition", "X-Content-Type-Options"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// A profile cut short must not end as if whole.
		panic(http.ErrAbortHandler)
	}
}
